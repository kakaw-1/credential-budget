package plugin

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"gopkg.in/yaml.v3"
)

// pricesYAML is the operator-facing shape of one rate card. Rates are USD per
// 1M tokens, the same unit the OpenRouter price book uses.
type pricesYAML struct {
	Prompt     *float64 `yaml:"prompt" json:"prompt"`
	Completion *float64 `yaml:"completion" json:"completion"`
	CacheRead  *float64 `yaml:"cache_read" json:"cache_read"`
	CacheWrite *float64 `yaml:"cache_write" json:"cache_write"`
}

func (p pricesYAML) toRates(fallback priceRates) (priceRates, error) {
	rates := fallback
	for _, item := range []struct {
		name   string
		value  *float64
		target *float64
	}{
		{"prompt", p.Prompt, &rates.PromptPer1M},
		{"completion", p.Completion, &rates.CompletionPer1M},
		{"cache_read", p.CacheRead, &rates.CacheReadPer1M},
		{"cache_write", p.CacheWrite, &rates.CacheWritePer1M},
	} {
		if item.value == nil {
			continue
		}
		if math.IsNaN(*item.value) || math.IsInf(*item.value, 0) || *item.value < 0 {
			return priceRates{}, fmt.Errorf("price_overrides.%s must be a non-negative number", item.name)
		}
		*item.target = *item.value
	}
	return rates, nil
}

// Config is the plugin's own block under plugins.configs.<id>.
type Config struct {
	DefaultLimitUSD   float64               `yaml:"default_limit_usd"`
	Enforce           *bool                 `yaml:"enforce"`
	AutoDisable       *bool                 `yaml:"auto_disable"`
	StateFile         string                `yaml:"state_file"`
	PriceSource       string                `yaml:"price_source"`
	PriceRefreshHours float64               `yaml:"price_refresh_hours"`
	UnpricedPolicy    string                `yaml:"unpriced_policy"`
	OnAllExhausted    string                `yaml:"on_all_exhausted"`
	Debug             bool                  `yaml:"debug"`
	FallbackPrice     pricesYAML            `yaml:"fallback_price"`
	PriceOverrides    map[string]pricesYAML `yaml:"price_overrides"`
	ChannelMultiplier float64               `yaml:"channel_multiplier"`

	// Derived, not read from YAML.
	FallbackRates priceRates            `yaml:"-" json:"-"`
	OverrideRates map[string]priceRates `yaml:"-" json:"-"`
}

func defaultConfig() Config {
	return Config{
		DefaultLimitUSD:   0,
		StateFile:         "plugins/" + PluginID + "-state.json",
		PriceSource:       "openrouter",
		PriceRefreshHours: 24,
		UnpricedPolicy:    "fallback",
		ChannelMultiplier: 1,
		FallbackRates:     defaultFallbackRates(),
		OverrideRates:     map[string]priceRates{},
	}
}

// defaultFallbackRates is deliberately not zero: an unpriced model must never be
// free, or the budget silently stops working for it.
func defaultFallbackRates() priceRates {
	return priceRates{PromptPer1M: 3, CompletionPer1M: 15, CacheReadPer1M: 0.3, CacheWritePer1M: 3.75}
}

func (c Config) enforceEnabled() bool {
	return c.Enforce == nil || *c.Enforce
}

func (c Config) autoDisableEnabled() bool {
	return c.AutoDisable == nil || *c.AutoDisable
}

// rejectsWhenAllExhausted reports whether the scheduler should refuse a request
// when every credential offered for its route is over budget. Rejecting is the
// honest answer; "pass" exists for operators who would rather keep serving.
func (c Config) rejectsWhenAllExhausted() bool {
	return c.OnAllExhausted != "pass"
}

