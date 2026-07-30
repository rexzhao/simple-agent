# Provider 重试统一重构 — 开发计划（r4，已PASS）

> 起因：codex 会话中服务端 500 时，turn 在第一次重试后即失败（日志证据见下）。定位为 `agent.go` 重试循环的 `break` 泄漏 bug；进一步发现 HTTP 层与会话层两套重试规则分裂。本计划统一重试规则。
>
> 评审历史（同一 grok-4.5 会话，session `20260729T104935.633590000Z-638dcda5`）：r1 FAIL（8 项）→ r2 FAIL（7 项）→ r3 FAIL（1 项阻断）→ **r4 PASS**。评审非阻塞建议已吸收：`errors.As` 正确写法；实施时注意 `internal/agent` 将新增对 `internal/model/httpstream` 的 import（无循环，`httpstream` 不依赖 `model`/`agent`）。
>
> **引用约定：下文一律以包名 + 函数名为准，不写死行号。**

---

## 0. 现状与已确认根因

### 0.1 三套各自为政的重试规则

| 层 | 位置 | 重试对象 | 次数 | 退避 | 可见性 |
|---|---|---|---|---|---|
| 传输层·状态码 | `internal/model/httpstream` `DoRequest` | 429 + 5xx（私有 `isRetryableStatus`） | 3（`DefaultMaxRetryAttempts`） | 固定 200ms | 不可见，最终报 `500 ... after 3 attempts` |
| 传输层·超时 | `internal/model/httpstream` `doRequestWithTimeoutRetry` | 请求超时（`DefaultRequestTimeout=15s`） | 2（`DefaultTimeoutRetries=1`） | 固定 200ms | 不可见 |
| 会话层 | `internal/agent` `streamModelTurn` | 流内 `server_error`/`server_is_overloaded` 且零进度 | 5（`maxAttempts`） | 5s→10s→20s→40s（`providerRetryBackoff`） | 可见（`ProviderRetryEvent` → `provider.retrying`） |

### 0.2 已确认的根因（日志证据）

故障 session 的 run 日志（`%APPDATA%/sai/logs/<run>/sai.jsonl`）当天 6 次失败模式完全一致：

```
provider_retry  attempt=2 max_attempts=5 delay_ms=5000
error           "server_error: ... (code server_error)"   ← 恰好 5.000s 后（误差 <1ms）
```

因果链：

1. `streamModelTurn` 的 `case model.ErrorEvent` 重试分支末尾 `break` 是 Go 的 **switch break，不是 for break**；
2. 于是 `waitForProviderRetry` 睡完 5s 后，控制流落到 switch 之后的 `out <- event`，**同一个可重试 ErrorEvent 被当作终态错误转发给下游**——这就是日志中 error 与 retry 间隔精确等于 backoff 的原因；
3. 下游 `runSessionTurn`（`internal/execution/agent_runner.go`）在其事件消费循环中对任何 `ErrorEvent` 立即 `return nil, modelStreamError(...)`，turn 失败、`turnCtx` 被取消；
4. agent 循环虽然 `continue` 到 attempt 2，但消费方已退出，第 2 次重试实际从未发生。

**5 次重试的循环从未真正跑到第 2 次。** 现有测试 `TestStreamRetriesServerErrorBeforeAnyProviderProgress` 只断言重试发生、文本恢复，未断言"错误事件不得转发"，因此带 bug 也能通过。

### 0.3 次生问题

- **HTTP 状态码 500 无会话层重试**：`httpstream` 3×200ms 失败后，`provider.Stream` 返回 `StatusError`（三个 provider 均为 `fmt.Errorf("... request failed: %w")` 包装），`streamModelTurn` 走 `"request model"` 分支**立即失败，无任何退避重试**。
- **compact 路径是特例**：`Provider.Compact`（`internal/model/openai_responses`）只有传输层 3 次，失败后由 compaction 规划层回退本地摘要（`planRemoteCompactionCheckpoint` 失败后记录日志并回退 `planSummaryCompactionCheckpoint`）。
- **字符串匹配覆盖不全且脆弱**：`isRetryableProviderStreamError`（`internal/agent`）只匹配 `code server_error` / `server_is_overloaded` 两个子串；Anthropic 的 `overloaded_error: Overloaded`（`anthropicStreamErrorEvent`）与 chat 的 `rate_limit_error: ... (code rate_limited)` 均不匹配，今天就不重试。

