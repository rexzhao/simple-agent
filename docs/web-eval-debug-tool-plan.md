# `web.eval` 内部调试工具开发方案

> **状态：主同步重构已完成；阶段 1 已完成并验收（executor 注册与单 lease 基础）。**本轮只处理阶段 1，不提前实现阶段 2、3、4。它是一个面向固定内部调试项目的最小 Agent 调试入口，不扩展 resource subscription/数据同步语义，使用独立 debug control 通道，也不反向改变同步架构。

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

## 2. 工具契约

工具参数保持最小：

```typescript
interface WebEvalArguments {
  code: string
  timeout_ms?: number // bounded；省略时使用服务端默认值
}
```

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

结果统一经过 bounded serializer，至少能安全处理：

- 循环引用和深层对象；
- DOM Node；
- `Error`；
- `BigInt`；
- `Function`；
- `Window` 及其他不可枚举或不可序列化对象。

serializer 必须有深度、元素数量、字符串长度和总字节数边界；超出边界时返回可识别的截断/摘要表示，而不是阻塞或递归溢出。结果较小时以内联 JSON 返回；较大结果复用现有 HTTP Blob 数据面，工具结果只返回 Blob descriptor。Blob 必须继续使用现有鉴权、大小、校验和生命周期规则。

断线、超时、序列化失败和脚本异常必须保留可区分的 typed error/diagnostic 信息。除内部 lease 选择外，broker 不替 Agent 修改代码、重试或重放。

## 6. 权限与误启保护

任意 JavaScript 是高权限能力：它拥有当前页面同源范围内的完整 DOM 读取和操作权限，且可以使用页面实际暴露的 Web 应用能力。文档和产品说明不得声称它被沙箱化、只能调用白名单 probe，或能隔离页面副作用。

安全边界由以下条件组成：

- 服务端 Debug 总开关默认关闭；
- 只对固定项目 `project-f25c5aac78f681b52aabf5c0` 做严格项目过滤；
- 非目标项目不注册 `web.eval`，即使页面存在也不能通过工具调用；
- 生产配置不能因为普通项目 ID 或页面参数而误开启总开关；
- 固定内部调试项目可以不做每次执行确认，但必须保留上述默认关闭和项目过滤；
- 工具注册、executor lease 和执行请求都必须再次校验调试条件，不能只信任页面自报状态。

## 7. 最小诊断日志

每次调用只记录足以重建一次诊断结论的最小信息：

```text
time
session/tool-call identity
executor connection identity
page identity
code 或 code hash
result 或 error
elapsed time
Blob reference（如有）
```

`executor connection identity` 和 `page identity` 是内部日志字段，不是工具参数。日志不得因此变成完整 Session trace；不记录无关页面的同步内容，也不把大结果正文复制进日志。所有执行失败都要留下明确原因。服务端不得跨 epoch 重试，日志中应能看出执行绑定的 connection/epoch 和失败类型。

## 8. 分阶段实施

### 阶段 1：executor 注册与 lease（已完成/已验收）

- 当前实现范围包括默认关闭的服务端 `debug.web_eval_enabled` 高危开关、Web server assembly 的 debug broker 注入，以及配置失败时不 fail-open 的启动边界。
- 当前实现范围包括现有 WebSocket 上专用 typed 的注册、注销、焦点更新及服务端确认消息；字段仅包含 `page_id`、`page_epoch`、`session_id`、`focused`，并沿用协议的 Decode/Encode、校验、fixture 和边界规则。
- 当前 Go/Web 两端共享的 V1 typed debug control contract 已接通；本轮仍只提供纯协议支持，不增加页面注册行为、React/UI 或 `window.__SAI_DEBUG__`。
- 当前实现范围包括固定项目 `project-f25c5aac78f681b52aabf5c0` 的服务端 SessionStore 权威校验，不接受客户端 `project_id`；非目标、缺失、删除或畸形身份不能成为候选。
- 当前实现范围包括每个 live connection 一个候选、全局唯一当前 lease，以及最近焦点优先、注销、连接 context 取消、刷新 epoch 和 broker Close 的确定性失效/回退；提供不带页面选择参数的 `Current` / `Acquire` API 和 `web_debug_not_connected` typed 错误。
- 当前实现范围包括 debug handler 只消费 debug control 消息，现有 command/subscription 委托路径不变。

阶段 2、3、4（`window.__SAI_DEBUG__`、`web.eval` 执行、JS/serializer/Blob/logging）本轮未实现。

### 阶段 2：`window.__SAI_DEBUG__`

- 在模块闭包内组装 LocalReplica、SyncRuntime、repositories、command facade 和 app state；
- 提供项目/Session 选择和 wait-idle 等必要控制能力；
- 只在服务端总开关和固定项目过滤同时满足时暴露入口；
- 明确任意 JS 的同源/DOM 权限，不引入虚假的 probe 沙箱。

### 阶段 3：`web.eval` broker/tool

- 动态注册专用 Agent 工具；
- 实现 `code` 与 bounded `timeout_ms` 校验；
- 将一次调用绑定到当前 executor，支持 async JS；
- 封装返回值、console、异常、超时、断线和 `web_debug_not_connected`；
- 禁止断线、刷新或 epoch 变化后的自动重放。

### 阶段 4：serializer、Blob 与 logging

- 实现 bounded serializer 及循环对象、DOM Node、Error、BigInt、Function、Window 的稳定封装；
- 小结果 inline，大结果接入现有 HTTP Blob 数据面；
- 写入最小诊断日志并保留 Blob 引用；
- 验证大小、深度、数量、超时和日志边界不会阻塞 Agent 或 Web 执行路径。

## 9. 验收条件

至少通过以下单元、集成和 E2E 验收：

1. 服务端总开关关闭时不注册工具；开启但 Session 不属于
   `project-f25c5aac78f681b52aabf5c0` 时也不注册工具；
2. 目标项目的符合条件页面能动态注册 `web.eval`，页面离开、刷新或断线后 lease 正确失效；
3. 多标签页并发连接只存在一个当前 executor，推荐最近焦点页面，Agent 不需要页面参数；
4. 一次执行固定在一个 connection；断线、刷新或 epoch 变化返回失败且不自动重放；无执行端返回
   `web_debug_not_connected`；
5. async JS、正常返回、console、异常、超时和结果截断/Blob descriptor 都可观察且类型可区分；
6. serializer 能处理循环对象、DOM Node、Error、BigInt、Function 和 Window，不递归溢出、不泄漏未界定的大结果；
7. 权限测试证明任意 JS 的同源/DOM 能力按文档工作，同时非目标项目无法注册或调用工具；
8. E2E 中 Agent 通过任意 JS 切换项目/Session，检查 DOM、LocalReplica 和可读取的执行层数据，并能调用 wait-idle 或等价调试能力；
9. 并发执行、焦点切换、lease 抢占、页面断线、超时和大结果场景均不会跨页面串结果；
10. 每次调用的最小日志包含时间、identity、connection/page、代码或 hash、结果/错误、耗时和 Blob 引用（如有），且没有跨 epoch 重试记录；
11. 非调试页面和正常 Session 页面不依赖 `__SAI_DEBUG__`，主同步路径不引入完整 Trace/Replay 运行时。
