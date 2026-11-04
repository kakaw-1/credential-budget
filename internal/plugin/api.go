package plugin

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

//go:embed panel.html
var panelHTML []byte

const (
	routeState     = "/state"
	routeLimits    = "/limits"
	routeUnseal    = "/unseal"
	routeUnsealAll = "/unseal-all"
	routeSync      = "/sync"
	routePriceSync = "/price/sync"
	routeRow       = "/row"
	routeAlias     = "/alias"
)

type managementRequest struct {
	Method  string
	Path    string
	Query   url.Values
	Body    []byte
	Headers http.Header
}

type managementResponse struct {
	StatusCode int         `json:"StatusCode"`
	Headers    http.Header `json:"Headers,omitempty"`
	Body       []byte      `json:"Body,omitempty"`
}

type managementRouteView struct {
	Method      string `json:"Method"`
	Path        string `json:"Path"`
	Menu        string `json:"Menu,omitempty"`
	Description string `json:"Description,omitempty"`
}

type managementRegistrationView struct {
	Routes    []managementRouteView `json:"routes"`
	Resources []managementRouteView `json:"resources"`
}

func managementRegistration() managementRegistrationView {
	return managementRegistrationView{
		Routes: []managementRouteView{
			{Method: http.MethodGet, Path: ManagementBase + routeState, Description: "凭证额度账本与价目表状态"},
			{Method: http.MethodPost, Path: ManagementBase + routeLimits, Description: "设置单条凭证的一次性额度"},
			{Method: http.MethodPost, Path: ManagementBase + routeAlias, Description: "给凭证起一个自己认得的备注名"},
			{Method: http.MethodPost, Path: ManagementBase + routeUnseal, Description: "手动解封：清零已用并重新启用该凭证"},
			{Method: http.MethodPost, Path: ManagementBase + routeUnsealAll, Description: "解封全部已停用凭证"},
			{Method: http.MethodPost, Path: ManagementBase + routeSync, Description: "从 host.auth.list 同步凭证清单"},
			{Method: http.MethodPost, Path: ManagementBase + routePriceSync, Description: "立即刷新价目表"},
			{Method: http.MethodDelete, Path: ManagementBase + routeRow, Description: "删除已消失凭证的账本行"},
		},
		Resources: []managementRouteView{
			{Path: UIPath, Menu: "凭证额度", Description: "按上游凭证限制一次性美元额度，达到即停止该凭证"},
		},
	}
}

func handleManagement(req managementRequest) managementResponse {
	path := strings.TrimRight(strings.TrimSpace(req.Path), "/")
	switch {
	case req.Method == http.MethodGet && (path == UIPath || path == ManagementBase+"/ui"):
		return htmlResponse(panelHTML)
	case req.Method == http.MethodGet && path == ManagementBase+routeState:
		return stateResponse()
	case req.Method == http.MethodPost && path == ManagementBase+routeLimits:
		return limitResponse(req.Body)
	case req.Method == http.MethodPost && path == ManagementBase+routeAlias:
		return aliasResponse(req.Body)
	case req.Method == http.MethodPost && path == ManagementBase+routeUnseal:
		return unsealResponse(req.Body)
	case req.Method == http.MethodPost && path == ManagementBase+routeUnsealAll:
		return unsealAllResponse()
	case req.Method == http.MethodPost && path == ManagementBase+routeSync:
		return syncResponse()
	case req.Method == http.MethodPost && path == ManagementBase+routePriceSync:
		return priceSyncResponse()
	case req.Method == http.MethodDelete && path == ManagementBase+routeRow:
		return removeRowResponse(req)
	default:
		return jsonError(http.StatusNotFound, "route not found: "+req.Method+" "+req.Path)
	}
}

func htmlResponse(body []byte) managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {"text/html; charset=utf-8"},
			"Cache-Control":           {"no-store"},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		},
		Body: body,
	}
}