---

## 1. 目标与成功标准

**目标**：状态码重试决策唯一在会话层 `streamModelTurn`（传输层仅保留超时快重试 2×200ms，不做状态码决策）；全部可重试错误共享同一退避策略与次数预算（idle 超时单独限额），且每次重试对用户可见。

成功标准：

1. 零进度前提下，HTTP 5xx/429/408、请求超时、流内 `server_error`/`overloaded`/`rate_limit`、**连接级 transport 窄集合（§3.1）** 统一按最多 5 次指数退避重试，日志可见 `provider_retry` 后才终态失败；
2. 流空闲超时（`StreamIdleTimeoutError`，2 分钟无帧）零进度时可重试，但**单独限额：最多 2 次尝试（1 次重试）**。idle 占用同一 `attempt` 计数、另受 `idleRetried` 上限约束——即"5 次预算内 idle 最多出现 2 次"，而非 5+2 的独立预算；
3. 重试中的 turn 不再提前失败——可重试错误不泄漏到下游事件流（回归测试断言事件流中 ErrorEvent 计数为 0）；
4. 有任何内容产出（text/reasoning/tool_call）后失败一律不重试（维持现有语义）；
5. HTTP 4xx（除 408/429）、认证错误、TLS/x509/DNS 永久失败、ctx 取消/超时不重试；
6. `POST /responses/compact` 外层最多 2 次尝试（含 1 次重试，退避 1s），最终失败仍回退本地摘要（现有兜底不变）；
7. 不新增配置项，`maxAttempts` 保持硬编码 5。

最坏耗时上界（用于评审与文案）：

- 全 5xx 快速失败路径：5 次请求 + 4 段退避（75s）+ 每次请求超时上界 15s ≈ **2.5 分钟**；
- 全 idle 超时路径：2 次尝试 × 2 分钟 + 1 段退避 5s ≈ **4 分 5 秒**；
- 混合路径不超过约 **5 分钟**。

## 2. 非目标

- 不做"有进度后的断点续传"（continuation resume）；`shouldRetryWithoutContinuation` 是 provider 内部既有特例，与本计划正交，不动；
- 不引入 idempotency key；
- 不改 UI（沿用现有 `provider.retrying` 事件，`internal/execution/session_events.go` 的 `provider.retrying` 分支）；
- 不改退避曲线公式（决策见 §3.3）；
- 不把 `ProviderError` 重构为携带结构化 `Retryable` 字段（列为后续可选清理，见 §6）；
- **`collectCompactionSummary`（`internal/execution/agent_runner.go`，本地摘要压缩）不获得会话层重试**：它是 `streamModelTurn` 之外唯一直接调用 `provider.Stream` 的生产路径（已全局检索确认）。传输层状态码重试降到 1 后，它从"3×200ms"降为"1 次 + 超时快重试"。该路径对持续 500 本就无有效抵抗力，失败语义不变（compaction 失败即 turn 失败），接受降级以控制范围，见 §6 风险表。

## 3. 设计

### 3.1 统一错误分类：`model.IsRetryableProviderError`

新文件 `internal/model/retry.go`，签名 `func IsRetryableProviderError(err error) bool`（**只有 err 参数**；调用方另有 ctx 守卫，见 §3.2）。按序判定：

