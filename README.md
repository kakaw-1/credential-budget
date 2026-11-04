# credential-budget

给 CLIProxyAPI 的上游凭证加一道**一次性美元额度**：给某个 key 设 $5，花到 $5 就停止使用，下游请求不会再落到它身上。**不会自动重置**，只能由人在面板上手动解封。

适用于 Oh My CPA 面板（插件页面会被 OMC 原样托管在控制台里），也适用于 CPA 自带的管理中心。

---

## 它做什么

- 按**单个上游凭证**（一个 OAuth 账号、或 `api-keys` 里的一条 key）累计花费。
- 达到额度后立刻把该凭证停下，后续请求绕开它。
- 面板里设额度、看进度、手动解封、看操作记录。

## 它不做什么

- 不做周期重置。没有日/周/月窗口，额度是一次性的，清零只能手动。
- 不重排路由。停掉一个凭证之后走哪个凭证、是否降级到别的供应商，全交给 CPA 自己的调度。
- 不改 `config.yaml`。额度与账本存在状态文件里，不走配置写回。

---

## 两个停止机制

插件同时用两条腿走路，因为「凭证」有两种形态：

**① 写 disabled（首选，物理凭证）**
凭证达到额度后，插件调 `host.auth.get` 读出它的 JSON，把 `disabled` 置为 `true`，再调 `host.auth.save` 写回。CPA 会同时更新运行时记录，**它自己的所有调度器都会跳过这个凭证**，不需要重启，也不需要插件再插手。OAuth 账号（claude / codex / gemini …）走这条路。

**② 调度否决（兜底，运行时凭证）**
配置里的 `api-keys`（例如 `openai-compatibility` 的每条 key）没有物理文件，写不了 disabled。这类凭证靠 `scheduler.pick` 拦：插件发现本次候选里出现过额凭证，就直接从剩下的候选里挑一个返回；如果一个都不剩，按 `on_all_exhausted` 决定是拒绝请求（默认）还是交回网关。

---

## 安装

### 插件商店

**1. 把商店源加进去。** 在 CPA 的 `config.yaml` 里：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - "https://raw.githubusercontent.com/kakaw-1/credential-budget/main/registry.json"
```

或者用控制台：Settings → Plugin store → 添加源，填同一个地址。

**2. 商店里搜 `credential-budget` → 安装。** 安装对话框要你手打一次 id 确认（这是 CPA 的规矩：确认的是"要在这个进程里跑代码"，不是确认来源）；装完插件会热加载，`restart_required: false`。

**3. 启用**，见下方"启用"一节。

也可以直接调接口装。**不要写 `version`**，不写才会装最新 release；写上就等于把版本钉死，写 `0.1.0` 就永远装 0.1.0：

```sh
curl -X POST http://127.0.0.1:8317/v8/management/plugins/store/credential-budget/install \
  -H "Authorization: Bearer $MANAGEMENT_KEY" -H 'Content-Type: application/json' \
  -d '{}'
```

升级到新版同样调这个接口、同样不写 `version`；装完看返回体里的 `version` 和落地的 `.so` 文件名核对。

### 手动安装

在 CPA 的工作目录执行：

```sh
curl -fsSL https://raw.githubusercontent.com/kakaw-1/credential-budget/main/install.sh | sh
```

或者从 Releases 下载对应平台的压缩包，把里面的 `.so` 放到 `plugins/linux/amd64/`（macOS 用 `plugins/darwin/<arch>/`，扩展名 `.dylib`）。

### 启用

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    credential-budget:
      enabled: true
      priority: 1
      default_limit_usd: 5      # 新凭证默认额度（美元）；0 = 不限额
      price_source: "openrouter"
```

重载配置即可生效，无需重启进程。确认加载成功：

```sh
grep credential-budget <CPA 日志>   # 应看到 plugin loaded / plugin registered
```

---

## 配置项

