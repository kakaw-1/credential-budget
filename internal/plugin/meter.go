package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// usageDetail mirrors CPA's token accounting block.
type usageDetail struct {
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64
}

type usageFailure struct {
	StatusCode int
	Body       string
}

// usageRecord mirrors the host's usage.handle payload. The host serializes the
// struct with Go field names, so these tags must stay as they are.
type usageRecord struct {
	RequestID            string
	TraceID              string
	SelectionID          string
	Provider             string
	BaseURL              string
	ExecutorType         string
	Model                string
	Alias                string
	APIKey               string
	IsNativeKey          bool
	AccessProvider       string
	SessionID            string
	ParentSessionID      string
	AuthID               string
	AuthIndex            string
	AuthType             string
	Source               string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	ResponseModel        string
	Generate             bool
	Stream               bool
	RequestedAt          time.Time
	Failed               bool
	Failure              usageFailure
	Detail               usageDetail
}

// tokensFromDetail converts CPA token counters into billable buckets. Input
// already includes the cached tokens; CPA reports cache writes separately.
func tokensFromDetail(detail usageDetail) tokens {
	output := detail.OutputTokens
	if detail.ReasoningTokens > output {
		// Some upstreams report reasoning tokens outside the completion count.
		output = detail.ReasoningTokens
	}
	cacheRead := detail.CacheReadTokens
	cacheWrite := detail.CacheCreationTokens
	if cacheRead == 0 && cacheWrite == 0 && detail.CachedTokens > 0 {
		// Older CPA builds publish only the aggregate cached counter.
		cacheRead = detail.CachedTokens
	}
	return tokens{
		Input:      detail.InputTokens,
		Output:     output,
		CacheRead:  cacheRead,
		CacheWrite: cacheWrite,
	}
}

func (t tokens) empty() bool {
	return t.Input == 0 && t.Output == 0 && t.CacheRead == 0 && t.CacheWrite == 0
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// handleUsage meters one settled request. It never fails the caller: a metering
// error must not turn a served request into a gateway error.
func handleUsage(request []byte) []byte {
	rt := runtime()
	if rt == nil {
		return okEnvelope(map[string]any{})
	}
	var record usageRecord
	if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
		appendLog("usage.handle: undecodable record: " + errUnmarshal.Error())
		return okEnvelope(map[string]any{})
	}
	// Generate=false marks bookkeeping calls that produced no billable output.
	if !record.Generate {
		return okEnvelope(map[string]any{})
	}
	usage := tokensFromDetail(record.Detail)
	if usage.empty() {
		return okEnvelope(map[string]any{})
	}
	key := credentialKey(strings.TrimSpace(record.AuthID), strings.TrimSpace(record.Provider), strings.TrimSpace(record.AuthIndex))
	if key == "" {
		return okEnvelope(map[string]any{})
	}

	model := firstNonEmpty(record.Model, record.ResponseModel, record.Alias)
	price, _, priced := rt.ledger.priceBook().lookup(model, rt.cfg.OverrideRates)
	at := settleTime(record)
	tierLabel := ""
	if !priced {
		if rt.cfg.UnpricedPolicy == "skip" {
			limitMicro, limitSource := rt.cfg.defaultLimitFor(record.Provider)
			rt.ledger.settle(settleInput{
				Key: key, AuthIndex: record.AuthIndex, Provider: record.Provider,
				BaseURL: record.BaseURL, Model: model, Unpriced: true, At: at,
				DefaultLimitMicro: limitMicro, DefaultLimitSource: limitSource,
			})
			return okEnvelope(map[string]any{})
		}
		price = modelPrice{Base: rt.cfg.FallbackRates}
	}
	// The tier is chosen by the full input (cached tokens included), the same
	// key the price book gates on.
	rates, tier := price.rateCard(usage.Input, at)
	if tier != nil {
		tierLabel = tierLabelFor(tier)
	}
	cost, errQuote := quoteMicro(rates, usage, rt.cfg.ChannelMultiplier)
	if errQuote != nil {
		appendLog("usage.handle: " + errQuote.Error())
		return okEnvelope(map[string]any{})
	}
	limitMicro, limitSource := rt.cfg.defaultLimitFor(record.Provider)
	rt.ledger.settle(settleInput{
		Key:                key,
		AuthIndex:          record.AuthIndex,
		Provider:           record.Provider,
		BaseURL:            record.BaseURL,
		Model:              model,
		CostMicro:          cost,
		Unpriced:           !priced,
		At:                 at,
		DefaultLimitMicro:  limitMicro,
		DefaultLimitSource: limitSource,
		TierLabel:          tierLabel,
	})
	return okEnvelope(map[string]any{})
}

// tierLabelFor names the applied step so the panel can show why a request cost
// more than the base rate suggests.
func tierLabelFor(tier *priceTier) string {
	if tier == nil {
		return ""
	}
	switch {
	case tier.MinPromptTokens > 0 && tier.hasWindow():
		return fmt.Sprintf("≥%dk · %04d-%04d UTC", tier.MinPromptTokens/1000, *tier.UTCStart, *tier.UTCEnd)
	case tier.MinPromptTokens > 0:
		return fmt.Sprintf("≥%dk tokens", tier.MinPromptTokens/1000)
	case tier.hasWindow():
		return fmt.Sprintf("%04d-%04d UTC", *tier.UTCStart, *tier.UTCEnd)
	}
	return ""
}

// settleTime prefers the host's receive time so a queued record keeps its order.
func settleTime(record usageRecord) time.Time {
	if !record.RequestedAt.IsZero() {
		return record.RequestedAt.UTC()
	}
	return unixNow()
}