| 错误形态 | 判定 | 实现 |
|---|---|---|
| ctx 取消/超时 | **不重试** | `errors.Is(err, context.Canceled)` / `errors.Is(err, context.DeadlineExceeded)`，全链 unwrap（`doRequestOnce` 在父 ctx 取消时直接返回 `ctx.Err()`，分类函数无需也无法访问 ctx） |
| `*httpstream.StatusError` | 408/429/5xx 重试，其余 4xx 不重试 | 复用 `httpstream` 状态码判定：私有 `isRetryableStatus` 导出为 `IsRetryableStatus` 并补 408 |
| `*httpstream.RequestTimeoutError` | 重试 | 传输层 15s 无响应 |
| `*httpstream.StreamIdleTimeoutError` | 重试（**受 §3.2 单独限额约束**） | 流 2 分钟无帧 |
| 连接级传输错误 | 重试 | **窄集合**：`io.ErrUnexpectedEOF` + 平台连接 errno（见下"跨平台判定"）。**不做 `errors.As(net.Error)` 全匹配**（TLS/x509 经 `*url.Error`→`*net.OpError` 也能命中，会误判）；**不含裸 `io.EOF`**（正常关流语义，非瞬时错误）；TLS/证书/DNS 永久失败默认不重试（靠"不在窄集合内"实现） |
| `*model.ProviderError`（流内 API 错误） | 消息小写含 `server_error` / `overloaded` / `rate_limit` 之一则重试 | 覆盖现状两个子串 + Anthropic `overloaded_error: Overloaded` + chat `rate_limit_error/(code rate_limited)`；字符串匹配为过渡方案，结构化标记见 §6 |
| 其他 | 不重试 | 默认安全 |

**连接 errno 的跨平台判定（build tag 拆分，已实测验证）**：

- `internal/model/retry_transport_unix.go`（`//go:build !windows`）：`syscall.ECONNRESET`、`ECONNABORTED`、`ECONNREFUSED`、`EPIPE`；
- `internal/model/retry_transport_windows.go`：`syscall.WSAECONNRESET`(10054)、`syscall.WSAECONNABORTED`(10053)、`syscall.ERROR_BROKEN_PIPE`(109)，以及**自定义** `wsaECONNREFUSED syscall.Errno = 10061`（stdlib 未导出 `WSAECONNREFUSED`）；
- 两个文件各自提供 `isRetryableConnError(err error) bool`（对平台 errno 列表做 `errors.Is`），`retry.go` 调用它。

**为什么不能直接用 `syscall.ECONNRESET`**（实测，go1.25.6 windows/amd64）：Windows 上这些 BSD 风格常量是 `zerrors_windows.go` 里的"编造值"（`APPLICATION_ERROR(1<<29)+iota`，如 `ECONNRESET=536870935`），**能编译但不等于真实 WSA errno**（`WSAECONNRESET=10054`）；真实连接错误以 WSA 值上浮，`net/error_windows.go` 内部也是用 `WSA*` 常量比较。直接 `errors.Is(err, syscall.ECONNRESET)` 在 Windows 上永不命中——表现为"编译通过但静默不重试"。仓库已有按 OS 拆分先例（`internal/sessions/write_lock_{unix,windows}.go`）。

同文件提供**导出**函数 `func RetryReason(err error) string`（供 `internal/agent` 跨包调用，`agent` 与 `model` 不同包，必须导出）：

| 条件 | reason |
|---|---|
| `StatusError` 429，或 `ProviderError` 消息含 `rate_limit` | `rate_limited` |
| `StatusError` 408/5xx，或 `ProviderError` 消息含 `server_error`/`overloaded` | `server_error` |
| `RequestTimeoutError` / `StreamIdleTimeoutError` | `timeout` |
| 连接级传输错误窄集合 | `transport` |
| 其他（防御性默认） | `server_error` |

放在 `internal/model` 的原因：`agent`（会话层重试）和 `openai_responses`（compact 重试）都需要它；`httpstream` 不 import `model`，无循环依赖。

### 3.2 会话层唯一重试循环（完整控制流闭环）

`streamModelTurn` 重构为两处出口共用同一判定。**以下为可照抄的完整闭环**（在现有结构上做最小改动）：