| 键 | 默认 | 说明 |
| --- | --- | --- |
| `default_limit_usd` | `0` | 新凭证第一次被记账时采用的额度（美元）。`0` 表示不限额，只在面板单独设。已存在的凭证不会被追溯修改。 |
| `provider_limits` | 空 | 按供应商给**新出现**的凭证指定默认额度，优先于 `default_limit_usd`。键写供应商名或它的任意片段（`openai-compatible-mock`、`mock` 都能命中），最长匹配优先。同样不追溯已存在的凭证。 |
| `enforce` | `true` | 关掉后只记账不熔断，用来先观察一段时间。 |
| `auto_disable` | `true` | 关掉后不写 disabled，只靠调度否决。 |
| `price_source` | `openrouter` | 单价来源。`openrouter` 用其公开价目表（无需 key，每 `price_refresh_hours` 刷一次）；`off` 只用 `price_overrides` + 兜底价。 |
| `price_refresh_hours` | `24` | 价目表刷新间隔，最小 15 分钟。抓取失败保留上一份缓存，不改变计费。 |
| `unpriced_policy` | `fallback` | 价目表查不到模型时：`fallback` 按兜底价计费（不是免费），`skip` 不计费但计入「未定价次数」。 |
| `fallback_price` | prompt 3 / completion 15 / cache_read 0.3 / cache_write 3.75 | 兜底价，单位美元/百万 token。 |
| `price_overrides` | 空 | 按模型手工定价，优先级高于价目表。 |
| `channel_multiplier` | `1` | 整体价格系数。 |
| `on_all_exhausted` | `reject` | 该路由上所有候选都过额时：`reject` 返回额度不足；`pass` 交回网关继续调度（会超支）。 |
| `state_file` | `plugins/credential-budget-state.json` | 账本与价目表缓存，相对路径按 CPA 工作目录解析。 |
| `debug` | `false` | 记录每次调度决策。 |

```yaml
      provider_limits:
        claude: 5                      # claude 下新账号自动带 $5
        openai-compatible-mock: 3      # 这个上游的每条新 key 自动带 $3
      price_overrides:
        claude-sonnet-4-5:
          prompt: 3
          completion: 15
          cache_read: 0.3
          cache_write: 3.75
        "gpt-5.1":
          prompt: 1.25
          completion: 10
        # 分段价：输入超过 200k 之后换档，未写的字段继承上面的基础价
        gemini-2.5-pro:
          prompt: 1.25
          completion: 10
          tiers:
            - min_prompt_tokens: 200000
              prompt: 2.5
              completion: 15
```

---

## 计费口径

沿用 Oh My CPA 的口径：输入、输出、缓存读、缓存写四个桶，单价按**美元/百万 token**，整数微美元（µUSD）累加，全程用有理数精确计算，不做浮点累加。

- 未缓存输入 = `输入 token − 缓存读 − 缓存写`
- 输出 = `输出 token`（若推理 token 更大则取推理 token）
- 缓存读 / 缓存写分别按自己的单价
- 未定价模型**默认按兜底价计费**，面板会显示「N 次未定价」，方便你补 `price_overrides`

单价参考 Oh My CPA 的做法，取 OpenRouter 公开模型列表（`prompt` / `completion` / `input_cache_read` / `input_cache_write`）。

### 分段计费（长上下文换档）

和 OMC 一样支持按上下文长度分档：同一个模型，输入超过某个 token 数之后换一套价。

- 阈值判定看**本次输入的完整 token 数（含缓存）**，`>= min_prompt_tokens` 才算进入该档；
- 一个请求**只取一档**，绝不叠加；多档同时命中时取阈值更大的那个；
- 档位只覆盖它写了的字段，没写的（比如只给了 `prompt`）继承基础价；
- 可选时间窗 `utc_start` / `utc_end`（HHMM，UTC，半开区间，可跨午夜），用于按时段变价的模型；
- 数据来源两种：OpenRouter 价目表里自带的 `pricing.overrides`（自动），以及 `price_overrides.<model>.tiers`（手写，优先）；
- 面板「价目表」会显示多少个模型带分段价，凭证行会用「按 ≥200k tokens 计价」标出最近一次落在哪一档。

实测（v0.1.2）：基础 $1/1M、≥200k 换 $2/1M 的模型，发三个请求 —— 1000 token 记 1000 µUSD、199999 token 记 199999 µUSD（不足阈值走基础价）、250000 token 记 500000 µUSD（进入高档），合计 $0.700999，与手算完全一致。

---

## 面板

`插件 → 凭证额度`。

**按供应商分组**：每个供应商一行组头，只显示统计（`N 条 · 已设额度 M · 已停用 K · 合计 $X`），下面才是这个供应商的凭证。分组只是看，改额度仍然是一条一条改。

**怎么认出是那张 key**：每行主标题按优先级取 `备注名 > 主机上报的名字/邮箱 > auth 文件名 > 运行时 ID`，第二行小字给 `上游 base-url · #ID后6位`，最近使用时间下面标出这次用的模型。OAuth 账号通常直接显示邮箱；配置型 key（`api-keys`）只有 `openai-compatibility:mock:68bc4482762f` 这种哈希，认不出就点「设置」给它起个备注名，比如「5刀-A」。

