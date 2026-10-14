# Web UI 设计分析（第一阶段：log 界面）· v2

目标：为 sai 建一个 Web 界面。第一阶段**不做聊天渲染**，做一个
**log 视图**：一个用户输入框、一个会话状态区、按顺序显示已持久化的
记录和正在输出的 item。第一性目标是 **Web 与后端数据一致性**——页面
上看到的东西必须可证明地等于会话的真实记录流。

v2 并入设计评审结论：两项前置工程（§0 会话锁、§3 CSRF）、hub 挂接
的边界补严、重放成本正视化、8 条协议细化。

## 0. 前置工程

### A：会话级单进程锁（最高优先，先于一切 web 代码）

现状：`jsonl/log.go` 以 `O_CREATE|O_RDWR` 打开（**无 O_APPEND**），
写入靠各自进程的 offset 推进。两个进程打开同一会话 → 从同一 offset
互写覆盖；`recover()` 还会把对方刚追加的行当撕裂尾截断。叠加分段
（`log.jsonl.next`、归档改名）破坏面更大。Web 模式让这条路径极易踩：
一个终端 `sai -session X`，另一个 `sai -http ... -session X`。

方案（storage 层，所有入口共用）：

- **OS 级排他锁**（Unix `flock`/`fcntl`、Windows `LockFileEx`）由
  内核保证原子性——pid 文件方案存在经典竞态（A 判陈旧→B unlink→
  A 重建→双持有者）
- pid/启动时刻仅写入锁文件**用于错误信息**（报出持有者）
- 锁随 `Close` 释放；进程崩溃由 OS 自动回收
- 互斥 UX：`-http` 启动打印 "serve mode: REPL disabled"

### B：POST 端点的最小 CSRF 防护

`POST /api/send` 能触发 shell 执行——浏览器里任意页面都能向
`http://127.0.0.1:7420` 发**简单请求**（`text/plain`/
`form-urlencoded` 不触发 CORS 预检），等于跨站直投本机命令执行面。
三件套：

1. 写端点**强制** `Content-Type: application/json` 并校验——JSON
   是预检类型，未获 CORS 允许的跨站 POST 发不出去
2. 校验 `Origin`（有则必须 loopback）与 `Host` 为 loopback
3. **不给任何宽松 CORS**（`/api/stream`、`/api/state` 同理，防跨站读）

`/api/send`、`/api/compact`、`/api/stop` 永不接受 GET。token 方案
列为可选加固，v1 可不做。

### C：记录边界的 UTF-8 规范化（web 前必修的存量 bug）

`json.Marshal` 会把字符串里的非法 UTF-8 字节替换为 U+FFFD，而内存里
是原始字节——含非法字节的记录（`tools/shell.go` 直接取命令输出，
全程无 UTF-8 校验；`cat` 二进制/非 UTF-8 日志/git diff 二进制文件
均可触发）会出现**内存 ≠ 落盘**的存量不一致（resume 后内容变），
且摘要链两端会哈希不同字节、**永久假 ❌**。修复：在记录进入持久层
的边界（工具结果入记录处或 append 前）用 `strings.ToValidUTF8`
规范化，且折叠/投递只用 append 返回的 stored 记录——一处修复同时
解决落盘一致性与链路一致性。

## 1. 现有后端盘点（不新增任何一致性原语）

一致性所需的原语在 agent 层已经存在，Web 层只是换一个消费者：

| 需求 | 现有 API | 说明 |
|---|---|---|
| 恰好一次、有序的记录流与瞬态事件 | `Session.Stream()` | **单一有序会话流**（milestone A）：seq 在会话锁内分配，`Initial()` 原子快照，之后 RecordCommitted/Transient/Status 按全序推送（见 §4.3）。旧的 `Subscribe()`/`Events()` 双通道已在 step 3/4 删除 |
| 输入提交 | `Session.Send(text)` | 检查、落盘、running 翻转在一个锁段；并发第二个提交被拒绝（天然 409 语义） |
| 状态快照 | `Session.SnapshotStream()` | records + 瞬态槽 + steer 队列 + 状态，同一锁段原子拷贝（`SnapshotStreamState()` 为不拷 records 的轻量变体）——连接时刻的 in-flight item 内容直接可取 |
| 停止 | `Session.Stop()` | run 以中断收尾，无失败记录，user.input 保持未回答 |

