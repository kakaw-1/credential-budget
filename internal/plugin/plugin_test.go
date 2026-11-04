package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost emulates the CPA host callbacks the plugin depends on.
type fakeHost struct {
	mu       sync.Mutex
	authDir  string
	entries  []hostAuthEntry
	saves    []map[string]any
	logLines []string
}

func newFakeHost(t *testing.T, dir string) *fakeHost {
	t.Helper()
	return &fakeHost{authDir: dir}
}

func (f *fakeHost) addFileAuth(t *testing.T, name, provider string, payload map[string]any) hostAuthEntry {
	t.Helper()
	if errWrite := os.MkdirAll(f.authDir, 0o700); errWrite != nil {
		t.Fatalf("mkdir auth dir: %v", errWrite)
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		t.Fatalf("marshal auth: %v", errMarshal)
	}
	if errWrite := os.WriteFile(filepath.Join(f.authDir, name), raw, 0o600); errWrite != nil {
		t.Fatalf("write auth file: %v", errWrite)
	}
	entry := hostAuthEntry{ID: name, AuthIndex: "idx-" + name, Name: name, Provider: provider}
	f.entries = append(f.entries, entry)
	return entry
}

func (f *fakeHost) call(method string, payload any) (json.RawMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "host.log":
		if fields, ok := payload.(map[string]any); ok {
			if message, okMessage := fields["message"].(string); okMessage {
				f.logLines = append(f.logLines, message)
			}
		}
		return json.RawMessage(`{}`), nil
	case "host.auth.list":
		return mustJSON(hostAuthListResponse{Files: f.entries}), nil
	case "host.auth.get":
		request, _ := payload.(map[string]any)
		authIndex, _ := request["auth_index"].(string)
		for _, entry := range f.entries {
			if entry.AuthIndex != authIndex {
				continue
			}
			raw, errRead := os.ReadFile(filepath.Join(f.authDir, entry.Name))
			if errRead != nil {
				return nil, errRead
			}
			return mustJSON(hostAuthGetResponse{
				AuthIndex: entry.AuthIndex,
				Name:      entry.Name,
				JSON:      json.RawMessage(raw),
			}), nil
		}
		return nil, fmt.Errorf("unknown auth index %q", authIndex)
	case "host.auth.save":
		request, _ := payload.(map[string]any)
		name, _ := request["name"].(string)
		raw, _ := request["json"].(json.RawMessage)
		if name == "" || len(raw) == 0 {
			return nil, fmt.Errorf("bad save request")
		}
		if errWrite := os.WriteFile(filepath.Join(f.authDir, name), raw, 0o600); errWrite != nil {
			return nil, errWrite
		}
		f.saves = append(f.saves, map[string]any{"name": name, "json": string(raw)})
		return mustJSON(map[string]any{"name": name, "path": filepath.Join(f.authDir, name)}), nil
	default:
		return nil, fmt.Errorf("unsupported host method %q", method)
	}
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}

func (f *fakeHost) savedDisabled(name string) (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.saves) - 1; i >= 0; i-- {
		if f.saves[i]["name"] != name {
			continue
		}
		var payload map[string]any
		if errUnmarshal := json.Unmarshal([]byte(f.saves[i]["json"].(string)), &payload); errUnmarshal != nil {
			return false, false
		}
		value, ok := payload["disabled"].(bool)
		return value, ok
	}
	return false, false
}

// setupRuntime installs a runtime using a fake host and an isolated state file.
func setupRuntime(t *testing.T, cfgYAML string) (*fakeHost, *runtimeState) {
	t.Helper()
	Quiesce()
	dir := t.TempDir()
	host := newFakeHost(t, filepath.Join(dir, "auths"))
	SetHostCall(host.call)
	cfg, errConfig := parseConfig([]byte(cfgYAML + "\nstate_file: " + filepath.Join(dir, "state.json") + "\n"))
	if errConfig != nil {
		t.Fatalf("parse config: %v", errConfig)
	}
	if errApply := applyConfig(cfg, true); errApply != nil {
		t.Fatalf("apply config: %v", errApply)
	}
	t.Cleanup(func() {
		Quiesce()
		SetHostCall(nil)
	})
	return host, runtime()
}

func usagePayload(authID, authIndex, model string, input, output int64) []byte {
	record := map[string]any{
		"RequestID":   "req-1",
		"Provider":    "claude",
		"Model":       model,
		"AuthID":      authID,
		"AuthIndex":   authIndex,
		"Generate":    true,
		"Failed":      false,
		"RequestedAt": time.Now().UTC(),
		"Detail": map[string]any{
			"InputTokens":  input,
			"OutputTokens": output,
		},
	}
	raw, _ := json.Marshal(record)
	return raw
}

