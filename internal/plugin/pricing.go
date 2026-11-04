package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// openRouterModelsURL is the only automatic price source: public, keyless, and
// the same one Oh My CPA prices requests with.
const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// priceRates is one rate card in USD per 1M tokens. That unit equals micro-USD
// per token, so a request cost in micro-USD is just the sum of rate x tokens.
type priceRates struct {
	PromptPer1M     float64 `json:"prompt_price_per_1m"`
	CompletionPer1M float64 `json:"completion_price_per_1m"`
	CacheReadPer1M  float64 `json:"cache_read_price_per_1m"`
	CacheWritePer1M float64 `json:"cache_write_price_per_1m"`
}

// priceBookState is the cached price book persisted with the ledger.
type priceBookState struct {
	Source    string                `json:"source"`
	FetchedAt *time.Time            `json:"fetched_at,omitempty"`
	Models    map[string]priceRates `json:"models,omitempty"`
	// LastError keeps the last refresh failure so the panel can show stale data
	// instead of silently pretending the book is current.
	LastError string `json:"last_error,omitempty"`
}

func (b *priceBookState) count() int {
	if b == nil {
		return 0
	}
	return len(b.Models)
}

func (b *priceBookState) summary() map[string]any {
	if b == nil {
		return map[string]any{"source": "", "models": 0, "stale": true}
	}
	view := map[string]any{"source": b.Source, "models": len(b.Models), "last_error": b.LastError}
	if b.FetchedAt != nil {
		view["fetched_at"] = b.FetchedAt
		view["age_hours"] = math.Round(time.Since(*b.FetchedAt).Hours()*10) / 10
	} else {
		view["stale"] = true
	}
	return view
}

// tokens is one request's billable buckets. Input includes cached tokens, which
// is how CPA reports usage.
type tokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// quoteMicro prices one request in micro-USD using exact rational arithmetic, so
// repeated cheap requests do not drift the way float accumulation would.
func quoteMicro(rates priceRates, request tokens, multiplier float64) (int64, error) {
	if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		multiplier = 1
	}
	uncached := clampTokens(request.Input)
	uncached -= minInt64(uncached, clampTokens(request.CacheRead))
	uncached -= minInt64(uncached, clampTokens(request.CacheWrite))

	total := new(big.Rat)
	for _, bucket := range []struct {
		tokens int64
		rate   float64
	}{
		{uncached, rates.PromptPer1M},
		{clampTokens(request.Output), rates.CompletionPer1M},
		{clampTokens(request.CacheRead), rates.CacheReadPer1M},
		{clampTokens(request.CacheWrite), rates.CacheWritePer1M},
	} {
		if bucket.tokens == 0 || bucket.rate == 0 {
			if bucket.rate < 0 {
				return 0, errors.New("negative price rate")
			}
			continue
		}
		rate, errRate := exactDecimal(bucket.rate)
		if errRate != nil {
			return 0, errRate
		}
		total.Add(total, rate.Mul(rate, new(big.Rat).SetInt64(bucket.tokens)))
	}
	scale, _ := exactDecimal(multiplier)
	if multiplier != 1 {
		total.Mul(total, scale)
	}
	micro, errRound := roundRat(total)
	if errRound != nil {
		return 0, errRound
	}
	return micro, nil
}

func clampTokens(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func exactDecimal(value float64) (*big.Rat, error) {
	rat, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	if !ok {
		return nil, fmt.Errorf("invalid decimal rate %v", value)
	}
	return rat, nil
}

// roundRat rounds half away from zero, matching the price book's own rounding.
func roundRat(value *big.Rat) (int64, error) {
	rounded, remainder := new(big.Int), new(big.Int)
	rounded.QuoRem(value.Num(), value.Denom(), remainder)
	if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(new(big.Int).Abs(value.Denom())) >= 0 {
		if value.Sign() < 0 {
			rounded.Sub(rounded, big.NewInt(1))
		} else {
			rounded.Add(rounded, big.NewInt(1))
		}
	}
	if !rounded.IsInt64() {
		return 0, errors.New("request cost exceeds micro-USD range")
	}
	return rounded.Int64(), nil
}

var (
	dateSuffix    = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
	versionSuffix = regexp.MustCompile(`-v\d+(?:\.\d+)*$`)
)

// lookup resolves a CPA model name against the price book. The candidates go
// from most to least precise: exact id, dash/dot swap, bare name without the
// vendor, then the date/version-stripped forms.
func (b *priceBookState) lookup(model string, overrides map[string]priceRates) (priceRates, string, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return priceRates{}, "", false
	}
	if rates, ok := overrides[name]; ok {
		return rates, "override", true
	}
	if b == nil || len(b.Models) == 0 {
		return priceRates{}, "", false
	}
	for _, candidate := range modelCandidates(name) {
		if rates, ok := b.Models[candidate]; ok {
			return rates, "openrouter", true
		}
	}
	return priceRates{}, "", false
}