注意快照的 records 是**进程内扁平流**（压缩后长于磁盘
活跃段，与 CLI 所见一致）。v1 的 log 视图就显示这个扁平流——与
`sai -session X` resume 看到的完全相同。**已知用户可见差异**：重启后
扁平流回到磁盘活跃段，历史显示变短——UI 显式画出压缩边界（"compaction
⋯ N archived records (log-*.jsonl)" 一行），不让用户以为历史丢了。

## 2. 架构

```
cmd/sai            -http <addr> 启动 serve 模式：web server + debug REPL
                   -http-static <dir> 在 /chat 托管独立构建的聊天客户端
internal/web       server.go   HTTP mux + 生命周期 + 关停（Stop+Close，
                               flush 日志、不留 log.jsonl.next、释放会话锁）
                   hub.go      会话流的单消费者 fan-out（全程非阻塞，订阅单一有序流）
                   wire.go     model.Event → tagged JSON（事件无 json tag）
                   static/     index.html + app.js + style.css（go:embed，
                               托管在 /log）
clients/chat/      独立构建的 assistant-ui 聊天客户端（React+Vite，
                   dist 构建产物；不进 go:embed，自己的 npm 构建链；
                   见 clients/chat/README.md）
```

- **进程模式**：`sai -http 127.0.0.1:7420` 启动 web server，同时保留
  一个**纯命令 REPL**（debug 用）：stdin 只接受 `/` 开头的命令，自由
  文本一律走 web 输入框，非 `/` 输入提示后丢弃。命令集后续讨论
  （v1 至少 /quit、/help）。debug REPL 从不 `Send()`，因此 hub 仍是
  唯一的 events drainer——单消费者约束不需要改。跨进程同会话由
  §0 的锁解决。

### 2.1 debug 命令：/tail（终端 viewer）

```
/tail                      # 观看当前 serve 的会话（最常用）
/tail <project> <session>  # 观看同 data root 下其他会话（只读重放）
/tail off                  # 关闭 viewer
```

- **当前会话 = 完整实时**：viewer 自行 Stream()（Initial 重放尾部
  20 条，同 resume 约定）+ 实时 Records，复用 cliApp 渲染栈
  （render.go + liveWriter，ItemID 去重/parked 同构）；delta 经
  **hub tap** 获得——hub 排干后拷贝给 tap，tap 有界缓冲溢出丢弃
  并显示丢弃计数（"/tail dropped N deltas"，不静默）。**viewer 的
  记录路径同样有损**：有界缓冲 + 溢出丢弃 + "tail lost records;
  重新 /tail 可恢复"提示——serve 模式下"调试 viewer 卡住 agent"
  必须不可发生（与 §4.4 承诺对齐，/tail 不再是例外）；丢弃可恢复，
  记录永远可重读。终端写入在自己的 goroutine，不由 hub 代劳，
  tap 拷贝在 hub 的无损记录路径之后
- **其他会话 = 只读磁盘重放**（归档段+活跃段，尾部 20 条），不做
  跨进程 live tail；只读打开不取 §0 排他锁
- **同一时刻至多一个 viewer**：重复 /tail 先关旧的（**必须 Stop()
  掉记录订阅**——有损模式下这是释放挂起终端的唯一手段）；目标为
  当前会话时复用进程内 session 对象，不重新 Open（双 Log 对象在同
  进程同样会损坏）
- **零依赖**：SSE 是纯 HTTP；无 WebSocket 库、无前端框架、无构建
  步骤。静态资源 go:embed 进二进制。
- **单会话**：一个 server 进程一个会话（与 CLI 同构）。
  【已取代】多项目/多会话、路由前缀与装配见 `docs/multi-session.md`，
  以该文为准；本文的快照协议/摘要链/hub 语义仍然有效。

## 4. 一致性协议（核心）

### 4.1 记录流：摘要链断点 + 窗口重放，不做序号游标

**摘要链**（两端从解码后字段构造，避开 JSON 再序列化的转义差异；
字段元组**纳入一切客户端可渲染或可作键的字段**，采用**长度前缀**
编码保证单射，空值/零工具调用的形式写死；链版本化，元组冻结——
将来新增可渲染字段 = 升版本 = 所有客户端重同步）：

```
h₀ = SHA256("sai/v2/" + sessionID)
输入串 = 每字段以十进制长度前缀拼接：
        itemID, type, ts, content, reasoning, is_error,
        [tool: name, arguments, call_id]…, provider, model, cwd, reasoning_level
hᵢ = SHA256(hᵢ₋₁ + "\n" + 输入串)
```

