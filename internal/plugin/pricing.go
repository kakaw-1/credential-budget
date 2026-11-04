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

// priceTier is a conditional rate override. A tier applies only when every
// condition it carries holds, and the tiers of one model never stack: the most
// specific match governs the whole request.
//
// Rates a tier leaves nil inherit the model's base rate, so a tier can move the
// prompt price alone without restating the rest of the card.
type priceTier struct {
	// MinPromptTokens is the smallest full input (cached tokens included) the
	// tier applies to. Zero means the tier is not gated on size.
	MinPromptTokens int64 `json:"min_prompt_tokens,omitempty"`
	// UTCStart and UTCEnd bound a billing window as HHMM in UTC, half-open and
	// wrapping past midnight when end <= start.
	UTCStart *int `json:"utc_start,omitempty"`
	UTCEnd   *int `json:"utc_end,omitempty"`

	PromptPer1M     *float64 `json:"prompt_price_per_1m,omitempty"`
	CompletionPer1M *float64 `json:"completion_price_per_1m,omitempty"`
	CacheReadPer1M  *float64 `json:"cache_read_price_per_1m,omitempty"`
	CacheWritePer1M *float64 `json:"cache_write_price_per_1m,omitempty"`
}

func (t priceTier) hasWindow() bool {
	return t.UTCStart != nil && t.UTCEnd != nil
}

func (t priceTier) hasCondition() bool {
	return t.MinPromptTokens > 0 || t.hasWindow()
}

// modelPrice is one model's base card plus its conditional overrides.
type modelPrice struct {
	Base  priceRates  `json:"base"`
	Tiers []priceTier `json:"tiers,omitempty"`
}

// UnmarshalJSON keeps state files written before tiers existed readable: those
// store the four rates directly on the model entry.
func (p *modelPrice) UnmarshalJSON(data []byte) error {
	var probe struct {
		Base  *priceRates `json:"base"`
		Tiers []priceTier `json:"tiers"`
	}
	if errUnmarshal := json.Unmarshal(data, &probe); errUnmarshal != nil {
		return errUnmarshal
	}
	if probe.Base != nil {
		p.Base = *probe.Base
		p.Tiers = probe.Tiers
		return nil
	}
	var legacy priceRates
	if errUnmarshal := json.Unmarshal(data, &legacy); errUnmarshal != nil {
		return errUnmarshal
	}
	p.Base = legacy
	p.Tiers = nil
	return nil
}

// rateCard resolves the rates that govern one request: the matching tier's
// overrides laid over the base card.
func (p modelPrice) rateCard(inputTokens int64, at time.Time) (priceRates, *priceTier) {
	rates := p.Base
	tier := selectTier(p.Tiers, inputTokens, at)
	if tier == nil {
		return rates, nil
	}
	if tier.PromptPer1M != nil {
		rates.PromptPer1M = *tier.PromptPer1M
	}
	if tier.CompletionPer1M != nil {
		rates.CompletionPer1M = *tier.CompletionPer1M
	}
	if tier.CacheReadPer1M != nil {
		rates.CacheReadPer1M = *tier.CacheReadPer1M
	}
	if tier.CacheWritePer1M != nil {
		rates.CacheWritePer1M = *tier.CacheWritePer1M
	}
	return rates, tier
}

// selectTier returns the single tier that governs a request, or nil when the
// base card applies.
//
// A tier matches when the full input reaches its threshold and, if it carries a
// window, the request timestamp falls inside it. Tiers never stack: the one with
// the larger threshold wins, and a window-only tier loses to any sized tier.
func selectTier(tiers []priceTier, inputTokens int64, at time.Time) *priceTier {
	var best *priceTier
	for index := range tiers {
		tier := &tiers[index]
		if !tier.hasCondition() {
			continue
		}
		if tier.MinPromptTokens > 0 && clampTokens(inputTokens) < tier.MinPromptTokens {
			continue
		}
		if tier.hasWindow() && !windowHolds(*tier, at) {
			continue
		}
		if best == nil || tierOutranks(*tier, *best) {
			best = tier
		}
	}
	return best
}

func tierOutranks(candidate, current priceTier) bool {
	if candidate.MinPromptTokens != current.MinPromptTokens {
		return candidate.MinPromptTokens > current.MinPromptTokens
	}
	return candidate.hasWindow() && !current.hasWindow()
}

// windowHolds reads the billing window on the UTC clock, half-open, wrapping
// past midnight when end <= start.
func windowHolds(tier priceTier, at time.Time) bool {
	if !tier.hasWindow() {
		return true
	}
	utc := at.UTC()
	minutes := utc.Hour()*60 + utc.Minute()
	start := clockMinutes(*tier.UTCStart)
	end := clockMinutes(*tier.UTCEnd)
	if start == end {
		return true
	}
	if start < end {
		return minutes >= start && minutes < end
	}
	return minutes >= start || minutes < end
}

// clockMinutes turns an HHMM integer into minutes since midnight, tolerating a
// value stored as minutes already.
func clockMinutes(value int) int {
	if value <= 2400 {
		return (value/100)*60 + value%100
	}
	return value
}

