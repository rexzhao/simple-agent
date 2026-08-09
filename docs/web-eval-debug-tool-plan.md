# `web.eval` 内部调试工具开发方案

> **状态：阶段 1、2、3A、3B 已完成并验收；阶段 4 Blob/诊断日志尚未实现。**本轮把 connection-bound Broker 接入 Agent，并仅在运行时条件满足时动态提供内部 `web.eval`；不引入 Blob 或独立 code/result 诊断日志。

## 1. 目标与边界

目标是让 Agent 在明确启用服务端 Debug 总开关、且目标 Session 属于固定项目
`project-f25c5aac78f681b52aabf5c0` 时，获得一个动态注册的专用工具：

```text
web.eval
```

Agent 可以用它在当前 Web 调试页面中执行少量、有界的 JavaScript，以观察和诊断：

- Web 端 Local Replica、Sync Runtime、Repository 和 command facade 的状态；
- 当前页面的 DOM 与界面状态；
- 可从页面访问的 Web/Go 执行层数据，并自行进行对比；
- 页面或 Session 切换后的数据一致性和空闲状态。

项目 ID 是**功能过滤条件**，不是 Debug 开关。服务端 Debug 总开关仍默认关闭；只有总开关开启并且 Session 确实属于上述项目时，才允许注册工具。

本任务只做即时诊断，不做完整同步取证系统。以下内容不再是本任务的交付目标：

- 完整 deterministic sync trace 或 replay bundle；
- Session baseline、snapshot barrier 级别的 Trace 录制；
- 独立 replay runtime、TraceReplayInput 或第二套同步归并实现；
- 大量预定义 probe、selector 或领域专用调试工具；
- 自动跨 epoch 重试或自动重放 JavaScript。

## 2. 工具契约（3B 已完成并验收）

`web.eval` 是内部 canonical tool name。它不写入 `session.EnabledTools` 或配置的
`tools.enabled`，而是在每次 Agent runtime 准备时按权威 SessionStore、目标项目、服务端
`web_eval` 开关和 Service 上的 live executor attachment 动态追加到当次 model request。
以下是本轮的最小参数设计：

```typescript
interface WebEvalArguments {
  code: string
  timeout_ms?: number // bounded；省略时使用服务端默认值
}
```

`code` 必须是非空、合法 UTF-8、最多 64 KiB UTF-8 bytes 的字符串；`timeout_ms` 默认为
5000，范围为 100..30000 的整数；schema 禁止未知字段，工具实现还会独立严格校验参数。
schema 的 `maxLength` 只能作为 provider-facing 提示，UTF-8 byte 上限以 runtime 校验为权威。
描述明确这是高风险的任意同源 JavaScript，支持 async；表达式直接返回 completion value，
statement 必须显式 `return`；不接受 page/session/connection selector，不 retry/replay，
同步死循环无法由浏览器 timer 中断。

运行时准备绑定当时的 Service registration generation/owner。真正调用前重新加载 caller
session 并确认项目仍为目标项目、attachment 仍是同一个 current registration；关闭或替换
后稳定失败，绝不迁移到新 executor。Broker 的 typed failure 以紧凑结构化 JSON 返回，
浏览器成功/失败结果保留 execution identity、elapsed、value（包括 `null`）和可选 console。
Go 和 Web presentation 都只显示 timeout/code bytes（或 hash）安全摘要，不显示 code 正文；
Go 先对 requested/started/history DTO 做防御性摘要，Web 同时兼容原始参数和已摘要 shape，
畸形输入 fail closed。正常 Agent tool 请求/结果仍按现有 durability 保存，这不等于阶段 4
的独立诊断 logging。

不要求也不接受 `session_id`、`page_instance_id`、`probe` 或 `selector`。Agent 通过任意 JavaScript 自行：

- 切换页面、项目或 Session；
- 读取 Web 内部数据并分析 DOM；
- 调用调试入口提供的控制能力；
- 在其能够读取的 Web/Go 执行层数据之间做对比。

工具注册时应明确公布 `code` 和有界超时的 schema；超时上限、代码大小上限和结果大小上限由服务端统一限制，不由页面自行放宽。

## 3. Web 调试面

符合条件的页面只暴露一个统一入口：

```text
window.__SAI_DEBUG__
```

它应当由模块闭包构造，仅向入口提供调试所需的稳定对象和控制能力，而不是把大量实现对象散落到 `window`：

```text
window.__SAI_DEBUG__
  - LocalReplica
  - SyncRuntime
  - repositories
  - commandFacade
  - appState
  - selectProject(projectID)
  - selectSession(sessionID)
  - waitIdle(options?)
```