```go
var idleRetried bool                                  // 跨 attempt 生效（idle 限额）
for attempt := 1; attempt <= maxAttempts; attempt++ {
    stream, err := provider.Stream(ctx, request)
    if err != nil {
        // Stream() 调用错误：madeProgress 恒 false，直接按分类判定
        if ctx.Err() == nil && attempt < maxAttempts && model.IsRetryableProviderError(err) {
            delay := providerRetryBackoff(attempt)
            out <- model.ProviderRetryEvent{Attempt: attempt + 1, MaxAttempts: maxAttempts, Delay: delay, Reason: model.RetryReason(err)}
            if err := waitForProviderRetry(ctx, delay); err != nil {
                out <- model.ErrorEvent{Err: err, Message: "retry model request"}
                return assistantContent.String(), reasoningContent.String(), nil, nil, true
            }
            continue                                  // → 下一 attempt
        }
        out <- model.ErrorEvent{Err: err, Message: "request model"}   // 终态
        return assistantContent.String(), reasoningContent.String(), nil, nil, true
    }

    madeProgress := false                             // 每次 attempt 内重置（不得提升为跨 attempt）
    retry := false
streamLoop:
    for event := range stream {
        switch event := event.(type) {
        // ... TextDelta/ReasoningDelta/ToolCall/MessageDone/Usage 置 madeProgress、
        //     ResponseStateEvent 不置位且 continue 跳过转发（均维持现状）...
        case model.ErrorEvent:
            var idleTimeoutErr *httpstream.StreamIdleTimeoutError
            idle := errors.As(event.Err, &idleTimeoutErr)
            if !madeProgress && attempt < maxAttempts && ctx.Err() == nil &&
                model.IsRetryableProviderError(event.Err) && (!idle || !idleRetried) {
                if idle {
                    idleRetried = true
                }
                delay := providerRetryBackoff(attempt)
                out <- model.ProviderRetryEvent{Attempt: attempt + 1, MaxAttempts: maxAttempts, Delay: delay, Reason: model.RetryReason(event.Err)}
                if err := waitForProviderRetry(ctx, delay); err != nil {
                    out <- model.ErrorEvent{Err: err, Message: "retry model request"}
                    return assistantContent.String(), reasoningContent.String(), nil, nil, true
                }
                retry = true
                break streamLoop                      // 带标签：跳出事件循环，且不执行下方 out <- event
            }
            out <- event                              // 终态
            return assistantContent.String(), reasoningContent.String(), nil, nil, true
        }
        out <- event
    }
    if retry {
        continue                                      // → 下一 attempt
    }
    return assistantContent.String(), reasoningContent.String(), toolCalls, responseState, false  // 成功出口
}
panic("unreachable")                                  // 维持现状
```

要点：

- **带标签 break**：可重试错误不再执行 switch 后的 `out <- event`（修 §0.2 根因）；
- **成功出口与重试出口分离**：`retry` 标志区分"range 正常结束"（→ success return）与"break streamLoop"（→ continue 下一 attempt），两者缺一不可——否则成功后会被外层 for 重复发起请求，或重试被成功 return 吃掉；
- **变量作用域**：`madeProgress`/`retry` 每次 attempt 内重置；`idleRetried` 是唯一跨 attempt 的状态；
- **idle 单独限额**：`StreamIdleTimeoutError` 命中时置 `idleRetried`，第二次 idle 直接终态——全 idle 路径最坏 2×2min+5s（§1 耗时上界）；idle 占用同一 `attempt` 计数（§1 标准 2）；
- **调用方 ctx 守卫**：分类函数拿不到 ctx，循环在重试前显式检查 `ctx.Err() == nil`（与既有 `waitForProviderRetry` 内部守卫互补）；
- `ProviderRetryEvent.Reason` 由 `model.RetryReason` 填充，不再硬编码 `"server_error"`；`logging.go` 与 `session_events.go` 已透传该字段，无需改；
- 删除 `isRetryableProviderStreamError`，调用点全部换成 `model.IsRetryableProviderError`。

**与 Step 1 的关系**：Step 1 只做最小修复——保留 `retry` 标志，把 `break` 改为 `break streamLoop`（加标签），即与上述终态结构自然对齐，不产生二次返工。

### 3.3 退避曲线与次数预算

**决策：维持 `5s→10s→20s→40s` 不变**（`providerRetryBackoff`）。

- commit `51540d3` 刚把基础退避提高到 5s，倾向保守，不在本次推翻；
- 取消传输层 200ms 快速状态码重试后，单次抖动型 500 的恢复从 ~0.4s 变为 ≥5s——可接受，换取规则统一与可预测预算（§1 耗时上界）；
- 备选（不采纳）：首档 1s 的 `1s→5s→15s→30s`，留待后续观察再调。