链折叠的是 **stored 记录**（append 之后，ts 已补）；前置工程 C
保证字段不含非法 UTF-8。**h 是进程生命周期内的链值，不是跨重启的
会话指纹**（重启后活跃段链在 compacted 处分叉，旧 h 必然 miss，
正确回落窗口；仅"进程内未发生过压缩"时重启后仍命中）。h 与所持有
记录同住内存/每标签独立 sessionStorage，刷新即窗口重放。

**连接**：`GET /api/stream?from=<h>`：

- hub 维护 `h → 位置` 映射（**启动时由 `Stream().Initial().Records`
  重建**）；为控内存可只保留最近 K 条前缀，旧 h miss → 窗口重放，
  这是有意降级非 bug
- **命中** → 只补发 h 之后的记录，页面追加，链继续
- **未命中/首次** → 初始窗口（尾部 N=200）+ 前缀锚点 hⱼ + 总数。
  **规则 A：miss ⇒ 丢弃全部已持有记录与链状态，从锚点重新折叠
  窗口**（不是续链——重启后旧链与新链分叉，续链会得混合视图）
- **规则 B：h 只在内存/每标签独立，不跨标签共享**（localStorage
  被另一标签写会导致 h 落后于所持记录，补发后缀被重复折叠 →
  假 ❌）

**from 不是必须正确的游标，而是可验证的缓存提示**（ETag 模式）：
错误的 h 只损失带宽，失败面是 ❌ + 重同步，不会是坏数据。**❌ 的
处置 = 丢弃 h、走窗口重放**；❌ 的语义是"漂移**或**陈旧提示"，
不一定是损坏。

**不变量（精确形式）**：页面 ≡ 锚点前缀（服务端提供）∪ 所收到的
记录流。EventSource 断线重连带内存 from，通常零重传。

**索引约定（消除 off-by-one）**：h_k = 折叠前 k 条记录后的链值
（h₀ 为链首）。数据窗口中记录的索引是 k+1..m，其**前缀锚点**即
h_k。`/api/history?before=h_k&limit=M` 返回记录 [k−M, k) 与新锚点
h_{k−M}，客户端校验 `fold(h_{k−M}, 返回记录) == h_k`——严格等式。
链首终结条件：`anchor == h₀` 或 `hasMore:false`。若两次请求间服务
端重启（链重建），history 返回明确 `stale_anchor`（409）让页面走
重同步，**不要**返回新锚点窗口（那会让严格等式失败，表现为漂移
而实际是重启）；hub 已截断的历史返回 `hasMore:false`，不返回半个
窗口。归档段是另一条链，其回填锚点语义待 P2 定。

摘要链校验的是页面**数据存储**，不是 DOM——有界 DOM 渲染（视口
外卸载）不影响校验，二者解耦。

### 4.2 In-flight item：ItemID 合并，record 取代 delta

行以 ItemID 为键。`AssistantMessageStartedEvent` 先于 record 提交
到达，带出 in-flight 行；delta 持续更新；**同 ItemID 的 record 到达后
取代** delta 累积内容，行转为已持久化样式；run 结束无 streaming 残留。

**中途连接**：Started 事件对"流已开始后才连上"的客户端已过去——
SSE 建连的**第一帧是 `snapshot`**：一次原子的 `SnapshotStream()`
（records 由 hub 镜像提供，Streaming/Running 槽、steer 队列与
running/compacting 状态同一锁段拷贝，§4.3）。Running 槽携带
`awaiting_approval`（§16.3：该工具调用正等待宿主审批决定——
`tool_approval_pending`/`tool_approval_resolved` 瞬态事件与之
同一临界区，重连客户端从快照重建审批卡）。“record 取代同 ItemID
streaming 行”的规则使任何重叠良性（同 ItemID 幂等覆盖）；此场景
写进文档并有测试。

**协议声明**：瞬态事件是 **best-effort**——任何时刻丢失只影响
"正在打字"的观感，最终 record 会取代它。正确性只挂在记录流上。

### 4.3 事件源：单一有序会话流（milestone A step 3）

hub 不再挂接 per-run 事件通道，而是**订阅会话的单一有序流**
（`Session.Stream()`，见 design §14.2）：seq 在会话锁内分配，
`RecordCommitted`/`Transient`/`Status` 三类事件按全序投递，hub 是
唯一的流消费者，帧序即 seq 序——旧的 wake/挂接/排干补丁全部删除。