上面的名称表示能力类别，最终 API 形状以实现阶段确定；每项都必须遵守现有对象的生命周期和权限边界。入口只在服务端总开关开启、Session 项目匹配且页面处于调试模式时存在，条件不满足时应不存在或不可用。

`web.eval` 中的任意 JavaScript 仍可直接访问当前页面的 `window`、`document` 和 DOM。统一入口不是沙箱，也不限制 Agent 通过浏览器同源能力观察或操作页面。

## 4. Go executor 与多标签页 lease

Go 侧只维护一个当前 Web debug executor，不让 Agent 选择页面或连接：

1. 符合条件的 Web 页面连接后注册为候选 executor；
2. 内部维护单一 executor lease；推荐把最近获得焦点的符合条件页面作为当前 lease；
3. Agent 的一次 `web.eval` 固定绑定一个 executor connection；执行期间不迁移到另一标签页；
4. 多标签页的焦点变化、连接关闭、刷新和重新注册只更新内部 lease，不改变工具参数；
5. 调用时没有可用 executor 时，返回稳定 typed 错误 `web_debug_not_connected`。

连接断开、页面刷新或 executor epoch 变化都使本次执行失败。服务端绝不跨连接、跨刷新或跨 epoch 自动重放代码，也不把失败伪装成页面切换后的成功结果。

## 5. 执行、结果与数据边界

`web.eval` 必须支持 async JavaScript，并提供以下可观察结果：

- 有界执行超时；
- executor 断线或页面失效错误；
- 返回值封装；
- `console` 输出封装；
- JavaScript exception/error 封装；
- 执行耗时和必要的执行 identity。

结果统一经过 bounded serializer。Broker wire 允许最多 64 KiB 的 inline JSON，最终 Agent tool content
保持在现有 limiter 下方的 48 KiB；不会把大数据直接塞进协议，也没有 Blob descriptor。serializer
对深度、对象键、数组元素、字符串、console 条数和参数数均有硬界，并对循环引用、DOM Node、
`Error`、`BigInt`、`Function`、`Symbol`、`undefined`、非有限数字和 `Window` 返回明确的
summary/error 标记。Blob 数据面是阶段 4 的后续工作。

断线、超时、序列化失败和脚本异常必须保留可区分的 typed error/diagnostic 信息。除内部 lease 选择外，broker 不替 Agent 修改代码、重试或重放。

## 6. 权限与误启保护

任意 JavaScript 是高风险能力：3A executor 没有虚假 sandbox，代码拥有当前页面同源范围内的 `window`、`document`、DOM 以及页面实际暴露能力。浏览器侧 timeout 只约束 async completion；同线程中的同步死循环无法被 timer 中断，这是任意同源 JavaScript 的固有限制。Go Broker 仍有独立的服务端 timeout、connection/epoch/lease 校验和断线失败语义。

安全边界由以下条件组成：

- 服务端 Debug 总开关默认关闭；
- 只对固定项目 `project-f25c5aac78f681b52aabf5c0` 做严格项目过滤；
- 非目标项目不注册 `web.eval`，即使页面存在也不能通过工具调用；
- 生产配置不能因为普通项目 ID 或页面参数而误开启总开关；
- 固定内部调试项目可以不做每次执行确认，但必须保留上述默认关闭和项目过滤；
- 工具注册、executor lease 和执行请求都必须再次校验调试条件，不能只信任页面自报状态。

## 7. 观测与安全边界

3A/3B 不记录 `code`、result 正文或独立诊断日志，不暴露 capability/ticket。每次 Broker execution
开始时重新校验当前 lease 的 SessionStore authority；Agent tool 调用前还会重新加载 caller session、
校验 target ProjectID 和同一 Service registration。该校验不是执行期间的持续订阅。执行身份固定在
开始时的 connection/page/epoch/session：focus/lease 切换不会取消、迁移或重放已经绑定的 execution。
只有 refresh/re-register、unregister、connection disconnect/watcher cancel 或 Broker Close 会取消
绑定执行。Go 和 Web presentation 只保留不含 code 的安全摘要；正常 Agent tool durability 不等于
阶段 4 的独立诊断 logging。

## 8. 分阶段实施

### 阶段 1：executor 注册与 lease（已完成/已验收）