### 3.4 传输层降级（注入点：生产装配层，两条返回路径都要设）

在 `providerHTTPOptions`（`internal/execution/agent_runner.go`）中，以 `MaxRetryAttempts: 1` 为**基础值**构造返回值，确保**两条返回路径都带显式 1**：

```go
func providerHTTPOptions(provider config.ProviderConfig) (httpstream.Options, error) {
    options := httpstream.Options{MaxRetryAttempts: 1}
    if provider.RequestTimeout == "" {
        return options, nil            // 默认生产路径也必须带 1
    }
    requestTimeout, err := time.ParseDuration(provider.RequestTimeout)
    if err != nil || requestTimeout <= 0 {
        return httpstream.Options{}, fmt.Errorf("request_timeout must be a positive duration")
    }
    options.RequestTimeout = requestTimeout
    return options, nil
}
```

- **关键陷阱**：现状 `RequestTimeout == ""` 时返回空 `Options{}`（`MaxRetryAttempts == 0`），`WithDefaults` 对 `<=0` 会回填 `DefaultMaxRetryAttempts = 3`——只改一条路径会让默认生产路径保持 3 次，设计落空。依赖的是**显式 1**，不是 `WithDefaults`；
- 该函数是全部 provider 构造的唯一装配点（`newProviderForRun` 调用它，主 provider、摘要 provider 等均走此漏斗）；
- **不改三个 provider 的 `NewProvider`**：构造器保持策略中立，单测仍可用显式 `MaxRetryAttempts: 2/3` 构造 provider 覆盖传输层重试能力——现有测试 `openai_chat.TestProviderStreamRetries429AndPreservesRequestBody`、`TestProviderStreamRetries5xxThenReturnsRedactedError`、`openai_responses.TestProviderStreamRetries429AndEmitsText`、`anthropic_messages.TestProviderStreamRetries5xxAndEmitsText` **全部保持通过、不迁移**；
- **保留**超时快速重试（`DefaultTimeoutRetries=1`，2×200ms）：连接级瞬时抖动的亚秒级抹平，对用户无感——这是唯一留在传输层的重试；
- `httpstream.DoRequest` 签名与语义不变；新增 `MaxRetryAttempts=1` 时 5xx 单次即返回的单测。

（评审备选"构造器强制覆盖"已否决：会打挂上述 4 个测试且让 provider 构造器夹带策略。）

### 3.5 compact 路径对齐

`Provider.Compact`（`internal/model/openai_responses`）加外层小循环：

- **外层最多 2 次尝试（含 1 次重试），退避 1s**；每次尝试内部仍保留传输层超时快重试（2×200ms），状态码不在传输层重试（§3.4 后恒为 1 次）——即最坏 2×(2 次超时尝试) 个 HTTP dial，表述以"外层尝试"为准；
- 判定复用 `model.IsRetryableProviderError`，重试前检查 `ctx.Err() == nil`；
- 最终失败仍返回错误，由 compaction 规划层按现状回退本地摘要（兜底逻辑不变）。

### 3.6 改动文件清单

| 文件 | 改动 |
|---|---|
| `internal/model/retry.go`（新增） | `IsRetryableProviderError` + 导出 `RetryReason` + 单测 |
| `internal/model/retry_transport_unix.go`（新增） | `//go:build !windows`：`isRetryableConnError`（ECONNRESET/ECONNABORTED/ECONNREFUSED/EPIPE） |
| `internal/model/retry_transport_windows.go`（新增） | `isRetryableConnError`（WSAECONNRESET/WSAECONNABORTED/ERROR_BROKEN_PIPE + 自定义 wsaECONNREFUSED=10061） |
| `internal/model/httpstream/httpstream.go` | 导出 `IsRetryableStatus`（补 408），私有 `isRetryableStatus` 改为调用导出版 |
| `internal/agent/agent.go` | `streamModelTurn` 按 §3.2 闭环重构；删 `isRetryableProviderStreamError`；`ProviderRetryEvent.Reason` 用 `model.RetryReason` |
| `internal/execution/agent_runner.go` | `providerHTTPOptions` 两条返回路径均带 `MaxRetryAttempts: 1` |
| `internal/model/openai_responses/provider.go` | `Compact` 加外层 2 次重试循环 |