- **帧映射**：`RecordCommitted` → record 帧（并折叠进摘要链）；
  `Transient` → event 帧（pressure/retry/compact notice 同时缓存进
  /api/state）；`Status` → status 帧
- **所有 status 帧的 running/compacting 都来自流**：hub 维护一份
  running/compacting 镜像（NewHub/resubscribe 时从订阅快照取初值，
  之后只由流内 Status 事件更新），`broadcastStatusLocked` 只读镜像。
  带外帧（Notice/ClearNotices）同样如此：hub 的 digest/count 天然
  滞后于会话，若 running/compacting 读实时值，帧会与流序自相矛盾
  （空闲帧抢在最后一条记录前面——正是当初 drainThenStatus 要防的）。
  位置型帧携带 hub 已处理到的 seq，快照比它新的客户端会跳过。
  **notice 帧例外**：带 notice 的帧对全部客户端无条件投递（快照帧
  不携带 Notice，seq 过滤会把它静默吞掉），且**省略位置字段**
  （running/compacting/digest/count）——发出 notice 时 hub 镜像可能
  滞后于新鲜客户端的快照，重发过时位置字段会把它 regress；客户端
  按缺席=no-change 处理（`compacting` 缺席时按 false——notice 只在
  压缩失败路径出现，此时无进行中的压缩，恰好正确；这是一条
  显式耦合，若未来 notice 出现在压缩进行中，需给帧补位置字段
  或改客户端语义）
- **重放过滤（seq 过滤）**：客户端连接时的快照是实时原子快照
  （seq S）：一切 seq ≤ S 的事件都已反映在快照里。hub 落后时
  （镜像 P < S），积压重放的 Transient/Status 帧（seq ≤ S）对该
  客户端跳过，否则同一段 delta 会出现两次（一次折进快照的
  Streaming 内容，一次重放）；record 帧豁免——快照的 records
  来自 hub 镜像（P），(P, S] 间的记录靠 record 帧补齐。流式 delta
  的状态变更与 seq 分配同锁段（§14.2），该不变量是结构保证
- **手动 /compact 的生命周期**：`StartCompact` 发
  CompactionStarted/Completed 瞬态事件 + Status{Compacting}，全部走
  流到达 hub——无需 handler 轮询或周期广播。**压缩 commit 本身不发
  独立的 record 帧**（§14.5：截断走 snapshot 重发）——SSE 消费者只在
  重发的 snapshot records 里看到 context.compacted，/log 页要到下一次
  snapshot/重连才渲染该条
- **run 开始的 Status 不丢弃**（§14.2）：它是消费者建立 run 生命
  周期机件的锚点；满则摘除订阅者（可恢复），绝不静默丢弃。
  （step 3 曾配有过渡性的 per-run 通道排空垫片，step 4 删除
  per-run 通道后一并移除——瞬态事件只在会话流上存在）
- **订阅被摘除（Lost）**：hub 的流订阅缓冲溢出时被 dispatcher 摘除；
  hub 重走 `Stream()`，从新快照重建镜像，并向所有 SSE 客户端
  **重发窗口快照**（resumed=false，客户端按规则 A 重新折叠），
  之后继续正常扇出。SSE 客户端无需感知——重发的快照与普通断线
  重连同路径
- **快照帧的原子性**：连接时的 snapshot 帧的状态部分由一次
  `SnapshotStreamState()`（瞬态槽 + steer 队列 + 状态，同一锁段拷贝，
  不拷 records——记录来自 hub 镜像）构造，不再是
  `Snapshot()` + `Running()` + `Compacting()` 三次取锁的拼接
  （旧写法在转换窗口内可能自相矛盾）

### 4.4 慢客户端（实现约束，非建议）

`subscription.deliver` 是**阻塞发送 + done 逃逸**——hub 作为唯一
记录订阅者一旦阻塞，**阻塞的是 committer（agent）**。因此 hub 的
fan-out **全程非阻塞**（`select` + `default` → 关闭该客户端连接），
per-client 有界缓冲（256）是唯一允许积压的地方。溢出 = 断连，
EventSource 重连走 §4.1 窗口/断点重放恢复。绝不允许写成
`ch <- msg`。**该承诺覆盖所有客户端**：SSE 页面与 /tail 终端
viewer（§2.1，有损+可恢复），无一例外。

