package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	stateVersion  = 1
	auditLimit    = 200
	flushInterval = 2 * time.Second
)

// Credential is one metered upstream credential (an auth record in CPA).
type Credential struct {
	// Key is the ledger key: the auth record ID, or the auth index when the
	// host reports no ID.
	Key string `json:"key"`
	// AuthIndex is the host's stable runtime credential index, needed to read
	// the physical auth file back.
	AuthIndex string `json:"auth_index,omitempty"`
	// Name is the auth file name (or runtime identifier) the host reports.
	Name string `json:"name,omitempty"`
	// Provider is the CPA provider key, e.g. claude or codex.
	Provider string `json:"provider,omitempty"`
	// Label is the human label the host reports (email, note, ...).
	Label string `json:"label,omitempty"`
	// LimitMicroUSD is the one-time budget in micro-USD. Zero means unlimited.
	LimitMicroUSD int64 `json:"limit_micro_usd"`
	// LimitSource is manual when an operator set the budget, default when it was
	// adopted from default_limit_usd, seed when the state was created empty.
	LimitSource string `json:"limit_source,omitempty"`
	// SpentMicroUSD is the credited spend over the credential's lifetime.
	SpentMicroUSD int64 `json:"spent_micro_usd"`
	// Requests counts settled requests that carried usage.
	Requests int64 `json:"requests"`
	// UnpricedRequests counts settled requests the price book could not price.
	UnpricedRequests int64 `json:"unpriced_requests"`
	// LastCostMicroUSD is the most recent settled request cost.
	LastCostMicroUSD int64 `json:"last_cost_micro_usd"`
	// LastRequestAt is the most recent settlement time.
	LastRequestAt *time.Time `json:"last_request_at,omitempty"`
	// Exhausted reports that the budget was consumed.
	Exhausted bool `json:"exhausted"`
	// ExhaustedAt records when the budget was consumed.
	ExhaustedAt *time.Time `json:"exhausted_at,omitempty"`
	// DisabledByPlugin reports that this plugin disabled the auth record. Only a
	// record carrying this flag is ever re-enabled by the plugin.
	DisabledByPlugin bool `json:"disabled_by_plugin"`
	// DisableError is the last failure while disabling the auth record.
	DisableError string `json:"disable_error,omitempty"`
	// Missing reports that the host no longer lists this credential.
	Missing bool `json:"missing,omitempty"`
	// UpdatedAt is the last write time for this row.
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

// effectiveLimit returns the budget in micro-USD, zero meaning unlimited.
func (c *Credential) effectiveLimit() int64 {
	if c == nil || c.LimitMicroUSD <= 0 {
		return 0
	}
	return c.LimitMicroUSD
}

// tripped reports whether the credential has consumed its budget.
func (c *Credential) tripped() bool {
	limit := c.effectiveLimit()
	return limit > 0 && c.SpentMicroUSD >= limit
}

// remaining returns the budget left in micro-USD, or -1 when unlimited.
func (c *Credential) remaining() int64 {
	limit := c.effectiveLimit()
	if limit <= 0 {
		return -1
	}
	left := limit - c.SpentMicroUSD
	if left < 0 {
		return 0
	}
	return left
}

type auditEntry struct {
	At     time.Time `json:"at"`
	Action string    `json:"action"`
	Key    string    `json:"key,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

type persistedState struct {
	Version     int                    `json:"version"`
	UpdatedAt   *time.Time             `json:"updated_at,omitempty"`
	Credentials map[string]*Credential `json:"credentials"`
	PriceBook   *priceBookState        `json:"price_book,omitempty"`
	Audit       []auditEntry           `json:"audit,omitempty"`
}

type ledger struct {
	mu           sync.Mutex
	path         string
	storageError string
	state        persistedState

	flushMu  sync.Mutex
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
	// onTrip runs after the lock is released when a credential crosses its
	// budget, so the caller can disable the auth record without holding the lock.
	onTrip func(cred Credential)
}

func newLedger(path string) *ledger {
	return &ledger{
		path:  path,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		state: persistedState{Version: stateVersion, Credentials: map[string]*Credential{}},
	}
}

func (l *ledger) load() {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, errRead := os.ReadFile(l.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return
	}
	if errRead != nil {
		l.storageError = fmt.Sprintf("账本读取失败，为保护原文件已暂停写入：%v", errRead)
		return
	}
	var decoded persistedState
	if errUnmarshal := json.Unmarshal(data, &decoded); errUnmarshal != nil {
		l.storageError = fmt.Sprintf("账本格式无效，为保护原文件已暂停写入：%v", errUnmarshal)
		return
	}
	if decoded.Version > stateVersion {
		l.storageError = fmt.Sprintf("账本版本 %d 高于本插件支持的 %d，已暂停写入", decoded.Version, stateVersion)
		return
	}
	if decoded.Credentials == nil {
		decoded.Credentials = map[string]*Credential{}
	}
	decoded.Version = stateVersion
	l.state = decoded
}

func (l *ledger) startFlusher() {
	go func() {
		defer close(l.done)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-l.stop:
				l.persist()
				return
			case <-ticker.C:
				l.persist()
			}
		}
	}()
}

func (l *ledger) close() {
	l.stopOnce.Do(func() { close(l.stop) })
	select {
	case <-l.done:
	case <-time.After(3 * time.Second):
	}
}

// persist writes the state atomically. A storage error keeps the previous file
// untouched and surfaces through the panel instead of destroying evidence.
func (l *ledger) persist() {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()

	l.mu.Lock()
	if l.storageError != "" {
		l.mu.Unlock()
		return
	}
	now := unixNow()
	l.state.Version = stateVersion
	l.state.UpdatedAt = &now
	l.state.Audit = trimAudit(l.state.Audit)
	raw, errMarshal := json.MarshalIndent(&l.state, "", "  ")
	l.mu.Unlock()
	if errMarshal != nil {
		return
	}

	dir := filepath.Dir(l.path)
	if dir != "" && dir != "." {
		if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
			l.setStorageError(fmt.Sprintf("账本目录创建失败：%v", errMkdir))
			return
		}
	}
	temp, errTemp := os.CreateTemp(dir, "."+filepath.Base(l.path)+".tmp-*")
	if errTemp != nil {
		l.setStorageError(fmt.Sprintf("账本临时文件创建失败：%v", errTemp))
		return
	}
	tempPath := temp.Name()
	if _, errWrite := temp.Write(raw); errWrite != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		l.setStorageError(fmt.Sprintf("账本写入失败：%v", errWrite))
		return
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		l.setStorageError(fmt.Sprintf("账本落盘失败：%v", errSync))
		return
	}
	if errClose := temp.Close(); errClose != nil {
		_ = os.Remove(tempPath)
		l.setStorageError(fmt.Sprintf("账本关闭失败：%v", errClose))
		return
	}
	if errRename := os.Rename(tempPath, l.path); errRename != nil {
		_ = os.Remove(tempPath)
		l.setStorageError(fmt.Sprintf("账本替换失败：%v", errRename))
	}
}

func (l *ledger) setStorageError(message string) {
	l.mu.Lock()
	l.storageError = message
	l.mu.Unlock()
}

func trimAudit(entries []auditEntry) []auditEntry {
	if len(entries) <= auditLimit {
		return entries
	}
	return entries[len(entries)-auditLimit:]
}

func (l *ledger) auditLocked(action, key, detail string) {
	l.state.Audit = append(l.state.Audit, auditEntry{At: unixNow(), Action: action, Key: key, Detail: detail})
}

// credentialKey picks the ledger key for one settled request.
func credentialKey(authID, authIndex string) string {
	if authID != "" {
		return authID
	}
	return authIndex
}

// settle records one settled request against a credential. It returns the row
// as it now stands and whether the credential crossed its budget in this call.
func (l *ledger) settle(key, authIndex, provider, label string, costMicro int64, unpriced bool, at time.Time, defaultLimitMicro int64) (Credential, bool) {
	l.mu.Lock()
	row, ok := l.state.Credentials[key]
	if !ok {
		now := at
		row = &Credential{
			Key:           key,
			AuthIndex:     authIndex,
			Provider:      provider,
			Label:         label,
			LimitMicroUSD: defaultLimitMicro,
			LimitSource:   limitSourceFor(defaultLimitMicro),
			UpdatedAt:     &now,
		}
		l.state.Credentials[key] = row
	}
	if row.AuthIndex == "" {
		row.AuthIndex = authIndex
	}
	if provider != "" {
		row.Provider = provider
	}
	if label != "" {
		row.Label = label
	}
	row.Missing = false
	if costMicro > 0 {
		row.SpentMicroUSD += costMicro
	}
	row.LastCostMicroUSD = costMicro
	if unpriced {
		row.UnpricedRequests++
	}
	row.Requests++
	stamp := at
	row.LastRequestAt = &stamp
	row.UpdatedAt = &stamp

	trippedNow := false
	if !row.Exhausted && row.tripped() {
		row.Exhausted = true
		row.ExhaustedAt = &stamp
		trippedNow = true
		l.auditLocked("budget_exhausted", key, fmt.Sprintf("spent=%s limit=%s", formatUSD(row.SpentMicroUSD), formatUSD(row.LimitMicroUSD)))
	}
	snapshot := *row
	onTrip := l.onTrip
	l.mu.Unlock()

	if trippedNow && onTrip != nil {
		onTrip(snapshot)
	}
	return snapshot, trippedNow
}

func limitSourceFor(limitMicro int64) string {
	if limitMicro > 0 {
		return "default"
	}
	return ""
}

// setLimit stores an operator-provided budget. Zero means unlimited.
func (l *ledger) setLimit(key string, limitMicro int64) (Credential, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.state.Credentials[key]
	if !ok {
		return Credential{}, false, fmt.Errorf("unknown credential %q", key)
	}
	previousLimit := row.LimitMicroUSD
	row.LimitMicroUSD = limitMicro
	row.LimitSource = "manual"
	now := unixNow()
	row.UpdatedAt = &now
	l.auditLocked("limit_set", key, fmt.Sprintf("%s -> %s", formatUSD(previousLimit), formatUSD(limitMicro)))
	needsUnseal := row.Exhausted && row.effectiveLimit() > 0 && row.SpentMicroUSD < row.effectiveLimit()
	return *row, needsUnseal, nil
}

// unseal clears the spend counter and releases the plugin's own disable flag.
// It reports whether the plugin had disabled the auth record, so the caller can
// re-enable it.
func (l *ledger) unseal(key string) (Credential, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.state.Credentials[key]
	if !ok {
		return Credential{}, false, fmt.Errorf("unknown credential %q", key)
	}
	wasDisabled := row.DisabledByPlugin
	now := unixNow()
	detail := fmt.Sprintf("spent %s reset to 0", formatUSD(row.SpentMicroUSD))
	row.SpentMicroUSD = 0
	row.Exhausted = false
	row.ExhaustedAt = nil
	row.DisabledByPlugin = false
	row.DisableError = ""
	row.UpdatedAt = &now
	l.auditLocked("unseal", key, detail)
	return *row, wasDisabled, nil
}

// markDisabled records the outcome of the host auth write. wrote reports
// whether the plugin actually flipped the credential to disabled; when the
// credential was already disabled the plugin leaves the operator's own flag
// alone and never claims ownership of it.
func (l *ledger) markDisabled(key string, wrote bool, errMessage string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, exists := l.state.Credentials[key]
	if !exists {
		return
	}
	now := unixNow()
	row.UpdatedAt = &now
	if errMessage != "" {
		row.DisableError = errMessage
		l.auditLocked("auth_disable_failed", key, errMessage)
		return
	}
	row.DisableError = ""
	if wrote {
		row.DisabledByPlugin = true
		l.auditLocked("auth_disabled", key, "host.auth.save disabled the credential")
		return
	}
	row.DisabledByPlugin = false
	l.auditLocked("auth_already_disabled", key, "credential was already disabled by the operator")
}

// describe refreshes the host-reported identity fields of a row.
func (l *ledger) describe(key, authIndex, provider, name, label string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, exists := l.state.Credentials[key]
	if !exists {
		return
	}
	if authIndex != "" {
		row.AuthIndex = authIndex
	}
	if provider != "" {
		row.Provider = provider
	}
	if name != "" {
		row.Name = name
	}
	if label != "" {
		row.Label = label
	}
}

// markEnabled records that the plugin re-enabled the auth record.
func (l *ledger) markEnabled(key string, errMessage string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, exists := l.state.Credentials[key]
	if !exists {
		return
	}
	now := unixNow()
	row.UpdatedAt = &now
	if errMessage != "" {
		row.DisableError = errMessage
		l.auditLocked("auth_enable_failed", key, errMessage)
		return
	}
	row.DisabledByPlugin = false
	row.DisableError = ""
	l.auditLocked("auth_enabled", key, "host.auth.save re-enabled the credential")
}

// remove drops a row that no longer maps to a credential.
func (l *ledger) remove(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.state.Credentials[key]; !ok {
		return fmt.Errorf("unknown credential %q", key)
	}
	delete(l.state.Credentials, key)
	l.auditLocked("row_removed", key, "ledger row removed by operator")
	return nil
}

// markMissing flags rows the host no longer lists.
func (l *ledger) markMissing(seen map[string]bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, row := range l.state.Credentials {
		missing := !seen[key]
		if row.Missing != missing {
			row.Missing = missing
		}
	}
}

// exhaustedKeys returns the ledger keys currently over budget. The scheduler
// uses it as a safety net for credentials the host would not let us disable.
func (l *ledger) exhaustedKeys() map[string]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	keys := make(map[string]bool, len(l.state.Credentials))
	for key, row := range l.state.Credentials {
		if row.Exhausted {
			keys[key] = true
		}
	}
	return keys
}

// rows returns a stable copy of every credential row.
func (l *ledger) rows() []Credential {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := make([]Credential, 0, len(l.state.Credentials))
	for _, row := range l.state.Credentials {
		rows = append(rows, *row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

func (l *ledger) row(key string) (Credential, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.state.Credentials[key]
	if !ok {
		return Credential{}, false
	}
	return *row, true
}

func (l *ledger) storage(errMessage string) {
	if errMessage != "" {
		l.setStorageError(errMessage)
	}
}

func (l *ledger) storageStatus() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.storageError
}

func (l *ledger) auditTrail(limit int) []auditEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := l.state.Audit
	if limit > 0 && len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	out := make([]auditEntry, len(entries))
	copy(out, entries)
	return out
}

func (l *ledger) priceBook() *priceBookState {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.state.PriceBook
}

func (l *ledger) storePriceBook(book *priceBookState) {
	l.mu.Lock()
	l.state.PriceBook = book
	l.mu.Unlock()
}

func usdToMicro(usd float64) int64 {
	if usd <= 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		return 0
	}
	return int64(math.Round(usd * 1e6))
}

func microToUSD(micro int64) float64 { return float64(micro) / 1e6 }

func formatUSD(micro int64) string {
	if micro == 0 {
		return "$0"
	}
	return fmt.Sprintf("$%.6f", microToUSD(micro))
}
