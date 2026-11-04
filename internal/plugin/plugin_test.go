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

func TestCredentialKeyKeepsProvidersApart(t *testing.T) {
	// Both providers number their credentials from zero. Keying on the index
	// alone would merge alpha#0 and beta#0 into one row, which is exactly the
	// "only the last provider is listed" symptom.
	alpha := credentialKey("", "openai-compatible-alpha", "0")
	beta := credentialKey("", "openai-compatible-beta", "0")
	if alpha == beta {
		t.Fatalf("index-only keys collided: %q", alpha)
	}
	if alpha != "openai-compatible-alpha#0" {
		t.Fatalf("alpha key = %q, want openai-compatible-alpha#0", alpha)
	}
	// A real auth ID is already globally unique and is kept verbatim.
	if got := credentialKey("openai-compatibility:alpha:1149eb559164", "openai-compatible-alpha", "3"); got != "openai-compatibility:alpha:1149eb559164" {
		t.Fatalf("auth ID was rewritten to %q", got)
	}
	if got := credentialKey("", "", ""); got != "" {
		t.Fatalf("empty identity produced key %q", got)
	}
}

func TestSyncSeedsEveryProviderFromTheRuntimeList(t *testing.T) {
	_, rt := setupRuntime(t, `
price_source: "off"
default_limit_usd: 1
provider_limits:
  alpha: 5
fallback_price: {prompt: 1000, completion: 1000, cache_read: 1000, cache_write: 1000}
`)
	credentials := []hostAuthModelEntry{
		{ID: "openai-compatibility:alpha:aaa", AuthIndex: "0", Provider: "openai-compatible-alpha"},
		{ID: "openai-compatibility:alpha:aaa-1", AuthIndex: "1", Provider: "openai-compatible-alpha"},
		{ID: "openai-compatibility:beta:bbb", AuthIndex: "2", Provider: "openai-compatible-beta"},
	}
	seen := map[string]bool{}
	for _, credential := range credentials {
		key := credentialKey(credential.ID, credential.Provider, credential.AuthIndex)
		limitMicro, limitSource := rt.cfg.defaultLimitFor(credential.Provider)
		rt.ledger.observe(key, credential.AuthIndex, credential.Provider, "", limitMicro, limitSource)
		seen[key] = true
	}
	if rows := rt.ledger.rows(); len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (%v)", len(rows), rows)
	}
	alphaRow, _ := rt.ledger.row("openai-compatibility:alpha:aaa")
	if alphaRow.LimitMicroUSD != 5_000_000 {
		t.Fatalf("alpha limit = %d, want 5000000 from provider_limits", alphaRow.LimitMicroUSD)
	}
	betaRow, _ := rt.ledger.row("openai-compatibility:beta:bbb")
	if betaRow.LimitMicroUSD != 1_000_000 || betaRow.LimitSource != "default" {
		t.Fatalf("beta row = limit %d source %q, want the global default", betaRow.LimitMicroUSD, betaRow.LimitSource)
	}
	for _, row := range rt.ledger.rows() {
		if !seen[row.Key] {
			t.Fatalf("row %q was not enumerated", row.Key)
		}
		if row.Requests != 0 {
			t.Fatalf("row %q should read as unused, got %d request(s)", row.Key, row.Requests)
		}
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

func TestDecodeOpenRouterKeepsSteppedPrices(t *testing.T) {
	body := []byte(`{"data":[
	  {"id":"vendor/long-context-model",
	   "pricing":{
	     "prompt":"0.00000125","completion":"0.00001",
	     "input_cache_read":"0.000000125","input_cache_write":"0.0000015",
	     "overrides":[
	       {"min_prompt_tokens":200000,"prompt":"0.0000025","completion":"0.000015"},
	       {"min_prompt_tokens":1000000,"completion":"0.00002"}
	     ]}}
	]}`)
	models, errDecode := decodeOpenRouter(body)
	if errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	price := models["vendor/long-context-model"]
	if price.Base.PromptPer1M != 1.25 || price.Base.CompletionPer1M != 10 {
		t.Fatalf("base = %+v, want 1.25/10 per 1M", price.Base)
	}
	if len(price.Tiers) != 2 {
		t.Fatalf("tiers = %d, want 2", len(price.Tiers))
	}
	first := price.Tiers[0]
	if first.MinPromptTokens != 200000 || first.PromptPer1M == nil || *first.PromptPer1M != 2.5 {
		t.Fatalf("first tier = %+v", first)
	}
	// A tier that only restates the completion price leaves prompt inheriting base.
	second := price.Tiers[1]
	if second.PromptPer1M != nil {
		t.Fatalf("second tier must not invent a prompt price: %+v", second)
	}
}

func TestSelectTierUsesFullInputAndDoesNotStack(t *testing.T) {
	tiers := []priceTier{
		{MinPromptTokens: 200_000},
		{MinPromptTokens: 1_000_000},
	}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	if tier := selectTier(tiers, 199_999, at); tier != nil {
		t.Fatalf("199999 tokens matched a tier: %+v", tier)
	}
	if tier := selectTier(tiers, 200_000, at); tier == nil || tier.MinPromptTokens != 200_000 {
		t.Fatalf("200000 tokens = %+v, want the 200k tier", tier)
	}
	// Both thresholds are met; only the larger one governs the request.
	if tier := selectTier(tiers, 1_500_000, at); tier == nil || tier.MinPromptTokens != 1_000_000 {
		t.Fatalf("1500000 tokens = %+v, want the 1M tier alone", tier)
	}
}

func TestTierWindowWrapsPastMidnight(t *testing.T) {
	start, end := 2300, 100
	tiers := []priceTier{{UTCStart: &start, UTCEnd: &end}}
	inside := []time.Time{
		time.Date(2026, 11, 4, 23, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 0, 30, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 23, 0, 0, 0, time.UTC),
	}
	for _, at := range inside {
		if tier := selectTier(tiers, 10, at); tier == nil {
			t.Fatalf("%s should fall inside 2300-0100 UTC", at.Format(time.RFC3339))
		}
	}
	outside := []time.Time{
		time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC),
	}
	for _, at := range outside {
		if tier := selectTier(tiers, 10, at); tier != nil {
			t.Fatalf("%s should fall outside 2300-0100 UTC", at.Format(time.RFC3339))
		}
	}
}