- 当前实现范围包括默认关闭的服务端 `debug.web_eval_enabled` 高危开关、Web server assembly 的 debug broker 注入，以及配置失败时不 fail-open 的启动边界。
- 当前实现范围包括现有 WebSocket 上专用 typed 的注册、注销、焦点更新及服务端确认消息；字段仅包含 `page_id`、`page_epoch`、`session_id`、`focused`，并沿用协议的 Decode/Encode、校验、fixture 和边界规则。
- 当前 Go/Web 两端共享的 V1 typed debug control contract 已接通；阶段 1 只提供纯协议支持，不增加页面注册行为、React/UI 或 `window.__SAI_DEBUG__`。
- 当前实现范围包括固定项目 `project-f25c5aac78f681b52aabf5c0` 的服务端 SessionStore 权威校验，不接受客户端 `project_id`；非目标、缺失、删除或畸形身份不能成为候选。
- 当前实现范围包括每个 live connection 一个候选、全局唯一当前 lease，以及最近焦点优先、注销、连接 context 取消、刷新 epoch 和 broker Close 的确定性失效/回退；提供不带页面选择参数的 `Current` / `Acquire` API 和 `web_debug_not_connected` typed 错误。
- 当前实现范围包括 debug handler 只消费 debug control 消息，现有 command/subscription 委托路径不变。

阶段 2、3A、3B 已完成并验收；阶段 4 Blob/serializer 增强/logging 尚未实现。3A 的 serializer v1 已完成并验收，但仍只服务于 inline execution result。

### 阶段 2：`window.__SAI_DEBUG__`（已完成并验收；本轮实现）

- 新增独立 `web/src/debug/webDebugBridge.ts` infrastructure 模块，由 `createSyncApplication` 组合进唯一应用对象图；复用现有 RuntimeTransport、signals、repositories、LocalReplica、SyncRuntime 和 CommandFacade。
- 浏览器侧严格使用固定项目 `project-f25c5aac78f681b52aabf5c0`、非空当前 Session，以及目标 SessionIndex 的 ready authoritative summary 作为本地候选；监听项目/Session signals、目标 SessionIndex publication、WebSocket ready/close、focus/blur 和应用 stop/dispose。
- 实现 crypto UUID 的稳定 `page_id` 与页面加载唯一 `page_epoch`（提供测试 generator seam）；按 connection generation、page、epoch、session 严格匹配 `debug_registered`，处理 pending focus reconcile、断线/切换/authority 失效/服务端 `web_debug_*` 错误和 best-effort unregister，避免无界重试。
- 仅在匹配 server ack 后挂载唯一 `window.__SAI_DEBUG__`；surface 提供 `replica`、`runtime`、`repositories`、`commandFacade`、只读 `appState`、`selectProject`、`selectSession` 和有界/可取消/稳定观察点的 `waitIdle`。`waitIdle` 同时等待当前 ProjectIndex、SessionIndex、Session Content（含 data availability 与 history loading）不再 loading；stale/error 作为可观察的终态，不阻断 idle。导航只使用 ProjectIndex/SessionIndex authority 和现有 signals，不调用 DOM click 或新增 HTTP。
- 为 runtime/command facade 增加 transport-neutral 的只读 debug snapshot seam；debug control error 在同步 runtime 和 command facade 中明确忽略，不改变普通 resource/command 路径或 `ApplicationPageServices`。

### 阶段 3A：connection-bound JS execution channel（已完成并验收）

- 在既有 V1 typed debug control channel 上增加 `debug_execute`（仅 server→browser）和 `debug_execution_result`（仅 browser→server），双方共用字段、identity bounds、one-of status、UTF-8 code 上限、100..30000ms wire timeout、64 KiB inline result budget、fixtures 和 malformed/size validation。
- Broker 提供只接受 `context`、`code`、`timeout_ms` 的 execution API。每次调用先做 SessionStore authority check，再在锁内把唯一 execution 原子绑定到当前 live connection、page、page epoch、session；一次最多一个 in-flight execution。busy、未连接、发送失败、超时、取消、refresh/re-register、unregister、watcher cancel、disconnect 和 Close 都有稳定 typed failure，结果只接受同一 connection 与完整 lease identity 的匹配 execution ID，绝不 replay。focus/lease 切换不会取消已绑定的 execution。
- 浏览器 executor 只在当前 registered generation 和完整 identity 匹配时执行；同页 busy 返回 typed failure。使用 AsyncFunction：优先以 expression completion value 支持 `await`，表达式解析失败后采用 body 语义，statement 调用者必须显式 `return`。代码可访问 `window`、`document`、DOM 和 `window.__SAI_DEBUG__`；执行期间捕获五类 console，并在 throw、timeout、stop、dispose、registration lifecycle 变化和 generation 改变时恢复原 console。timeout 会立即释放 active slot 并只发送一次 timeout；旧 Promise 不会再发送结果，但无法取消脚本及其后续副作用，过期脚本与新执行之间无法对全局 console/DOM 副作用提供虚假隔离。
- serializer v1 对 primitive、undefined、非有限数字、BigInt、Function、Symbol、Error、循环引用、DOM Node 和 Window 做有界 inline summary；达到边界明确标记 truncated/summary，不调用 getter/toJSON，不因 getter/proxy 异常破坏清理。浏览器 timeout 只限制 async completion，同步死循环无法由同线程 timer 中断；Go 仍有独立 timeout。
- CSP 默认保持 `script-src 'self'`；只有 server 启动时读取的 `debug.web_eval_enabled=true` 才对静态/SPA 响应增加 `'unsafe-eval'`，API 响应仍不放宽。任意 JS 是高风险同源能力，不是 sandbox。