func parseConfig(raw []byte) (Config, error) {
	cfg := defaultConfig()
	if len(raw) == 0 {
		return cfg, nil
	}
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return Config{}, fmt.Errorf("parse plugins.configs.%s: %w", PluginID, errUnmarshal)
	}
	cfg.StateFile = strings.TrimSpace(cfg.StateFile)
	if cfg.StateFile == "" {
		cfg.StateFile = "plugins/" + PluginID + "-state.json"
	}
	cfg.PriceSource = strings.ToLower(strings.TrimSpace(cfg.PriceSource))
	switch cfg.PriceSource {
	case "openrouter", "off":
	case "":
		cfg.PriceSource = "openrouter"
	default:
		return Config{}, fmt.Errorf("price_source must be openrouter or off, got %q", cfg.PriceSource)
	}
	cfg.UnpricedPolicy = strings.ToLower(strings.TrimSpace(cfg.UnpricedPolicy))
	switch cfg.UnpricedPolicy {
	case "fallback", "skip":
	case "":
		cfg.UnpricedPolicy = "fallback"
	default:
		return Config{}, fmt.Errorf("unpriced_policy must be fallback or skip, got %q", cfg.UnpricedPolicy)
	}
	cfg.OnAllExhausted = strings.ToLower(strings.TrimSpace(cfg.OnAllExhausted))
	switch cfg.OnAllExhausted {
	case "reject", "pass":
	case "":
		cfg.OnAllExhausted = "reject"
	default:
		return Config{}, fmt.Errorf("on_all_exhausted must be reject or pass, got %q", cfg.OnAllExhausted)
	}
	if cfg.DefaultLimitUSD < 0 || math.IsNaN(cfg.DefaultLimitUSD) || math.IsInf(cfg.DefaultLimitUSD, 0) {
		return Config{}, fmt.Errorf("default_limit_usd must be a non-negative number")
	}
	if cfg.PriceRefreshHours <= 0 || math.IsNaN(cfg.PriceRefreshHours) {
		cfg.PriceRefreshHours = 24
	}
	if cfg.ChannelMultiplier <= 0 || math.IsNaN(cfg.ChannelMultiplier) || math.IsInf(cfg.ChannelMultiplier, 0) {
		cfg.ChannelMultiplier = 1
	}
	fallback, errFallback := cfg.FallbackPrice.toRates(defaultFallbackRates())
	if errFallback != nil {
		return Config{}, errFallback
	}
	cfg.FallbackRates = fallback
	overrides := make(map[string]priceRates, len(cfg.PriceOverrides))
	for model, entry := range cfg.PriceOverrides {
		key := strings.TrimSpace(model)
		if key == "" {
			continue
		}
		rates, errRates := entry.toRates(cfg.FallbackRates)
		if errRates != nil {
			return Config{}, fmt.Errorf("price_overrides[%s]: %w", key, errRates)
		}
		overrides[strings.ToLower(key)] = rates
	}
	cfg.OverrideRates = overrides
	return cfg, nil
}

// publicView is what the panel may see: configuration without secrets. The
// state file path and the raw override table stay server-side.
func (c Config) publicView() map[string]any {
	return map[string]any{
		"default_limit_usd":   c.DefaultLimitUSD,
		"enforce":             c.enforceEnabled(),
		"auto_disable":        c.autoDisableEnabled(),
		"price_source":        c.PriceSource,
		"price_refresh_hours": c.PriceRefreshHours,
		"unpriced_policy":     c.UnpricedPolicy,
		"on_all_exhausted":    c.OnAllExhausted,
		"debug":               c.Debug,
		"channel_multiplier":  c.ChannelMultiplier,
		"fallback_price": map[string]float64{
			"prompt":      c.FallbackRates.PromptPer1M,
			"completion":  c.FallbackRates.CompletionPer1M,
			"cache_read":  c.FallbackRates.CacheReadPer1M,
			"cache_write": c.FallbackRates.CacheWritePer1M,
		},
		"override_models": overrideModels(c.OverrideRates),
	}
}

func overrideModels(rates map[string]priceRates) []string {
	models := make([]string, 0, len(rates))
	for model := range rates {
		models = append(models, model)
	}
	sortStrings(models)
	return models
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// runtimeState holds the live configuration and ledger for one loaded plugin.
type runtimeState struct {
	cfg    Config
	ledger *ledger
}

func configure(request []byte, firstLoad bool) error {
	var req lifecycleRequest
	if len(request) > 0 {
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return fmt.Errorf("decode plugin lifecycle request: %w", errUnmarshal)
		}
	}
	cfg, errConfig := parseConfig(req.ConfigYAML)
	if errConfig != nil {
		return errConfig
	}
	return applyConfig(cfg, firstLoad)
}