func TestQuoteChargesTheMatchedTier(t *testing.T) {
	promptHigh := 2.0
	price := modelPrice{
		Base:  priceRates{PromptPer1M: 1, CompletionPer1M: 4},
		Tiers: []priceTier{{MinPromptTokens: 200_000, PromptPer1M: &promptHigh}},
	}
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

	shortRates, shortTier := price.rateCard(199_999, at)
	if shortTier != nil {
		t.Fatal("a short request must use the base card")
	}
	shortCost, _ := quoteMicro(shortRates, tokens{Input: 199_999}, 1)
	if shortCost != 199_999 {
		t.Fatalf("short cost = %d micro-USD, want 199999 (base rate)", shortCost)
	}

	longRates, longTier := price.rateCard(200_000, at)
	if longTier == nil {
		t.Fatal("a 200k request must hit the tier")
	}
	longCost, _ := quoteMicro(longRates, tokens{Input: 200_000}, 1)
	if longCost != 400_000 {
		t.Fatalf("long cost = %d micro-USD, want 400000 (tier rate)", longCost)
	}
	// The tier only moved the prompt rate; completion keeps the base rate.
	completionRates, _ := price.rateCard(200_000, at)
	if completionRates.CompletionPer1M != 4 {
		t.Fatalf("completion rate = %v, want the base 4", completionRates.CompletionPer1M)
	}
}

func TestConfigOverrideAcceptsTiers(t *testing.T) {
	cfg, errConfig := parseConfig([]byte(`
price_source: "off"
price_overrides:
  gemini-2.5-pro:
    prompt: 1.25
    completion: 10
    tiers:
      - min_prompt_tokens: 200000
        prompt: 2.5
        completion: 15
`))
	if errConfig != nil {
		t.Fatalf("parse: %v", errConfig)
	}
	price := cfg.OverrideRates["gemini-2.5-pro"]
	if price.Base.PromptPer1M != 1.25 || len(price.Tiers) != 1 {
		t.Fatalf("override = %+v", price)
	}
	if price.Tiers[0].CompletionPer1M == nil || *price.Tiers[0].CompletionPer1M != 15 {
		t.Fatalf("tier completion = %+v", price.Tiers[0])
	}

	if _, errBad := parseConfig([]byte(`
price_source: "off"
price_overrides:
  broken:
    prompt: 1
    tiers:
      - prompt: 2
`)); errBad == nil {
		t.Fatal("a tier without a threshold or window must be rejected")
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