func jsonResponse(status int, payload any) managementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		body = []byte(`{"error":"encode response failed"}`)
		status = http.StatusInternalServerError
	}
	return managementResponse{
		StatusCode: status,
		Headers: http.Header{
			"Content-Type":  {"application/json; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: body,
	}
}

func jsonError(status int, message string) managementResponse {
	return jsonResponse(status, map[string]any{"error": message})
}

type credentialView struct {
	Key              string     `json:"key"`
	Name             string     `json:"name,omitempty"`
	Alias            string     `json:"alias,omitempty"`
	Display          string     `json:"display"`
	Tail             string     `json:"tail,omitempty"`
	BaseURL          string     `json:"base_url,omitempty"`
	LastModel        string     `json:"last_model,omitempty"`
	LastTier         string     `json:"last_tier,omitempty"`
	Provider         string     `json:"provider,omitempty"`
	Label            string     `json:"label,omitempty"`
	LimitUSD         float64    `json:"limit_usd"`
	SpentUSD         float64    `json:"spent_usd"`
	RemainingUSD     float64    `json:"remaining_usd"`
	UsageRatio       float64    `json:"usage_ratio"`
	Requests         int64      `json:"requests"`
	UnpricedRequests int64      `json:"unpriced_requests"`
	LastCostUSD      float64    `json:"last_cost_usd"`
	Exhausted        bool       `json:"exhausted"`
	ExhaustedAt      *time.Time `json:"exhausted_at,omitempty"`
	DisabledByPlugin bool       `json:"disabled_by_plugin"`
	DisableError     string     `json:"disable_error,omitempty"`
	Missing          bool       `json:"missing"`
	LimitSource      string     `json:"limit_source,omitempty"`
	Status           string     `json:"status"`
	// ObservedOnly marks a credential that the host offered for routing but that
	// has never settled a request: its spend is a real zero, not a missing value.
	ObservedOnly  bool       `json:"observed_only"`
	LastRequestAt *time.Time `json:"last_request_at,omitempty"`
}

// displayName is what the operator reads first: their own alias, then the host
// label (an email for OAuth accounts), then the credential file name, and only
// then the runtime id, which for config-backed keys is an opaque hash.
func displayName(row Credential) string {
	for _, candidate := range []string{row.Alias, row.Label, row.Name} {
		if strings.TrimSpace(candidate) != "" {
			return strings.TrimSpace(candidate)
		}
	}
	return row.Key
}

// idTail is the last six characters of the credential id: enough to tell two
// sibling keys of the same provider apart when they have no other name.
func idTail(row Credential) string {
	identifier := strings.TrimSpace(row.AuthIndex)
	if identifier == "" {
		identifier = row.Key
	}
	if len(identifier) <= 6 {
		return identifier
	}
	return identifier[len(identifier)-6:]
}

func toView(row Credential) credentialView {
	view := credentialView{
		Key:              row.Key,
		Name:             row.Name,
		Alias:            row.Alias,
		Display:          displayName(row),
		Tail:             idTail(row),
		BaseURL:          row.BaseURL,
		LastModel:        row.Model,
		LastTier:         row.LastTier,
		Provider:         row.Provider,
		Label:            row.Label,
		LimitUSD:         microToUSD(row.effectiveLimit()),
		SpentUSD:         microToUSD(row.SpentMicroUSD),
		Requests:         row.Requests,
		UnpricedRequests: row.UnpricedRequests,
		LastCostUSD:      microToUSD(row.LastCostMicroUSD),
		Exhausted:        row.Exhausted,
		ExhaustedAt:      row.ExhaustedAt,
		DisabledByPlugin: row.DisabledByPlugin,
		DisableError:     row.DisableError,
		Missing:          row.Missing,
		LimitSource:      row.LimitSource,
		ObservedOnly:     row.Requests == 0,
		LastRequestAt:    row.LastRequestAt,
	}
	if remaining := row.remaining(); remaining >= 0 {
		view.RemainingUSD = microToUSD(remaining)
	}
	limit := row.effectiveLimit()
	if limit > 0 {
		view.UsageRatio = float64(row.SpentMicroUSD) / float64(limit)
		if view.UsageRatio > 1 {
			view.UsageRatio = 1
		}
	}
	switch {
	case row.Exhausted:
		view.Status = "exhausted"
	case limit > 0:
		view.Status = "limited"
	default:
		view.Status = "unlimited"
	}
	return view
}

func stateResponse() managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	rows := rt.ledger.rows()
	views := make([]credentialView, 0, len(rows))
	var spent int64
	limited, exhausted := 0, 0
	type groupTotals struct {
		provider  string
		count     int
		limited   int
		exhausted int
		spent     int64
	}
	groups := make(map[string]*groupTotals)
	groupOrder := make([]string, 0)
	for _, row := range rows {
		views = append(views, toView(row))
		spent += row.SpentMicroUSD
		if row.effectiveLimit() > 0 {
			limited++
		}
		if row.Exhausted {
			exhausted++
		}
		key := row.Provider
		if key == "" {
			key = "未标注供应商"
		}
		group, ok := groups[key]
		if !ok {
			group = &groupTotals{provider: key}
			groups[key] = group
			groupOrder = append(groupOrder, key)
		}
		group.count++
		group.spent += row.SpentMicroUSD
		if row.effectiveLimit() > 0 {
			group.limited++
		}
		if row.Exhausted {
			group.exhausted++
		}
	}
	sortStrings(groupOrder)
	groupViews := make([]map[string]any, 0, len(groupOrder))
	for _, name := range groupOrder {
		group := groups[name]
		groupViews = append(groupViews, map[string]any{
			"provider":  group.provider,
			"count":     group.count,
			"limited":   group.limited,
			"exhausted": group.exhausted,
			"spent_usd": microToUSD(group.spent),
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"credentials": views,
		"groups":      groupViews,
		"totals": map[string]any{
			"credentials": len(views),
			"limited":     limited,
			"exhausted":   exhausted,
			"spent_usd":   microToUSD(spent),
		},
		"config":        rt.cfg.publicView(),
		"price_book":    rt.ledger.priceBook().summary(),
		"audit":         rt.ledger.auditTrail(40),
		"storage_error": rt.ledger.storageStatus(),
		"now":           unixNow(),
	})
}

type limitRequest struct {
	Key      string   `json:"key"`
	LimitUSD *float64 `json:"limit_usd"`
}

func limitResponse(body []byte) managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	var req limitRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return jsonError(http.StatusBadRequest, "请求格式无效："+errUnmarshal.Error())
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		return jsonError(http.StatusBadRequest, "缺少 key")
	}
	if req.LimitUSD == nil {
		return jsonError(http.StatusBadRequest, "缺少 limit_usd")
	}
	if *req.LimitUSD < 0 {
		return jsonError(http.StatusBadRequest, "额度不能为负数")
	}

	row, needsUnseal, errLimit := rt.ledger.setLimit(key, usdToMicro(*req.LimitUSD))
	if errLimit != nil {
		return jsonError(http.StatusNotFound, errLimit.Error())
	}
	rt.ledger.persist()
	return jsonResponse(http.StatusOK, map[string]any{
		"credential":      toView(row),
		"requires_unseal": needsUnseal,
	})
}

