# 验证记录

这份文件记录插件在**真实 CLIProxyAPI 进程**里的验证方式与结果，不依赖任何 mock 的 ABI。

## 环境

| 项 | 值 |
| --- | --- |
| CPA | `router-for-me/CLIProxyAPI` 主干源码，`go build ./cmd/server` |
| 插件 | `make build`（`-tags cshared -buildmode=c-shared`） |
| 上游 | 本地 OpenAI 兼容 mock（`/v1/chat/completions`，固定返回 usage 2000/1000 token） |
| 凭证 | `api-keys.openai-compatibility` 下两条 key（运行时凭证，无物理文件） |
| 插件配置 | `default_limit_usd: 0.005`，`price_source: off`，兜底价 1 美元/百万 token，`debug: true` |

单价 1 美元/百万 token × 3000 token = 每次请求 3000 µUSD = $0.003，所以 $0.005 的额度在第二次请求后耗尽。

## 结果

启动：

```
pluginhost: plugin loaded    plugin_id=credential-budget path=plugins/linux/amd64/credential-budget.so
pluginhost: plugin registered plugin_id=credential-budget version=0.1.0
```

请求 1–3 通过，key A 在第 2 次请求后过额：

```
scheduler.pick: provider= model=gpt-4o-mini candidates=2 exhausted_rows=0
scheduler.pick: provider= model=gpt-4o-mini candidates=2 exhausted_rows=0
scheduler.pick: provider= model=gpt-4o-mini candidates=2 exhausted_rows=0
credential openai-compatibility:mock:68bc4482762f exhausted its budget (spent $0.006000 of $0.005000)
cannot disable: credential not found in host.auth.list      # 配置型 key 没有物理文件，符合预期
```

请求 4 被调度否决改道到另一条 key：

```
scheduler.pick: provider= model=gpt-4o-mini candidates=2 exhausted_rows=1
scheduler.pick: steering away from 1 over-budget candidate(s), picked openai-compatibility:mock:82906d360369
credential openai-compatibility:mock:82906d360369 exhausted its budget (spent $0.006000 of $0.005000)
```

两条 key 都过额后，请求被拒（`on_all_exhausted: reject` 默认）：

```
scheduler.pick: all 2 candidates for provider  are over budget
POST /v1/chat/completions 500 {"error":{"message":"credential_budget_exhausted: all credentials for this route have consumed their one-time budget", ...}}
```

面板 API（`GET /v0/management/plugins/credential-budget/state`）：

```json
{"credentials":[
  {"key":"openai-compatibility:mock:68bc4482762f","spent_usd":0.006,"limit_usd":0.005,"status":"exhausted"},
  {"key":"openai-compatibility:mock:82906d360369","spent_usd":0.006,"limit_usd":0.005,"status":"exhausted"}
], "totals":{"credentials":2,"exhausted":2,"spent_usd":0.012}}
```

改额度 + 解封后同一行立刻回到「限额中」，`spent_usd` 归零、`disabled_by_plugin` 清空。

## 物理凭证（OAuth 账号）路径

配置型 key 走不了 `host.auth.save`，所以 disabled 写入用单元测试覆盖（`internal/plugin/plugin_test.go`）：

- `TestUsageTripsBudgetAndDisablesAuthFile` — 过额后 `host.auth.save` 收到的 JSON 里 `disabled=true`，且 `access_token` 等字段原样保留
- `TestUnsealClearsSpendAndReenables` — 解封把同一份凭证写回 `disabled=false`，计数归零

宿主侧的行为由 CPA 自己的测试保证（`internal/pluginhost/auth_callbacks_test.go::TestHostAuthSaveCallbackPreservesDisabledState`：写入 `disabled:true` 后运行时记录变为 `StatusDisabled`，再写 `false` 回到 `StatusActive`），而路由层会排除 disabled 凭证（`conductor_selection.go:1106`）。

## 第二轮：provider_limits、备注名、分组

配置改为 `provider_limits: {openai-compatible-mock: 0.005}`（不再设 `default_limit_usd`），重启后跑三个请求：

```
credential openai-compatibility:mock:68bc4482762f limit 0.005 source provider_default spent 0.006 exhausted
credential openai-compatibility:mock:82906d360369 limit 0.005 source provider_default spent 0.003 limited
groups [{count: 2, exhausted: 1, limited: 2, provider: openai-compatible-mock, spent_usd: 0.009}]
```

两条**新出现**的凭证都自动拿到 $0.005，`limit_source` 记为 `provider_default`，不需要在面板点任何东西。

再给两条 key 设备注名（`POST /alias`），面板随即以备注为主标题、base-url 与 ID 尾号作次要行：