// priceBookState is the cached price book persisted with the ledger.
type priceBookState struct {
	Source    string                `json:"source"`
	FetchedAt *time.Time            `json:"fetched_at,omitempty"`
	Models    map[string]modelPrice `json:"models,omitempty"`
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

// tieredModels counts models whose price depends on context size or time.
func (b *priceBookState) tieredModels() int {
	if b == nil {
		return 0
	}
	count := 0
	for _, price := range b.Models {
		if len(price.Tiers) > 0 {
			count++
		}
	}
	return count
}

func (b *priceBookState) summary() map[string]any {
	if b == nil {
		return map[string]any{"source": "", "models": 0, "tiered": 0, "stale": true}
	}
	view := map[string]any{
		"source":     b.Source,
		"models":     len(b.Models),
		"tiered":     b.tieredModels(),
		"last_error": b.LastError,
	}
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
func (b *priceBookState) lookup(model string, overrides map[string]modelPrice) (modelPrice, string, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return modelPrice{}, "", false
	}
	if price, ok := overrides[name]; ok {
		return price, "override", true
	}
	if b == nil || len(b.Models) == 0 {
		return modelPrice{}, "", false
	}
	for _, candidate := range modelCandidates(name) {
		if price, ok := b.Models[candidate]; ok {
			return price, "openrouter", true
		}
	}
	return modelPrice{}, "", false
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
//
// The optional `pricing.overrides` array carries the vendor's own conditional
// prices, which is where long-context steps such as "the first 200k tokens cost
// one rate, beyond that another" come from.
func decodeOpenRouter(body []byte) (map[string]modelPrice, error) {
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
	models := make(map[string]modelPrice, len(envelope.Data))
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
		models[id] = modelPrice{
			Base: priceRates{
				PromptPer1M:     prompt,
				CompletionPer1M: completion,
				CacheReadPer1M:  cacheRead,
				CacheWritePer1M: cacheWrite,
			},
			Tiers: decodeOpenRouterTiers(entry.Pricing["overrides"], okCacheRead, okCacheWrite),
		}
	}
	return models, nil
}

// decodeOpenRouterTiers reads one model's conditional prices. Entries without a
// size threshold or a window carry no condition and are dropped.
func decodeOpenRouterTiers(raw json.RawMessage, hasCacheRead, hasCacheWrite bool) []priceTier {
	if len(raw) == 0 {
		return nil
	}
	var entries []map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&entries); errDecode != nil {
		return nil
	}
	tiers := make([]priceTier, 0, len(entries))
	for _, entry := range entries {
		var tier priceTier
		if value, ok := integerField(entry["min_prompt_tokens"]); ok && value > 0 {
			tier.MinPromptTokens = value
		}
		start, hasStart := integerField(entry["utc_start"])
		end, hasEnd := integerField(entry["utc_end"])
		if hasStart && hasEnd {
			startClock, endClock := int(start), int(end)
			tier.UTCStart, tier.UTCEnd = &startClock, &endClock
		}
		tier.PromptPer1M = optionalPerMillion(entry["prompt"])
		tier.CompletionPer1M = optionalPerMillion(entry["completion"])
		tier.CacheReadPer1M = optionalPerMillion(entry["input_cache_read"])
		tier.CacheWritePer1M = optionalPerMillion(entry["input_cache_write"])
		if tier.PromptPer1M != nil && !hasCacheRead && tier.CacheReadPer1M == nil {
			value := *tier.PromptPer1M
			tier.CacheReadPer1M = &value
		}
		if tier.PromptPer1M != nil && !hasCacheWrite && tier.CacheWritePer1M == nil {
			value := *tier.PromptPer1M
			tier.CacheWritePer1M = &value
		}
		if !tier.hasCondition() {
			continue
		}
		tiers = append(tiers, tier)
	}
	if len(tiers) == 0 {
		return nil
	}
	return tiers
}

func integerField(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	text := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	value, errParse := strconv.ParseInt(text, 10, 64)
	if errParse != nil {
		return 0, false
	}
	return value, true
}

// optionalPerMillion is perMillion with "absent" kept distinct from "zero".
func optionalPerMillion(raw json.RawMessage) *float64 {
	value, ok := perMillion(raw)
	if !ok {
		return nil
	}
	return &value
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

// The upstream price list is public but not free to hammer: the host reloads the
// plugin several times at startup, so refreshes are rate limited.
var (
	priceRefreshMu       sync.Mutex
	lastPriceRefresh     time.Time
	priceRefreshCooldown = 5 * time.Minute
)

// refreshPriceBook fetches the public price list and stores it. Failures keep
// the previous book and only record the error, so a transient outage never
// changes how requests are billed.
//
// The host calls plugin.register/reconfigure several times while it starts, and
// each call installs a refresher; the cooldown keeps that from turning into a
// burst of upstream fetches. A manual refresh passes force=true.
func refreshPriceBook(led *ledger, cfg Config, client *http.Client, force ...bool) error {
	if cfg.PriceSource != "openrouter" {
		return nil
	}
	forceRefresh := len(force) > 0 && force[0]
	priceRefreshMu.Lock()
	if !forceRefresh && !lastPriceRefresh.IsZero() && time.Since(lastPriceRefresh) < priceRefreshCooldown {
		priceRefreshMu.Unlock()
		return nil
	}
	priceRefreshMu.Unlock()
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
	book := &priceBookState{Source: "openrouter", FetchedAt: &now, Models: models}
	led.storePriceBook(book)
	led.persist()
	priceRefreshMu.Lock()
	lastPriceRefresh = time.Now()
	priceRefreshMu.Unlock()
	appendLog(fmt.Sprintf("price book refreshed: %d models, %d with stepped prices", len(models), book.tieredModels()))
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
