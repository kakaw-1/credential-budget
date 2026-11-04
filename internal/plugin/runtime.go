package plugin

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

var (
	rtMu      sync.Mutex
	current   *runtimeState
	refresher *priceBookRefresher
)

// startupSyncDelay gives the host time to register its auth records before the
// plugin asks for them.
const startupSyncDelay = 2 * time.Second

// applyConfig installs (or reinstalls) the runtime for one configuration block.
func applyConfig(cfg Config, firstLoad bool) error {
	rtMu.Lock()
	defer rtMu.Unlock()

	var led *ledger
	switch {
	case current == nil:
		led = newLedger(cfg.StateFile)
		led.load()
		led.startFlusher()
	case current.cfg.StateFile != cfg.StateFile:
		// A path change starts a new ledger; the old file is left untouched.
		current.ledger.close()
		led = newLedger(cfg.StateFile)
		led.load()
		led.startFlusher()
	default:
		led = current.ledger
	}
	if refresher != nil {
		refresher.close()
		refresher = nil
	}
	rt := &runtimeState{cfg: cfg, ledger: led}
	led.onTrip = func(cred Credential) { tripCredential(rt, cred) }
	current = rt
	refresher = startPriceRefresh(rt)
	if !firstLoad {
		appendLog("configuration reloaded")
	}
	// The host registers its credentials shortly after the plugin loads, so the
	// first enumeration is deferred instead of racing the auth manager. Without
	// it a provider only appears in the panel after it has served a request.
	go func() {
		time.Sleep(startupSyncDelay)
		if runtime() != rt {
			return
		}
		if errSync := syncCredentials(rt.ledger, rt.cfg); errSync != nil {
			if rt.cfg.Debug {
				appendLog("startup enumeration failed: " + errSync.Error())
			}
			return
		}
		rt.ledger.persist()
		if rt.cfg.Debug {
			appendLog(fmt.Sprintf("startup enumeration: %d credential(s) known to the runtime", len(rt.ledger.rows())))
		}
	}()
	return nil
}

func runtime() *runtimeState {
	rtMu.Lock()
	defer rtMu.Unlock()
	return current
}

// Quiesce flushes state and stops background work before the host unloads us.
func Quiesce() {
	rtMu.Lock()
	rt := current
	current = nil
	book := refresher
	refresher = nil
	rtMu.Unlock()
	if book != nil {
		book.close()
	}
	if rt != nil {
		rt.ledger.close()
	}
}

// appendLog forwards a message to CPA's log stream.
func appendLog(message string) {
	_, _ = invokeHost("host.log", map[string]any{
		"level":   "info",
		"message": "[" + PluginID + "] " + message,
		"fields":  map[string]any{"plugin_id": PluginID},
	})
}

// tripCredential stops a credential that just consumed its budget.
func tripCredential(rt *runtimeState, cred Credential) {
	if rt == nil {
		return
	}
	appendLog(fmt.Sprintf("credential %s exhausted its budget (spent %s of %s)", cred.Key, formatUSD(cred.SpentMicroUSD), formatUSD(cred.LimitMicroUSD)))
	if !rt.cfg.enforceEnabled() {
		appendLog("enforce is off: metering only, credential stays in rotation")
		return
	}
	if !rt.cfg.autoDisableEnabled() {
		appendLog("auto_disable is off: the scheduler veto keeps the credential out of rotation")
		return
	}
	entry, ok := findAuth(cred.Key, cred.AuthIndex)
	if !ok {
		rt.ledger.markDisabled(cred.Key, false, "host.auth.list does not contain this credential")
		appendLog("cannot disable: credential not found in host.auth.list")
		return
	}
	wrote, errDisable := setAuthDisabled(entry, true)
	if errDisable != nil {
		rt.ledger.markDisabled(cred.Key, false, errDisable.Error())
		appendLog("cannot disable auth record: " + errDisable.Error())
		rt.ledger.persist()
		return
	}
	rt.ledger.markDisabled(cred.Key, wrote, "")
	rt.ledger.persist()
	if wrote {
		appendLog("auth record disabled: " + entry.Name)
	} else {
		appendLog("auth record was already disabled")
	}
}

// enableCredential re-enables an auth record this plugin disabled.
func enableCredential(rt *runtimeState, key string) error {
	if rt == nil {
		return fmt.Errorf("plugin is not configured")
	}
	row, ok := rt.ledger.row(key)
	if !ok {
		return fmt.Errorf("unknown credential %q", key)
	}
	entry, found := findAuth(row.Key, row.AuthIndex)
	if !found {
		rt.ledger.markEnabled(key, "")
		return nil
	}
	wrote, errEnable := setAuthDisabled(entry, false)
	if errEnable != nil {
		rt.ledger.markEnabled(key, errEnable.Error())
		return errEnable
	}
	rt.ledger.markEnabled(key, "")
	rt.ledger.persist()
	if wrote {
		appendLog("auth record re-enabled: " + entry.Name)
	}
	return nil
}

// hostReply is used by tests to fake an envelope.
func hostReply(payload any) json.RawMessage {
	raw, _ := json.Marshal(payload)
	return raw
}

var _ = time.Second