func TestUsageTripsBudgetAndDisablesAuthFile(t *testing.T) {
	// $1000 per 1M tokens means 1 token costs 1000 micro-USD, so the $0.005
	// budget is consumed after a handful of tokens.
	host, rt := setupRuntime(t, `
default_limit_usd: 0.005
price_source: "off"
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
`)
	entry := host.addFileAuth(t, "acc-a.json", "claude", map[string]any{
		"type": "claude", "email": "a@example.com", "access_token": "token-a", "disabled": false,
	})

	handleUsage(usagePayload(entry.ID, entry.AuthIndex, "claude-sonnet-4-5", 2, 2))
	row, ok := rt.ledger.row(entry.ID)
	if !ok {
		t.Fatalf("ledger has no row for %s", entry.ID)
	}
	if row.Exhausted {
		t.Fatalf("credential tripped too early: spent=%d", row.SpentMicroUSD)
	}

	handleUsage(usagePayload(entry.ID, entry.AuthIndex, "claude-sonnet-4-5", 2, 2))
	row, _ = rt.ledger.row(entry.ID)
	if !row.Exhausted {
		t.Fatalf("credential did not trip at the budget: spent=%d limit=%d", row.SpentMicroUSD, row.LimitMicroUSD)
	}
	if !row.DisabledByPlugin {
		t.Fatalf("DisableError=%q, want the auth file disabled", row.DisableError)
	}
	disabled, present := host.savedDisabled("acc-a.json")
	if !present || !disabled {
		t.Fatalf("host.auth.save did not write disabled=true (present=%v disabled=%v)", present, disabled)
	}
	// The rest of the credential JSON must survive the rewrite.
	raw, errRead := os.ReadFile(filepath.Join(host.authDir, "acc-a.json"))
	if errRead != nil {
		t.Fatalf("read auth file: %v", errRead)
	}
	if !strings.Contains(string(raw), "token-a") {
		t.Fatalf("rewrite dropped the credential material: %s", raw)
	}
}

func TestUnsealClearsSpendAndReenables(t *testing.T) {
	host, rt := setupRuntime(t, `
default_limit_usd: 0.002
price_source: "off"
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
`)
	entry := host.addFileAuth(t, "acc-b.json", "claude", map[string]any{
		"type": "claude", "access_token": "token-b",
	})
	handleUsage(usagePayload(entry.ID, entry.AuthIndex, "claude-sonnet-4-5", 5, 0))
	row, _ := rt.ledger.row(entry.ID)
	if !row.Exhausted || !row.DisabledByPlugin {
		t.Fatalf("setup: exhausted=%v disabled=%v", row.Exhausted, row.DisabledByPlugin)
	}

	response := unsealResponse([]byte(fmt.Sprintf(`{"key":%q}`, entry.ID)))
	if response.StatusCode != 200 {
		t.Fatalf("unseal status = %d body=%s", response.StatusCode, response.Body)
	}
	row, _ = rt.ledger.row(entry.ID)
	if row.Exhausted || row.DisabledByPlugin || row.SpentMicroUSD != 0 {
		t.Fatalf("after unseal: %+v", row)
	}
	disabled, present := host.savedDisabled("acc-b.json")
	if !present || disabled {
		t.Fatalf("re-enable did not write disabled=false (present=%v disabled=%v)", present, disabled)
	}
}

func TestSchedulerVetoesExhaustedCandidate(t *testing.T) {
	_, rt := setupRuntime(t, `
default_limit_usd: 0.002
price_source: "off"
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
auto_disable: false
`)
	handleUsage(usagePayload("auth-a", "idx-a", "claude-sonnet-4-5", 5, 0))
	if _, ok := rt.ledger.row("auth-a"); !ok {
		t.Fatal("missing ledger row")
	}

	request, _ := json.Marshal(map[string]any{
		"Provider": "claude",
		"Model":    "claude-sonnet-4-5",
		"Candidates": []map[string]any{
			{"ID": "auth-a", "Provider": "claude", "Priority": 10},
			{"ID": "auth-b", "Provider": "claude", "Priority": 5},
		},
	})
	raw := handlePick(request)
	var envelope struct {
		OK     bool                  `json:"ok"`
		Result schedulerPickResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode pick response: %v", errUnmarshal)
	}
	if !envelope.Result.Handled || envelope.Result.AuthID != "auth-b" || envelope.Result.Reject {
		t.Fatalf("pick = %+v, want auth-b handled", envelope.Result)
	}
}

