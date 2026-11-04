package plugin

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// schedulerCandidate mirrors one SchedulerAuthCandidate from the host.
type schedulerCandidate struct {
	ID         string
	Provider   string
	Priority   int
	Status     string
	Attributes map[string]string
	Metadata   map[string]any
}

type schedulerPickRequest struct {
	Provider   string
	Providers  []string
	Model      string
	Stream     bool
	Candidates []schedulerCandidate
}

type schedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
	Reject          bool   `json:"Reject,omitempty"`
	RejectReason    string `json:"RejectReason,omitempty"`
	RejectCode      string `json:"RejectCode,omitempty"`
}

// handlePick is the safety net, not the primary mechanism.
//
// The primary stop is host.auth.save flipping the credential's disabled flag:
// CPA's own routers then skip it with no further help from us. Some credentials
// cannot be written that way (runtime-only records, or a failed write). When the
// host offers candidates that our ledger already marks exhausted, this veto keeps
// them out of this request: we pick the highest-priority remaining candidate
// instead, and when nothing remains we stay unhandled so the host's own routing
// (including other providers) still decides.
func handlePick(request []byte) []byte {
	rt := runtime()
	if rt == nil {
		return okEnvelope(schedulerPickResponse{})
	}
	var req schedulerPickRequest
	if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
		return okEnvelope(schedulerPickResponse{})
	}
	if len(req.Candidates) == 0 {
		return okEnvelope(schedulerPickResponse{})
	}
	// Every routed request reveals the rest of the provider's credentials. A
	// config-backed key has no auth file, so this candidate list is the only
	// place it can be discovered before it has ever served a request.
	registerCandidates(rt, req)
	exhausted := rt.ledger.exhaustedKeys()
	if rt.cfg.Debug {
		appendLog(fmt.Sprintf("scheduler.pick: provider=%s model=%s candidates=%d exhausted_rows=%d", req.Provider, req.Model, len(req.Candidates), len(exhausted)))
	}
	if len(exhausted) == 0 {
		return okEnvelope(schedulerPickResponse{})
	}

	blocked := 0
	available := make([]schedulerCandidate, 0, len(req.Candidates))
	for _, candidate := range req.Candidates {
		if candidateExhausted(candidate, exhausted) {
			blocked++
			continue
		}
		available = append(available, candidate)
	}
	if blocked == 0 {
		return okEnvelope(schedulerPickResponse{})
	}
	if len(available) == 0 {
		// Every candidate offered for this route is over budget. Either answer
		// the operator's budget honestly, or hand the pick back to the host.
		if rt.cfg.rejectsWhenAllExhausted() {
			appendLog(fmt.Sprintf("scheduler.pick: all %d candidates for provider %s are over budget", blocked, req.Provider))
			return okEnvelope(schedulerPickResponse{
				Handled:      true,
				Reject:       true,
				RejectCode:   "credential_budget_exhausted",
				RejectReason: "all credentials for this route have consumed their one-time budget",
			})
		}
		return okEnvelope(schedulerPickResponse{})
	}
	sort.SliceStable(available, func(i, j int) bool {
		return available[i].Priority > available[j].Priority
	})
	for _, candidate := range available {
		if strings.TrimSpace(candidate.ID) == "" {
			continue
		}
		if rt.cfg.Debug {
			appendLog(fmt.Sprintf("scheduler.pick: steering away from %d over-budget candidate(s), picked %s", blocked, candidate.ID))
		}
		return okEnvelope(schedulerPickResponse{AuthID: candidate.ID, Handled: true})
	}
	return okEnvelope(schedulerPickResponse{})
}

func candidateExhausted(candidate schedulerCandidate, exhausted map[string]bool) bool {
	if candidate.ID != "" && exhausted[candidate.ID] {
		return true
	}
	for _, key := range []string{"name", "file_name", "auth_index"} {
		if value := strings.TrimSpace(candidate.Attributes[key]); value != "" && exhausted[value] {
			return true
		}
	}
	return false
}

// registerCandidates records every credential the host offered for this route,
// so a provider's whole key set shows up in the panel after the first request
// instead of only the keys that happened to serve one.
func registerCandidates(rt *runtimeState, req schedulerPickRequest) {
	created := 0
	for _, candidate := range req.Candidates {
		key := strings.TrimSpace(candidate.ID)
		if key == "" {
			continue
		}
		provider := firstNonEmpty(candidate.Provider, req.Provider)
		limitMicro, limitSource := rt.cfg.defaultLimitFor(provider)
		if rt.ledger.observe(key, strings.TrimSpace(candidate.Attributes["auth_index"]), provider, strings.TrimSpace(candidate.Attributes["base_url"]), limitMicro, limitSource) {
			created++
		}
	}
	if created > 0 {
		rt.ledger.persist()
		if rt.cfg.Debug {
			appendLog(fmt.Sprintf("scheduler.pick: registered %d new credential(s) from the candidate list", created))
		}
	}
}