三个 provider 的 `NewProvider` 及既有传输层重试测试**均不改**。

## 4. 分步实施

**Step 1 — 修根因（可独立合并）**
- `agent.go`：保留 `retry` 标志，`break` 改为带标签的 `break streamLoop`（加 `streamLoop:` 标签）；
- 新增回归测试：attempt 1 可重试错误 → attempt 2 恢复，断言 (a) provider 收到 2 次请求；(b) 收集到的事件中 **ErrorEvent 计数为 0**；(c) 文本完整。
- 验收：`go test ./internal/agent/` 通过；新测试在改动前必失败（已人工推演确认）。

**Step 2 — 统一错误分类**
- 新增 `internal/model/retry.go` + `retry_transport_{unix,windows}.go`；导出 `httpstream.IsRetryableStatus`（补 408）；
- `agent.go` 流内分支改用新分类函数 + `RetryReason`，删旧字符串匹配函数；
- 单测覆盖 §3.1 全表（ctx 取消包装错误、4xx/408/429/5xx、两类超时、平台连接 errno 正反例、TLS/x509 误判防护、ProviderError 三个子串正反例）及 reason 映射全表。
- 验收：`go test ./internal/model/... ./internal/agent/` 通过；**`GOOS=windows` 与 `GOOS=linux` 交叉编译 `go build ./internal/model/...` 均通过**。

**Step 3 — 重试上移 + 传输层降级 + compact 对齐**
- `streamModelTurn` 按 §3.2 完整闭环接入 `Stream()` 错误分支重试与 idle 限额；
- `providerHTTPOptions` 两条路径设 `MaxRetryAttempts: 1`；
- `Compact` 外层重试循环。
- 验收：§5 全部测试通过；手工故障注入验证 §5.3。

## 5. 测试计划

### 5.1 agent 层（`internal/agent/agent_test.go`）

**夹具扩展**：`fakeProvider` 增加按 turn 注入 `Stream()` 错误的能力（如 `streamErrs []error`，与 `turns` 对齐，非 nil 时直接返回错误）——现有夹具只能吐事件、无法返回错误，下列 StatusError/超时用例依赖此扩展。

| 用例 | 断言 |
|---|---|
| 流内可重试错误→恢复（Step 1 回归） | 2 次请求；**ErrorEvent 计数 0**；文本完整；retry 事件 attempt=2/max=5 |
| 有进度后流内 server_error | 不重试；ErrorEvent 透传（现有测试覆盖，保持） |
| 成功 turn 不重复请求 | 纯文本成功路径 provider 恰好 1 次请求（防 §3.2 闭环缺失类回归） |
| `Stream()` 返回 5xx StatusError→恢复 | 重试至恢复；retry 事件可见、`Reason=="server_error"`；无 `"request model"` 提前终态 |
| `Stream()` 返回 400 StatusError | 不重试，立即终态 |
| `Stream()` 返回 `RequestTimeoutError`→恢复 | 重试至恢复；`Reason=="timeout"` |
| 流内 `StreamIdleTimeoutError` | 第 1 次 idle 重试（`Reason=="timeout"`）；**第 2 次 idle 立即终态**（限额），provider 共 2 次请求 |
| `Stream()` 返回 429 StatusError | retry 事件 `Reason=="rate_limited"`（agent 包内断言事件字段，不跨包断言 execution 映射） |
| 连续 5 次可重试失败 | provider 恰好 5 次请求；retry 事件 attempt=2..5；最终 ErrorEvent |
| 退避等待中 ctx 取消 | 立即终态（`"retry model request"`），无多余请求 |

（测试中 `providerRetryBackoff` 替换为 0，沿用现有模式。）

### 5.2 model / httpstream / provider / execution 层