### 阶段 3B：Agent tool（已完成并验收）

- 已接入动态 runtime schema：仅目标 ProjectID、服务端开关开启且 Service 有 live Web
  executor attachment 时可见；runtime 入口先从 `session.EnabledTools` 移除 canonical
  `web.eval`，再做 child/builtin/session/MCP 选择，因此 child 不继承它，runtime metadata
  也永不持久化它；动态 schema/executor 只由 attachment 和权威 target session 决定。
- 已接入 owner/CAS-safe Service attachment 和 Server assembly/Close 生命周期；runtime
  绑定准备时 registration，调用前重新加载 session 并拒绝删除、项目变化、关闭或替换后的
  stale registration，不迁移、不 replay。
- 已接入独立严格参数校验、Broker typed error 与浏览器 structured JSON result、context
  cancellation、busy/closed/disconnected 语义，以及 Go/Web 两层防御性安全 presentation 摘要。
  executor payload 在 adapter 边界重新校验 status、one-of、identity、value/error 和 console
  JSON，不可信 payload 统一 fail closed 为稳定 generic error。OpenAI Chat 适配器补齐合法
  provider name mapper；Responses/Anthropic 复用既有 mapper。
- registration 的 current check 是调用线性化点：替换先发生则旧 owner 稳定失败；检查后替换
  只允许已经绑定的旧 owner 完成一次，绝不迁移或 replay，也不跨浏览器执行持 Service 锁。
- 本阶段仍只使用有界 inline output（Broker wire budget 为 64 KiB，最终 Agent tool content
  保持在现有 limiter 下方的 48 KiB），不做 Blob、不增加独立 debug/diagnostic logging；
  阶段 4 仍未实现。

### 阶段 4：Blob、增强 serializer 与诊断 logging（未实现）

- 大结果 Blob descriptor/HTTP data plane 未接入。
- Stage4 的 Blob 生命周期、serializer 增强和最小诊断日志未实现；本轮不记录 code/result 正文。

## 9. 验收条件

至少通过以下单元、集成和 E2E 验收：

1. 服务端总开关关闭时不注册工具；开启但 Session 不属于
   `project-f25c5aac78f681b52aabf5c0` 时也不注册工具；
2. 目标项目的符合条件页面能动态注册 `web.eval`，页面离开、刷新或断线后 lease 正确失效；
3. 多标签页并发连接只存在一个当前 executor，推荐最近焦点页面，Agent 不需要页面参数；
4. 一次执行固定在一个 connection；断线、刷新或 epoch 变化返回失败且不自动重放；无执行端返回
   `web_debug_not_connected`；
5. 3A 中 async JS、正常返回、console、异常、Go/浏览器超时、断线和 bounded inline result 都可观察且类型可区分；Blob descriptor 属于未实现的阶段 4；
6. serializer v1 能处理循环对象、DOM Node、Error、BigInt、Function 和 Window，不递归溢出、不泄漏未界定的大结果；
7. 权限测试证明任意 JS 的同源/DOM 能力按文档工作，同时非目标项目无法注册或调用工具；
8. E2E 中 Agent 通过任意 JS 切换项目/Session，检查 DOM、LocalReplica 和可读取的执行层数据，并能调用 wait-idle 或等价调试能力；
9. 并发执行、焦点切换、lease 抢占、页面断线、超时和大结果场景均不会跨页面串结果；
10. 3A 不记录 code/result 正文，也不实现阶段 4 的诊断日志；未来阶段 4 的最小日志与 Blob 引用不能回溯补写本轮执行；
11. 非调试页面和正常 Session 页面不依赖 `__SAI_DEBUG__`，主同步路径不引入完整 Trace/Replay 运行时。