func modelCandidates(name string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	bare := name
	if index := strings.LastIndex(bare, "/"); index >= 0 {
		bare = bare[index+1:]
	}
	add(name)
	add(strings.ReplaceAll(name, "-", "."))
	add(bare)
	add(strings.ReplaceAll(bare, "-", "."))
	add(dateSuffix.ReplaceAllString(name, ""))
	add(versionSuffix.ReplaceAllString(name, ""))
	add(dateSuffix.ReplaceAllString(bare, ""))
	add(versionSuffix.ReplaceAllString(bare, ""))
	return out
}

// decodeOpenRouter reads the public model list into rate cards. OpenRouter
// quotes USD per token as strings; missing cache rates fall back to the prompt
// rate, which is what the vendor charges when it does not publish one.
func decodeOpenRouter(body []byte) (map[string]priceRates, error) {
	var envelope struct {
		Data []struct {
			ID      string                     `json:"id"`
			Pricing map[string]json.RawMessage `json:"pricing"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&envelope); errDecode != nil {
		return nil, fmt.Errorf("decode OpenRouter price list: %w", errDecode)
	}
	models := make(map[string]priceRates, len(envelope.Data))
	for _, entry := range envelope.Data {
		id := strings.ToLower(strings.TrimSpace(entry.ID))
		if id == "" || strings.Contains(id, ":") || len(id) > 256 {
			continue
		}
		prompt, okPrompt := perMillion(entry.Pricing["prompt"])
		completion, okCompletion := perMillion(entry.Pricing["completion"])
		if !okPrompt || !okCompletion {
			continue
		}
		cacheRead, okCacheRead := perMillion(entry.Pricing["input_cache_read"])
		if !okCacheRead {
			cacheRead = prompt
		}
		cacheWrite, okCacheWrite := perMillion(entry.Pricing["input_cache_write"])
		if !okCacheWrite {
			cacheWrite = prompt
		}
		models[id] = priceRates{
			PromptPer1M:     prompt,
			CompletionPer1M: completion,
			CacheReadPer1M:  cacheRead,
			CacheWritePer1M: cacheWrite,
		}
	}
	return models, nil
}

// perMillion converts a USD-per-token string into USD per 1M tokens.
func perMillion(raw json.RawMessage) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if text == "" || text == "null" {
		return 0, false
	}
	value, errParse := strconv.ParseFloat(text, 64)
	if errParse != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value * 1e6, true
}

type priceBookRefresher struct {
	mu    sync.Mutex
	state *runtimeState
	stop  chan struct{}
	once  sync.Once
}

// refreshPriceBook fetches the public price list and stores it. Failures keep
// the previous book and only record the error, so a transient outage never
// changes how requests are billed.
func refreshPriceBook(led *ledger, cfg Config, client *http.Client) error {
	if cfg.PriceSource != "openrouter" {
		return nil
	}
	ctxClient := client
	if ctxClient == nil {
		ctxClient = &http.Client{Timeout: 20 * time.Second}
	}
	request, errRequest := http.NewRequest(http.MethodGet, openRouterModelsURL, nil)
	if errRequest != nil {
		return errRequest
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", PluginID+"/"+PluginVersion)
	response, errDo := ctxClient.Do(request)
	if errDo != nil {
		recordPriceError(led, errDo)
		return errDo
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		errStatus := fmt.Errorf("openrouter price list returned HTTP %d", response.StatusCode)
		recordPriceError(led, errStatus)
		return errStatus
	}
	body, errRead := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if errRead != nil {
		recordPriceError(led, errRead)
		return errRead
	}
	models, errDecode := decodeOpenRouter(body)
	if errDecode != nil {
		recordPriceError(led, errDecode)
		return errDecode
	}
	if len(models) == 0 {
		errEmpty := errors.New("openrouter price list was empty")
		recordPriceError(led, errEmpty)
		return errEmpty
	}
	now := unixNow()
	led.storePriceBook(&priceBookState{Source: "openrouter", FetchedAt: &now, Models: models})
	led.persist()
	appendLog(fmt.Sprintf("price book refreshed: %d models", len(models)))
	return nil
}

func recordPriceError(led *ledger, err error) {
	book := led.priceBook()
	if book == nil {
		book = &priceBookState{Source: "openrouter"}
	}
	book.LastError = err.Error()
	led.storePriceBook(book)
	appendLog("price book refresh failed: " + err.Error())
}

// startPriceRefresh runs the periodic refresh in the background.
func startPriceRefresh(rt *runtimeState) *priceBookRefresher {
	refresher := &priceBookRefresher{state: rt, stop: make(chan struct{})}
	go func() {
		interval := time.Duration(rt.cfg.PriceRefreshHours * float64(time.Hour))
		if interval < 15*time.Minute {
			interval = 15 * time.Minute
		}
		// Refresh at load when the cached book is missing or older than the interval.
		book := rt.ledger.priceBook()
		if book == nil || book.FetchedAt == nil || book.count() == 0 || time.Since(*book.FetchedAt) > interval {
			_ = refreshPriceBook(rt.ledger, rt.cfg, nil)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-refresher.stop:
				return
			case <-ticker.C:
				_ = refreshPriceBook(rt.ledger, rt.cfg, nil)
			}
		}
	}()
	return refresher
}

func (r *priceBookRefresher) close() {
	if r == nil {
		return
	}
	r.once.Do(func() { close(r.stop) })
}