### 4.5 多标签页/多客户端

同一会话可同时开 N 个 web UI，机制：**每个 SSE 连接向 hub 注册**
（各自的 Initial + Records，恰好一次由同一原语各自保证）；**瞬态事件
由 hub 单点消费会话流后扇出**（多标签页不是多个流订阅者——hub 是
会话唯一的流消费者，页面向 hub 注册；共享会话状态
无页间私有副本，唯一真相源是会话本身（记录流 + status 广播）。两个
标签页同时 POST /api/send：锁段保证恰一个成功，另一个 409，UI 在
RUNNING 时禁用输入并在 409 时就地提示；一个标签页停止/压缩，效果
经记录流到达所有页。慢标签页隔离见 §4.4。

## 5. 事件 wire 格式

`storage.Record` 自带 jsonl tag，直接上 SSE；字段全部 `omitempty`
——客户端**不区分"字段不存在"与"空"**。`model.Event` 结构体无 json
tag（内部类型不加），`wire.go` 做判别编码：

```json
{"kind":"snapshot","running":true,"streaming":{...},"records":[...]}
{"kind":"record","t":"assistant.message","item":"...","content":"..."}
{"kind":"event","t":"reasoning.delta","item":"...","text":"..."}
{"kind":"status","running":true}
```

`Send` 的 409 用稳定 wire code（如 `{"error":"run_active"}`），
不靠错误字符串匹配。

## 6. API

【已取代】路由已加会话前缀（`/api/s/<id>/...`）并新增
`GET /api/projects`，见 `docs/multi-session.md` §4；下表保留作
第一阶段的历史记录。

**顶层路由（现行）**：`/` 302 → `/chat`；`/chat` 聊天客户端
（`-http-static <dir>` 托管 clients/chat 构建产物；未挂载时是
指引页）；`/log` 零依赖 log 视图（go:embed）；未匹配的 `/api/...`
一律 JSON 404（不给页面重定向）。**CORS**：对 loopback http 来源
的 `/api/` 请求回预检应答与来源回声——聊天客户端开发流（Vite 在
另一 loopback 端口）需要读响应与预检 JSON POST；这是有意的信任
扩展而非削弱：serve 本就 loopback-only 无认证，本地进程本可直接
调用；非 loopback 来源照旧被拒。