- `internal/model/retry_test.go`：§3.1 分类全表 + `RetryReason` 映射全表（含 TLS/x509 包装链反例、ctx 包装错误反例）；连接 errno 用例按平台构造：Windows 下用 `&net.OpError{Err: &os.SyscallError{Err: syscall.Errno(10054)}}` 正例（10054=`WSAECONNRESET`）、`syscall.Errno(10022)`(`WSAEINVAL`) 反例；unix 下对应 `syscall.ECONNRESET` 正反例；
- **跨平台编译**：CI/手工执行 `GOOS=windows go build ./internal/model/...` 与 `GOOS=linux go build ./internal/model/...` 均通过；
- `httpstream_test.go`：`IsRetryableStatus`（408/429/500/503/400/401/404）；`MaxRetryAttempts=1` 时 5xx 单次即返回（参照现有 `TestDoRequestDoesNotRetryStatus600` 模式）；
- `openai_responses/provider_test.go`：`Compact` 首次 500 → 第 2 次成功；连续 500 → 恰好 2 次外层尝试后报错（httptest server 计数）；
- 现有 4 个 provider 传输层重试测试（§3.4 列表）：**保持不动、必须通过**，作为 httpstream 能力的留存覆盖；
- `internal/execution` 装配层：`providerHTTPOptions` 在 **`RequestTimeout == ""` 与已配置两种形态下**返回值均断言 `MaxRetryAttempts == 1`；
- `internal/execution/session_events_test.go`：扩展现有 `TestSessionStreamProviderRetryEventIncludesBackoffDetails`，补一个非 `server_error` reason（如 `rate_limited`/`timeout`）透传到 `provider.retrying` payload 的用例。

### 5.3 手工故障注入（开发机）

本地 httptest/代理将 codex baseURL 指向故障端点：前 2 次返回 500、第 3 次正常流式响应。观察：

1. run 日志出现 `provider_retry` attempt=2、3，reason=server_error，随后正常 `text_delta`，turn 成功；
2. 全程无提前终态 error；
3. 持续全 500 时，attempt 2..5 各一次后退场，turn 报错信息语义清晰。

## 6. 风险与后续可选清理

| 项 | 说明 |
|---|---|
| 必然失败的 turn 挂起时间变长 | HTTP 500 从"~0.6s 后失败"变为最坏约 2.5 分钟（全 idle 约 4 分钟，§1 上界）；用户可通过既有 cancel 中止，`provider.retrying` 事件使过程可见 |
| `collectCompactionSummary` 静默降级 | 3×200ms → 1 次 + 超时快重试（§2 非目标，已全局检索确认无其他直连 `provider.Stream` 的生产路径）；如后续认为不可接受，再抽共享 retry helper 给 summary/compact 复用 |
| `ProviderError` 字符串匹配残留 | 三个子串（`server_error`/`overloaded`/`rate_limit`）为过渡方案；后续可给 `ProviderError` 加结构化 `Retryable`/kind 字段由 provider 显式标记（`model.go` 的注释本就预留了"HTTP error status"语义），届时分类函数简化 |
| 窄集合连接错误可能漏判 | 某些真实瞬时错误（如特定 DNS 临时失败）不在窄集合内 → 不重试，是刻意的保守方向（宁可少重试，不可对 TLS/证书类永久错误做 75s 无效退避） |
| Windows errno 维护 | `wsaECONNREFUSED=10061` 为自定义常量（stdlib 未导出）；若 Go 后续版本导出官方符号，迁移一行即可；平台拆分文件保证 Unix 行为不受影响 |
| `MaxRetryAttempts: 1` 在装配层而非构造器 | 若未来新增不经 `providerHTTPOptions` 的 provider 装配路径，传输层重试会复活；§5.2 双形态装配断言缓解 |

## 7. 完成定义（DoD）

1. §1 成功标准 1-7 全部满足；
2. §5.1/§5.2 测试全部新增/保持并通过，Step 1 回归测试在修复前必失败；
3. `GOOS=windows` 与 `GOOS=linux` 下 `go build ./...` 均通过；
4. §5.3 手工故障注入观察项 1-3 全部符合；
5. `go test ./...` 全绿。