func TestSchedulerRejectsWhenAllCandidatesExhausted(t *testing.T) {
	setupRuntime(t, `
default_limit_usd: 0.002
price_source: "off"
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
auto_disable: false
`)
	handleUsage(usagePayload("auth-a", "idx-a", "claude-sonnet-4-5", 5, 0))
	handleUsage(usagePayload("auth-b", "idx-b", "claude-sonnet-4-5", 5, 0))

	request, _ := json.Marshal(map[string]any{
		"Provider":   "claude",
		"Candidates": []map[string]any{{"ID": "auth-a"}, {"ID": "auth-b"}},
	})
	raw := handlePick(request)
	var envelope struct {
		Result schedulerPickResponse `json:"result"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode pick response: %v", errUnmarshal)
	}
	if !envelope.Result.Reject || envelope.Result.RejectCode != "credential_budget_exhausted" {
		t.Fatalf("pick = %+v, want a budget rejection", envelope.Result)
	}

	// "pass" hands the decision back to the host instead of refusing. It needs its
	// own runtime: the configuration of a live runtime is never mutated in place.
	_, passRuntime := setupRuntime(t, `
default_limit_usd: 0.002
price_source: "off"
on_all_exhausted: "pass"
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
auto_disable: false
`)
	handleUsage(usagePayload("auth-a", "idx-a", "claude-sonnet-4-5", 5, 0))
	handleUsage(usagePayload("auth-b", "idx-b", "claude-sonnet-4-5", 5, 0))
	if _, ok := passRuntime.ledger.row("auth-a"); !ok {
		t.Fatal("pass runtime missing ledger row")
	}
	raw = handlePick(request)
	envelope.Result = schedulerPickResponse{}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		t.Fatalf("decode pick response: %v", errUnmarshal)
	}
	if envelope.Result.Handled || envelope.Result.Reject {
		t.Fatalf("pass mode pick = %+v, want unhandled", envelope.Result)
	}
}

func TestUnpricedModelUsesFallbackRates(t *testing.T) {
	_, rt := setupRuntime(t, `
default_limit_usd: 0
price_source: "off"
fallback_price: {prompt: 2, completion: 8, cache_read: 0.5, cache_write: 1}
`)
	handleUsage(usagePayload("auth-c", "idx-c", "unknown-model-x", 1_000_000, 0))
	row, _ := rt.ledger.row("auth-c")
	// 1M prompt tokens at $2/1M is exactly $2.
	if row.SpentMicroUSD != 2_000_000 {
		t.Fatalf("spent = %d micro-USD, want 2000000", row.SpentMicroUSD)
	}
	if row.UnpricedRequests != 1 {
		t.Fatalf("unpriced counter = %d, want 1", row.UnpricedRequests)
	}
}

func TestSkipPolicyChargesNothingButCounts(t *testing.T) {
	_, rt := setupRuntime(t, `
price_source: "off"
unpriced_policy: "skip"
`)
	handleUsage(usagePayload("auth-d", "idx-d", "unknown-model-y", 1_000_000, 0))
	row, _ := rt.ledger.row("auth-d")
	if row.SpentMicroUSD != 0 || row.UnpricedRequests != 1 || row.Requests != 1 {
		t.Fatalf("row = %+v, want zero spend with one unpriced request", row)
	}
}

func TestCachedTokensAreNotBilledTwice(t *testing.T) {
	_, rt := setupRuntime(t, `
price_source: "off"
fallback_price: {prompt: 3, completion: 15, cache_read: 0.3, cache_write: 3.75}
`)
	record := map[string]any{
		"Model":    "claude-sonnet-4-5",
		"AuthID":   "auth-e",
		"Generate": true,
		"Detail": map[string]any{
			"InputTokens":         1_000_000,
			"OutputTokens":        0,
			"CacheReadTokens":     1_000_000,
			"CacheCreationTokens": 0,
		},
	}
	raw, _ := json.Marshal(record)
	handleUsage(raw)
	row, _ := rt.ledger.row("auth-e")
	// Every prompt token was a cache read, so only the cache-read rate applies.
	if row.SpentMicroUSD != 300_000 {
		t.Fatalf("spent = %d micro-USD, want 300000 (cache read only)", row.SpentMicroUSD)
	}
}

func TestUsageWithoutTokensIsIgnored(t *testing.T) {
	_, rt := setupRuntime(t, `price_source: "off"`)
	record := map[string]any{"Model": "m", "AuthID": "auth-f", "Generate": true}
	raw, _ := json.Marshal(record)
	handleUsage(raw)
	if _, ok := rt.ledger.row("auth-f"); ok {
		t.Fatal("a request without tokens must not create a ledger row")
	}
}

func TestProviderLimitsSeedNewCredentialsOnly(t *testing.T) {
	_, rt := setupRuntime(t, `
price_source: "off"
default_limit_usd: 1
provider_limits:
  claude: 5
  mock: 0.25
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
`)
	// "openai-compatible-mock" contains "mock", so the provider entry wins over
	// the global default.
	handleUsage(usagePayload("auth-i", "idx-i", "claude-sonnet-4-5", 1, 0))
	handleUsageWithProvider(t, "auth-j", "openai-compatible-mock", "gpt-4o-mini", 1, 0)
	handleUsageWithProvider(t, "auth-k", "codex", "gpt-5.1", 1, 0)

	claudeRow, _ := rt.ledger.row("auth-i")
	if claudeRow.LimitMicroUSD != 5_000_000 || claudeRow.LimitSource != "provider_default" {
		t.Fatalf("claude row = limit %d source %q, want 5 USD from provider_limits", claudeRow.LimitMicroUSD, claudeRow.LimitSource)
	}
	mockRow, _ := rt.ledger.row("auth-j")
	if mockRow.LimitMicroUSD != 250_000 {
		t.Fatalf("mock row limit = %d, want 250000 micro-USD", mockRow.LimitMicroUSD)
	}
	codexRow, _ := rt.ledger.row("auth-k")
	if codexRow.LimitMicroUSD != 1_000_000 || codexRow.LimitSource != "default" {
		t.Fatalf("codex row = limit %d source %q, want the global default", codexRow.LimitMicroUSD, codexRow.LimitSource)
	}

	// An operator-set budget survives a later settlement of the same credential.
	if _, _, errLimit := rt.ledger.setLimit("auth-i", 42_000_000); errLimit != nil {
		t.Fatalf("setLimit: %v", errLimit)
	}
	handleUsage(usagePayload("auth-i", "idx-i", "claude-sonnet-4-5", 1, 0))
	claudeRow, _ = rt.ledger.row("auth-i")
	if claudeRow.LimitMicroUSD != 42_000_000 || claudeRow.LimitSource != "manual" {
		t.Fatalf("manual budget was overwritten: limit %d source %q", claudeRow.LimitMicroUSD, claudeRow.LimitSource)
	}
}

func handleUsageWithProvider(t *testing.T, authID, provider, model string, input, output int64) {
	t.Helper()
	record := map[string]any{
		"Model":    model,
		"Provider": provider,
		"AuthID":   authID,
		"Generate": true,
		"Detail":   map[string]any{"InputTokens": input, "OutputTokens": output},
	}
	raw, _ := json.Marshal(record)
	handleUsage(raw)
}

func TestAliasNamesACredential(t *testing.T) {
	_, rt := setupRuntime(t, `price_source: "off"`)
	handleUsage(usagePayload("openai-compatibility:mock:68bc4482762f", "idx-x", "gpt-4o-mini", 1, 0))

	response := aliasResponse([]byte(`{"key":"openai-compatibility:mock:68bc4482762f","alias":"5刀-A"}`))
	if response.StatusCode != 200 {
		t.Fatalf("alias status = %d body=%s", response.StatusCode, response.Body)
	}
	row, _ := rt.ledger.row("openai-compatibility:mock:68bc4482762f")
	if row.Alias != "5刀-A" {
		t.Fatalf("alias = %q", row.Alias)
	}
	if displayName(row) != "5刀-A" {
		t.Fatalf("display = %q, want the alias", displayName(row))
	}
	// The id tail still distinguishes two sibling keys without any other name.
	if got := idTail(Credential{Key: "openai-compatibility:mock:68bc4482762f", AuthIndex: "openai-compatibility:mock:82906d360369"}); got != "360369" {
		t.Fatalf("tail = %q, want the auth index's last six characters", got)
	}
}

func TestSchedulerRegistersEveryCandidateOfAProvider(t *testing.T) {
	_, rt := setupRuntime(t, `
price_source: "off"
provider_limits:
  openai-compatible-mock: 0.005
fallback_price: {prompt: 1, completion: 1, cache_read: 1, cache_write: 1}
`)

	// One request routed by a provider with three configured keys reveals all
	// three, not just the one that serves it.
	request, _ := json.Marshal(map[string]any{
		"Provider": "openai-compatible-mock",
		"Model":    "gpt-4o-mini",
		"Candidates": []map[string]any{
			{"ID": "openai-compatibility:mock:aaa", "Provider": "openai-compatible-mock", "Attributes": map[string]string{"base_url": "https://up.example/v1", "auth_index": "idx-a"}},
			{"ID": "openai-compatibility:mock:bbb", "Provider": "openai-compatible-mock", "Attributes": map[string]string{"base_url": "https://up.example/v1", "auth_index": "idx-b"}},
			{"ID": "openai-compatibility:mock:ccc", "Provider": "openai-compatible-mock", "Attributes": map[string]string{"base_url": "https://up.example/v1", "auth_index": "idx-c"}},
		},
	})
	handlePick(request)

	rows := rt.ledger.rows()
	if len(rows) != 3 {
		t.Fatalf("ledger holds %d rows, want 3: %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.LimitMicroUSD != 5000 {
			t.Fatalf("%s limit = %d, want 5000 from provider_limits", row.Key, row.LimitMicroUSD)
		}
		if row.SpentMicroUSD != 0 || row.Requests != 0 {
			t.Fatalf("%s already has spend/requests: %+v", row.Key, row)
		}
		if row.BaseURL != "https://up.example/v1" {
			t.Fatalf("%s base_url = %q", row.Key, row.BaseURL)
		}
		if view := toView(row); !view.ObservedOnly {
			t.Fatalf("%s should read as observed-only", row.Key)
		}
	}

	// A second pick must not reset a budget or a spend an operator already set.
	if _, _, errLimit := rt.ledger.setLimit("openai-compatibility:mock:bbb", 9_000_000); errLimit != nil {
		t.Fatalf("setLimit: %v", errLimit)
	}
	if _, errAlias := rt.ledger.setAlias("openai-compatibility:mock:bbb", "5刀-B"); errAlias != nil {
		t.Fatalf("setAlias: %v", errAlias)
	}
	handleUsage(usagePayload("openai-compatibility:mock:bbb", "idx-b", "gpt-4o-mini", 100, 0))
	handlePick(request)
	row, _ := rt.ledger.row("openai-compatibility:mock:bbb")
	if row.LimitMicroUSD != 9_000_000 || row.Alias != "5刀-B" || row.SpentMicroUSD == 0 {
		t.Fatalf("re-observing overwrote operator state: %+v", row)
	}
	if view := toView(row); view.ObservedOnly {
		t.Fatal("a credential that served a request must not read as observed-only")
	}
}

func TestLedgerSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	led := newLedger(path)
	led.load()
	led.settle(settleInput{
		Key: "auth-g", AuthIndex: "idx-g", Provider: "claude", Label: "g@example.com",
		BaseURL: "https://api.example.com/v1", Model: "claude-sonnet-4-5",
		CostMicro: 1234, At: time.Now().UTC(), DefaultLimitMicro: 5_000_000,
	})
	led.persist()

	reloaded := newLedger(path)
	reloaded.load()
	row, ok := reloaded.row("auth-g")
	if !ok {
		t.Fatal("row missing after reload")
	}
	if row.SpentMicroUSD != 1234 || row.LimitMicroUSD != 5_000_000 || row.Provider != "claude" {
		t.Fatalf("row = %+v", row)
	}
}

func TestLimitEditReportsPendingUnseal(t *testing.T) {
	dir := t.TempDir()
	led := newLedger(filepath.Join(dir, "state.json"))
	led.load()
	led.settle(settleInput{Key: "auth-h", Provider: "claude", CostMicro: 600, At: time.Now().UTC(), DefaultLimitMicro: 500})
	row, ok := led.row("auth-h")
	if !ok || !row.Exhausted {
		t.Fatalf("setup row = %+v", row)
	}
	_, needsUnseal, errLimit := led.setLimit("auth-h", 5_000_000)
	if errLimit != nil {
		t.Fatalf("setLimit: %v", errLimit)
	}
	if !needsUnseal {
		t.Fatal("raising the budget above spend must ask for an explicit unseal")
	}
	row, _ = led.row("auth-h")
	if !row.Exhausted {
		t.Fatal("raising the budget must not silently resume traffic")
	}
}