```
5刀-A          http://127.0.0.1:9099/v1 · #fe336a     $0.006 / $0.005   已停用
5刀-B 备用      http://127.0.0.1:9099/v1 · #9c2bc6     $0.003 / $0.005   限额中
```

## 第三轮：一次请求带出同供应商的全部 key

同一个 `openai-compatibility` 上游配 3 条 key，只发**一个**请求：

```
scheduler.pick: registered 3 new credential(s) from the candidate list
openai-compatibility:mock:533899238533  limit=0.005 spends=0.003 observed_only=False
openai-compatibility:mock:6a5b9975e57a  limit=0.005 spends=0      observed_only=True
openai-compatibility:mock:c7ad9ed46668  limit=0.005 spends=0      observed_only=True
totals: {credentials: 3, exhausted: 0, limited: 3, spent_usd: 0.003}
```

一条 key 发生结算，另外两条从未被轮到，但它们已经在面板里、已经带 `provider_limits` 的 $0.005、可以立刻单独改额度。这条路径是配置型 key 唯一能被"发现"的通道——`host.auth.list` 只列物理 auth 文件。

## 商店安装与升级（v0.1.1）

```
GET  /v8/management/plugins/store              → credential-budget 0.1.1, github-release
POST /v8/management/plugins/store/credential-budget/install {"version":"0.1.1"}
                                                → 200 installed, plugins/linux/amd64/credential-budget-v0.1.1.so
pluginhost: plugin loaded version=0.1.1        → 热加载，restart_required=false
```

v0.1.0 与 v0.1.1 都从商店装过一遍，CI 的 tag↔版本一致性门禁把版本和 release 资产名绑在一起。

## 第四轮：分段（长上下文）计费

价目 override：`big-model` / `edge-model` 基础 $1/1M，`min_prompt_tokens: 200000` 之后 $2/1M。mock 按模型返回不同 token 数，三个请求：

```
small   prompt_tokens=1000    → 1000 µUSD      （基础价）
edge    prompt_tokens=199999  → 199999 µUSD    （差 1 个 token，仍走基础价）
big     prompt_tokens=250000  → 500000 µUSD    （进入 ≥200k 档）
合计 spent_usd = 0.700999，last_tier = "≥200k tokens"
```

手算 `(1000 + 199999 + 250000×2) µUSD = 700999 µUSD = $0.700999`，与面板一致。

OpenRouter 真实价目表：一次刷新拿到 **361 个模型，其中 58 个带分段价**，说明 `pricing.overrides` 的解析在真实数据上生效。

顺带修掉一个自扰问题：宿主启动时会多次调用 `plugin.register` / `reconfigure`，每次都装一个新的刷新协程，原先会连着打好几次 OpenRouter；加了 5 分钟冷却（手动「刷新价目」按钮用 force 绕过），同一次启动只发一次请求。

## 单元测试

```
$ go test -race ./...
=== RUN   TestUsageTripsBudgetAndDisablesAuthFile        --- PASS
=== RUN   TestUnsealClearsSpendAndReenables              --- PASS
=== RUN   TestSchedulerVetoesExhaustedCandidate          --- PASS
=== RUN   TestSchedulerRejectsWhenAllCandidatesExhausted --- PASS
=== RUN   TestUnpricedModelUsesFallbackRates             --- PASS
=== RUN   TestSkipPolicyChargesNothingButCounts          --- PASS
=== RUN   TestCachedTokensAreNotBilledTwice              --- PASS
=== RUN   TestUsageWithoutTokensIsIgnored                --- PASS
=== RUN   TestProviderLimitsSeedNewCredentialsOnly       --- PASS
=== RUN   TestAliasNamesACredential                      --- PASS
=== RUN   TestSchedulerRegistersEveryCandidateOfAProvider --- PASS
=== RUN   TestDecodeOpenRouterKeepsSteppedPrices          --- PASS
=== RUN   TestSelectTierUsesFullInputAndDoesNotStack     --- PASS
=== RUN   TestTierWindowWrapsPastMidnight                --- PASS
=== RUN   TestQuoteChargesTheMatchedTier                 --- PASS
=== RUN   TestConfigOverrideAcceptsTiers                 --- PASS
=== RUN   TestLedgerSurvivesReload                       --- PASS
=== RUN   TestLimitEditReportsPendingUnseal              --- PASS
PASS
ok  github.com/kakaw-1/credential-budget/internal/plugin  1.064s
```

## 未覆盖

- **Oh My CPA 控制台里的托管页面**：需要完整 OMC 栈。插件侧只依赖 OMC 的既有契约（`/v0/resource/plugins/<id>/...` 页面 + `/v0/management/...` 路由 + 页面内的绝对路径会被 OMC 重写到 `/api/v1/plugin-host/...`）。
- **真实 OAuth 账号的 disabled 写入**：需要真实 claude/codex 凭证才能端到端跑一次。
