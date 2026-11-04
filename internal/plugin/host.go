package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// hostAuthEntry mirrors the host's auth file entry without importing the SDK.
type hostAuthEntry struct {
	ID          string `json:"id,omitempty"`
	AuthIndex   string `json:"auth_index,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Label       string `json:"label,omitempty"`
	Status      string `json:"status,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
	RuntimeOnly bool   `json:"runtime_only,omitempty"`
	Email       string `json:"email,omitempty"`
	Path        string `json:"path,omitempty"`
}

type hostAuthListResponse struct {
	Files []hostAuthEntry `json:"files"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

var (
	authCacheMu   sync.Mutex
	authCache     []hostAuthEntry
	authCacheTime time.Time
)

const authCacheTTL = 10 * time.Second

// listAuths returns the host's credential list, briefly cached because a burst
// of settlements would otherwise call into the host once per request.
func listAuths(force bool) ([]hostAuthEntry, error) {
	authCacheMu.Lock()
	if !force && time.Since(authCacheTime) < authCacheTTL && authCache != nil {
		cached := authCache
		authCacheMu.Unlock()
		return cached, nil
	}
	authCacheMu.Unlock()

	raw, errCall := invokeHost("host.auth.list", map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	var response hostAuthListResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host.auth.list: %w", errUnmarshal)
	}
	authCacheMu.Lock()
	authCache = response.Files
	authCacheTime = time.Now()
	authCacheMu.Unlock()
	return response.Files, nil
}

func invalidateAuthCache() {
	authCacheMu.Lock()
	authCacheTime = time.Time{}
	authCacheMu.Unlock()
}

// findAuth locates the host entry backing a ledger row.
func findAuth(key, authIndex string) (hostAuthEntry, bool) {
	entries, errList := listAuths(true)
	if errList != nil {
		return hostAuthEntry{}, false
	}
	for _, entry := range entries {
		if authIndex != "" && entry.AuthIndex == authIndex {
			return entry, true
		}
	}
	for _, entry := range entries {
		switch key {
		case entry.ID, entry.Name, entry.AuthIndex:
			return entry, true
		}
	}
	return hostAuthEntry{}, false
}

// readAuthJSON loads the physical credential JSON for one auth index.
func readAuthJSON(authIndex string) (hostAuthGetResponse, map[string]any, error) {
	raw, errCall := invokeHost("host.auth.get", map[string]any{"auth_index": authIndex})
	if errCall != nil {
		return hostAuthGetResponse{}, nil, errCall
	}
	var response hostAuthGetResponse
	if errUnmarshal := json.Unmarshal(raw, &response); errUnmarshal != nil {
		return hostAuthGetResponse{}, nil, fmt.Errorf("decode host.auth.get: %w", errUnmarshal)
	}
	if len(response.JSON) == 0 {
		return response, nil, errors.New("host.auth.get returned no credential JSON")
	}
	payload := map[string]any{}
	if errUnmarshal := json.Unmarshal(response.JSON, &payload); errUnmarshal != nil {
		return response, nil, fmt.Errorf("decode credential JSON: %w", errUnmarshal)
	}
	return response, payload, nil
}

// setAuthDisabled writes the credential back with the disabled flag flipped. The
// host applies the record to its runtime as well, so CPA's own routing stops
// selecting it without a restart.
func setAuthDisabled(entry hostAuthEntry, disabled bool) (bool, error) {
	if strings.TrimSpace(entry.AuthIndex) == "" {
		return false, errors.New("credential has no auth index")
	}
	response, payload, errRead := readAuthJSON(entry.AuthIndex)
	if errRead != nil {
		return false, errRead
	}
	current, hasCurrent := payload["disabled"].(bool)
	if hasCurrent && current == disabled {
		return false, nil
	}
	payload["disabled"] = disabled
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return false, fmt.Errorf("encode credential JSON: %w", errMarshal)
	}
	name := strings.TrimSpace(response.Name)
	if name == "" {
		name = strings.TrimSpace(entry.Name)
	}
	if name == "" || !strings.HasSuffix(name, ".json") {
		return false, fmt.Errorf("credential %q has no physical auth file to write", entry.ID)
	}
	if _, errCall := invokeHost("host.auth.save", map[string]any{"name": name, "json": json.RawMessage(encoded)}); errCall != nil {
		return false, errCall
	}
	invalidateAuthCache()
	return true, nil
}

// credentialLabel picks the most useful human label the host exposes.
func credentialLabel(entry hostAuthEntry) string {
	for _, candidate := range []string{entry.Label, entry.Email} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return ""
}

// syncCredentials refreshes the ledger's descriptive fields from the host and
// flags rows whose credential disappeared.
func syncCredentials(led *ledger) error {
	entries, errList := listAuths(true)
	if errList != nil {
		return errList
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		key := credentialKey(entry.ID, entry.AuthIndex)
		if key == "" {
			continue
		}
		seen[key] = true
		led.describe(key, entry.AuthIndex, entry.Provider, entry.Name, credentialLabel(entry))
	}
	led.markMissing(seen)
	return nil
}
