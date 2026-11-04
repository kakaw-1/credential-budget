// Package plugin implements a CLIProxyAPI plugin that meters upstream credential
// spend and stops a credential once its one-time budget is consumed.
package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// PluginID is the stable plugin identifier. The shared library file name,
	// the plugins.configs key and the management routes all derive from it.
	PluginID = "credential-budget"
	// PluginVersion is the release version. A tag must match it.
	PluginVersion = "0.1.2"
	// ABIVersion is the native C ABI version this plugin was built against.
	ABIVersion = 1
	// SchemaVersion is the RPC JSON contract version. 10 is the newest CPA
	// contract this plugin was written against; older hosts read the fields
	// they know.
	SchemaVersion = 10

	// ManagementBase is the only Management API prefix plugins may register under.
	ManagementBase = "/v0/management/plugins/" + PluginID
	// UIPath is the browser-navigable panel route.
	UIPath = "/v0/resource/plugins/" + PluginID + "/ui"
)

// hostCall is injected by the C ABI shim (or by tests).
var hostCall func(method string, payload any) (json.RawMessage, error)

// SetHostCall wires the native host callback into the plugin package.
func SetHostCall(call func(method string, payload any) (json.RawMessage, error)) {
	hostMu.Lock()
	hostCall = call
	hostMu.Unlock()
}

var hostMu sync.RWMutex

func invokeHost(method string, payload any) (json.RawMessage, error) {
	hostMu.RLock()
	call := hostCall
	hostMu.RUnlock()
	if call == nil {
		return nil, errors.New("host callback is not available")
	}
	return call(method, payload)
}

// Envelope is the JSON-RPC reply the host decodes.
type Envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *EnvelopeError  `json:"error,omitempty"`
}

// EnvelopeError carries the plugin error plus the HTTP status the host should
// surface to the client.
type EnvelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *EnvelopeError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// ErrorEnvelope builds a failed reply.
func ErrorEnvelope(code, message string, httpStatus ...int) []byte {
	status := 0
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	raw, _ := json.Marshal(Envelope{OK: false, Error: &EnvelopeError{Code: code, Message: message, HTTPStatus: status}})
	return raw
}

func okEnvelope(v any) []byte {
	result, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return ErrorEnvelope("plugin_error", errMarshal.Error(), http.StatusInternalServerError)
	}
	raw, _ := json.Marshal(Envelope{OK: true, Result: result})
	return raw
}

// DecodeHostReply turns a host callback reply into its result or an error.
func DecodeHostReply(method string, code int, raw []byte) (json.RawMessage, error) {
	name := strings.TrimSpace(method)
	if len(raw) == 0 {
		if code != 0 {
			return nil, fmt.Errorf("%s: host callback failed with code %d", name, code)
		}
		return json.RawMessage("{}"), nil
	}
	var envelope Envelope
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		return nil, fmt.Errorf("%s: invalid host reply: %w", name, errUnmarshal)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("%s: %s", name, envelope.Error.Error())
	}
	if code != 0 {
		return nil, fmt.Errorf("%s: host callback failed with code %d", name, code)
	}
	if len(envelope.Result) == 0 {
		return json.RawMessage("{}"), nil
	}
	return envelope.Result, nil
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      metadata               `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type metadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Default     any    `json:"Default,omitempty"`
	EnumValues  []string
	Description string `json:"Description"`
}

type registrationCapability struct {
	UsagePlugin   bool `json:"usage_plugin"`
	ManagementAPI bool `json:"management_api"`
	Scheduler     bool `json:"scheduler"`
}

func (c configField) MarshalJSON() ([]byte, error) {
	payload := map[string]any{
		"Name":        c.Name,
		"Type":        c.Type,
		"Description": c.Description,
	}
	if c.Default != nil {
		payload["Default"] = c.Default
	}
	if len(c.EnumValues) > 0 {
		payload["EnumValues"] = c.EnumValues
	}
	return json.Marshal(payload)
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: SchemaVersion,
		Metadata: metadata{
			Name:             PluginID,
			Version:          PluginVersion,
			Author:           "kakaw-1",
			GitHubRepository: "https://github.com/kakaw-1/credential-budget",
			Logo:             logoSVG,
			ConfigFields: []configField{
				{Name: "default_limit_usd", Type: "number", Default: 0, Description: "新建凭证的默认一次性额度（美元）。0 表示不限额，仅在面板单独设置时生效。"},
				{Name: "enforce", Type: "boolean", Default: true, Description: "达到额度后停止调度该凭证。关闭后只记账不熔断。"},
				{Name: "auto_disable", Type: "boolean", Default: true, Description: "达到额度时通过 host.auth.save 将该凭证置为 disabled，让 CPA 自身路由绕开它。"},
				{Name: "price_source", Type: "enum", Default: "openrouter", EnumValues: []string{"openrouter", "off"}, Description: "单价来源。openrouter 使用其公开模型价目表；off 仅用配置里的 price_overrides。"},
				{Name: "unpriced_policy", Type: "enum", Default: "fallback", EnumValues: []string{"fallback", "skip"}, Description: "价目表查不到模型时的处理：fallback 按兜底价计费，skip 不计费但计数。"},
				{Name: "on_all_exhausted", Type: "enum", Default: "reject", EnumValues: []string{"reject", "pass"}, Description: "该路由上所有凭证都爆额时：reject 直接返回额度不足，pass 交回网关继续调度。"},
				{Name: "debug", Type: "boolean", Default: false, Description: "记录每次调度决策，排查用。"},
				{Name: "provider_limits", Type: "object", Description: "按供应商给新出现的凭证指定默认额度，例如 {claude: 5}。只影响之后新记账的凭证。"},
				{Name: "state_file", Type: "string", Default: "plugins/" + PluginID + "-state.json", Description: "账本与价目表缓存文件，相对路径按 CPA 工作目录解析。"},
			},
		},
		Capabilities: registrationCapability{UsagePlugin: true, ManagementAPI: true, Scheduler: true},
	}
}

// HandleMethod dispatches one RPC from the host.
func HandleMethod(method string, request []byte) (response []byte) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = ErrorEnvelope("plugin_panic", fmt.Sprint(recovered), http.StatusInternalServerError)
		}
	}()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if errConfigure := configure(request, method == "plugin.register"); errConfigure != nil {
			return ErrorEnvelope("plugin_config_invalid", errConfigure.Error(), http.StatusBadRequest)
		}
		return okEnvelope(pluginRegistration())
	case "plugin.quiesce", "plugin.shutdown":
		Quiesce()
		return okEnvelope(map[string]any{})
	case "usage.handle":
		return handleUsage(request)
	case "scheduler.pick":
		return handlePick(request)
	case "management.register":
		return okEnvelope(managementRegistration())
	case "management.handle":
		var req managementRequest
		if errUnmarshal := json.Unmarshal(request, &req); errUnmarshal != nil {
			return ErrorEnvelope("invalid_request", errUnmarshal.Error(), http.StatusBadRequest)
		}
		return okEnvelope(handleManagement(req))
	default:
		return ErrorEnvelope("unknown_method", "unsupported plugin method: "+method, http.StatusNotFound)
	}
}

// Shutdown flushes the ledger and stops the background workers.
func Shutdown() {
	Quiesce()
}

const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#72787c" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2v20"/><path d="M17 6H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6"/></svg>`

func unixNow() time.Time { return time.Now().UTC() }