type aliasRequest struct {
	Key   string `json:"key"`
	Alias string `json:"alias"`
}

func aliasResponse(body []byte) managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	var req aliasRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return jsonError(http.StatusBadRequest, "请求格式无效："+errUnmarshal.Error())
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		return jsonError(http.StatusBadRequest, "缺少 key")
	}
	row, errAlias := rt.ledger.setAlias(key, req.Alias)
	if errAlias != nil {
		return jsonError(http.StatusNotFound, errAlias.Error())
	}
	rt.ledger.persist()
	return jsonResponse(http.StatusOK, map[string]any{"credential": toView(row)})
}

type unsealRequest struct {
	Key string `json:"key"`
}

func unsealResponse(body []byte) managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	var req unsealRequest
	if errUnmarshal := json.Unmarshal(body, &req); errUnmarshal != nil {
		return jsonError(http.StatusBadRequest, "请求格式无效："+errUnmarshal.Error())
	}
	key := strings.TrimSpace(req.Key)
	if key == "" {
		return jsonError(http.StatusBadRequest, "缺少 key")
	}
	row, wasDisabled, errUnseal := rt.ledger.unseal(key)
	if errUnseal != nil {
		return jsonError(http.StatusNotFound, errUnseal.Error())
	}
	if wasDisabled {
		if errEnable := enableCredential(rt, key); errEnable != nil {
			rt.ledger.persist()
			return jsonResponse(http.StatusOK, map[string]any{
				"credential": toView(row),
				"warning":    "额度已清零，但重新启用凭证失败：" + errEnable.Error(),
			})
		}
		row, _ = rt.ledger.row(key)
	}
	rt.ledger.persist()
	return jsonResponse(http.StatusOK, map[string]any{"credential": toView(row)})
}

func unsealAllResponse() managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	rows := rt.ledger.rows()
	unsealed := make([]string, 0, len(rows))
	warnings := make([]string, 0)
	for _, row := range rows {
		if !row.Exhausted && !row.DisabledByPlugin {
			continue
		}
		_, wasDisabled, errUnseal := rt.ledger.unseal(row.Key)
		if errUnseal != nil {
			warnings = append(warnings, row.Key+": "+errUnseal.Error())
			continue
		}
		if wasDisabled {
			if errEnable := enableCredential(rt, row.Key); errEnable != nil {
				warnings = append(warnings, row.Key+": "+errEnable.Error())
				continue
			}
		}
		unsealed = append(unsealed, row.Key)
	}
	rt.ledger.persist()
	return jsonResponse(http.StatusOK, map[string]any{"unsealed": unsealed, "warnings": warnings})
}

func syncResponse() managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	if errSync := syncCredentials(rt.ledger, rt.cfg); errSync != nil {
		return jsonError(http.StatusBadGateway, "同步凭证失败："+errSync.Error())
	}
	rt.ledger.persist()
	return stateResponse()
}

func priceSyncResponse() managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	if errRefresh := refreshPriceBook(rt.ledger, rt.cfg, nil, true); errRefresh != nil {
		return jsonError(http.StatusBadGateway, "刷新价目表失败："+errRefresh.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"price_book": rt.ledger.priceBook().summary()})
}

func removeRowResponse(req managementRequest) managementResponse {
	rt := runtime()
	if rt == nil {
		return jsonError(http.StatusServiceUnavailable, "插件尚未初始化")
	}
	key := strings.TrimSpace(req.Query.Get("key"))
	if key == "" && len(req.Body) > 0 {
		var body struct {
			Key string `json:"key"`
		}
		if errUnmarshal := json.Unmarshal(req.Body, &body); errUnmarshal == nil {
			key = strings.TrimSpace(body.Key)
		}
	}
	if key == "" {
		return jsonError(http.StatusBadRequest, "缺少 key")
	}
	if errRemove := rt.ledger.remove(key); errRemove != nil {
		return jsonError(http.StatusNotFound, errRemove.Error())
	}
	rt.ledger.persist()
	return jsonResponse(http.StatusOK, map[string]any{"removed": key})
}

var _ = fmt.Sprintf