**一个供应商下的多条 key 会全部列出来。** 配置型 key 没有 auth 文件，`host.auth.list`（也就是「同步凭证」按钮）拉不到它们；插件的做法是**在每次调度时把该供应商的全部候选登记进账本**。所以只要这个供应商**发过一次请求**，它名下所有 key 就都会出现——已经服务过的那条显示已用金额，其余标「未使用」（已用 $0），额度按 `provider_limits` 预置，可以立刻逐条调额度。零流量时仍然列不出来，因为 CPA 没有给插件读配置的接口。

**设置** 展开后可以同时改备注名和额度，一次保存；**解封** 清零已用并放开该凭证。

- **解封 = 清零已用 + 解除插件设的 disabled**，等于重发一张新额度。
- **提额不会自动解封**：额度改高了但凭证仍停用，面板会提示你再点一次「解封」。这是刻意的——解封只有人能触发。
- 插件只解除**自己**设的 disabled；如果这个凭证是你手动关掉的，解封不会替你打开。
- **整组批量设额度没有实现，也不打算做**：额度必须一条 key 一个数，否则同组 key 会共享池子。想让新 key 自动带额度，用 `provider_limits`（只影响之后新出现的凭证）。

---

## 行为边界（重要）

1. **硬上限做不到 100% 不超。** 预检在请求前、结算在响应后，进行中的请求会把额度花穿一点（典型是最后一次请求的整笔费用）。要绝对不超只能按 `max_tokens` 预留，插件没这么做。
2. **运行时凭证只能靠调度拦。** `api-keys` 里的 key 没有物理文件，`host.auth.save` 不适用，面板会显示「禁用失败」。这类凭证靠 ② 兜住。它们的**发现**也受同一个限制：靠调度候选登记，所以要发过一次请求才看得见；候选只含当前优先级层里可用的凭证，配了不同 `priority` 的低层 key 可能暂时不出现。
3. **全部凭证过额时，默认直接拒绝**（HTTP 500 + `credential_budget_exhausted`）。CPA 的调度拒绝没有独立状态码位，所以是 500 而不是 429；要让它继续调度就设 `on_all_exhausted: pass`。
4. **写 disabled 会把整份凭证 JSON 写回**，`access_token` / `refresh_token` 等字段原样保留。极端情况下若恰好有 token 刷新发生在读与写之间，写回的是稍旧的一份 token——凭证本来就会被禁用，解封后 CPA 会自行刷新。
5. **额度只在账本里**，删掉状态文件等于全部重置（未解封的 disabled 不会自动恢复）。
6. 插件进程内运行在 CPA 里，`host.auth.save` 失败不会拖垮请求，只会记录到面板的 `disable_error`。

---

## 状态文件

```json
{
  "version": 1,
  "credentials": {
    "openai-compatibility:mock:68bc4482762f": {
      "key": "openai-compatibility:mock:68bc4482762f",
      "provider": "openai-compatible-mock",
      "limit_micro_usd": 5000000,
      "spent_micro_usd": 6000000,
      "requests": 2,
      "exhausted": true,
      "disabled_by_plugin": true,
      "limit_source": "manual"
    }
  },
  "price_book": { "source": "openrouter", "fetched_at": "...", "models": { } },
  "audit": [ ]
}
```

写入是「临时文件 + fsync + rename」。读到的文件损坏或版本更高时，插件**停写而不是覆盖**，面板顶部会显示红色的「账本写入已暂停」。每 2 秒合并落盘一次，熔断与解封会立即落盘。

---

## 故障排查

| 现象 | 原因 |
| --- | --- |
| 面板看不到凭证 | 先点「同步凭证」；或该凭证还没被用过（记账是请求结算后才产生） |
| 「禁用失败：credential not found in host.auth.list」 | 这是运行时凭证（`api-keys` 配置项），正常，靠调度否决 |
| 额度到了还在跑 | 检查 `enforce`、`auto_disable`；开 `debug` 看 `scheduler.pick` 是否被调用 |
| 价目表 0 个模型 | 出网失败或 `price_source: off`；看面板里的 `last_error` |
| 全部凭证停了之后报 500 | 默认行为，见边界 3 |

---

## 开发

```sh
make test      # go test -race ./...
make build     # dist/credential-budget.so
make package   # 按发行包命名打 zip，用于插件商店
```

`.so` 必须用 `-tags cshared -buildmode=c-shared` 构建，导出 `cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`。

核对 ABI：插件按 CPA schema version 10 编写，用到 `usage.handle`、`management.register` / `management.handle`、`scheduler.pick` 与 `host.auth.list` / `host.auth.get` / `host.auth.save` / `host.log`。

## License

MIT