| 路由 | 方法 | 作用 |
|---|---|---|
| `/` | GET | 静态页面 |
| `/api/stream` | GET | SSE：首帧 snapshot（含锚点/总数/运行态/in-flight），此后 record/event/status；`?from=<h>` 摘要链断点续传，未命中回退窗口重放 |
| `/api/history` | GET | `?before=<h>&limit=M` 滚动回加载：窗口前 M 条 + 新前缀锚点，可后向验证 |
| `/api/state` | GET | 摘要：session id、running、`RecordCount()`（进程内扁平口径）、in-flight item、**Compacting 槽**（`Snapshot()` 已含）、hub 缓存的最近 pressure/retry/**compact notice**（事件是瞬态的，agent 无可查历史，hub 自己缓存） |
| `/api/send` | POST | `{"text":"..."}`，强制 application/json + Origin/Host 校验；run 活跃 409 `run_active` |
| `/api/compact` | POST | **异步：handler 起 goroutine 立即返 202**（`Compact()` 持单飞槽直到摘要完成可能数十秒，同步会占住连接且前端无状态可显）。同步前置校验：run 活跃 409 `run_active`、`Snapshot().Context` 为空 409 `nothing_to_compact`（可同步判定，不进 goroutine）。**压缩生命周期（compacting:true/false、CompactionStarted/Completed）全部经会话流到达 hub**（§4.3），无轮询无周期广播。**前置校验是 TOCTOU，异步失败的唯一归宿是带外通道**：goroutine 在 `Compact()` 返回非 nil 时把错误交给 hub，由 hub 以 status 帧广播 `{"kind":"status","compacting":false,"notice":"compact failed: …"}`；`/api/state` 摘要携带最近一次 notice，断线期间打开的页面也能读到。异步 202 若无带外错误通道，语义就是"接受且不告知结果"，比同步阻塞更差 |
| `/api/stop` | POST | `Stop()`；**无 run 时幂等成功** |
| `/approval` | POST | `{"item","allow","reason"}`——回答待审批的工具调用（§16.3）；过 guardAPI + 会话解析（会话级码先行）；`approval_unknown` 404 / `approval_resolved` 409 |

v1 不做：会话列表、认证。默认 `127.0.0.1:7420`，仅 `-http` 显式启用。

## 7. UI 组成（log 视图，非聊天）

```
┌──────────────────────────────────────────────┐
│ session a3f2… · 128 records · RUNNING        │ ← 状态区：id/数量/running
│ ▓▓▓░░ context 71k/100k · retrying (2/4)      │    /压力/重试/重放量
├──────────────────────────────────────────────┤
│ 10:39:01 user.input      现在几点了            │ ← 每条记录一行：时间/
│ 10:39:03 assistant.msg   · (reasoning, 灰)    │    类型徽章/ItemID/
│                         现在是 10:39。        │    原文（转义，不美化）
│ 10:39:04 tool.call       shell{...}           │
│ 10:39:05 tool.result     (exit 0, 截断)        │
│ compaction ⋯ 96 archived records             │ ← 压缩边界行（§1）
│ 10:40:12 user.input      那昨天呢              │
│ ▍assistant.msg (streaming) · 感谢您的…▌        │ ← in-flight 行
├──────────────────────────────────────────────┤
│ [ 输入框………………………… ] [发送] [停止]           │
└──────────────────────────────────────────────┘
```

reasoning 用 CSS 灰显（对齐 CLI）。内容按记录原文渲染（HTML 转义）。
DOM 有界（§4.1）。

## 8. 验证计划：运行时自证 + 集成测试

**核心是页面运行时自校验**（摘要链）：服务端在 status 帧下发链头
`d`，页面对自己已持有的记录算同一条链，状态区常驻 ✅ synced /
❌ drift（红条 + 两侧摘要值）。一致性从"测试时验证"变成"运行时
持续自证"——任何时刻打开页面，绿勾即证明（**页面数据存储 ==
服务端交付的记录流**的逐条哈希链；不等于 == 磁盘日志——磁盘
一致性由 storage 层测试覆盖，`d` 由服务端从它自己交付的记录算
出）；红了自带现场，且 ❌ 的处置是丢弃 h 走窗口重放（§4.1）。
后向滚动加载同样可验证（两端锚点已知）。

集成测试（CI，无浏览器）：

- **INV1（wire ≡ 记录流）**：驱动**确定性的 run**（fakeProvider 一次
  启动到 settle），以流自身首帧 snapshot 的 records 为基准，比对最终
  收集的 record 序列
- **INV2（断点/重连一致）**：带 from 重连零重传且序列对齐；未命中
  from 回退窗口重放后链头一致；无重复
- **INV3（in-flight 取代）**：流完成后无 streaming 行；**中途连接**
  能通过 snapshot 首帧恢复 in-flight 行
- **INV4（并发输入）**：两个并发 POST，恰一个 200 一个 409 `run_active`
- **INV5（慢客户端）**：不读 SSE 的客户端不阻塞 run 完成
- **INV6（多客户端一致）**：两个并发 SSE 客户端 record 序列相同；
  两页并发提交恰一个成功；一页断线不影响另一页
- **摘要链互验**：Go 侧独立实现页面同款链算法，对同一段记录
  断言两端链头相等（防两端实现漂移）；**测试数据显式包含三类
  漂移高发样本**：`<>&\u2028`（转义差异）、**非法 UTF-8**（规范化
  差异，前置工程 C 后应稳定）、空字段/缺字段——两个 Go 实现会
  一致地犯错，恰恰这三类抓的是跨语言漂移
- **会话锁**：第二个 Open 同会话失败并报持有者 pid；kill 持有者后
  可重开；Close 释放
- **CSRF**：非 JSON content-type 被拒；非 loopback Origin 被拒
- 冒烟：真实 provider 走一轮含工具调用的对话，人工看页面
  （仅确认观感，数据一致性由链自证）

## 9. 分期

1. **P1（本次）**：前置工程 A（OS 级会话锁）/ B（CSRF）/ C（UTF-8
   规范化）→ server 骨架 + debug REPL（/tail 族）+ SSE 双流（含
   摘要链断点/窗口/自校验）+ history 滚动加载 + 输入/压缩 + log 页
   + 状态区 + 一致性测试
2. **P2**：agent 层事件 fan-out（唤醒补丁退役）、归档段回填滚动
   加载、会话列表/切换、命令面板、debug 命令集扩充
3. **P3**：聊天式渲染（在确认数据通道完全可靠之后）
