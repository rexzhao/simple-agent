# 多项目多会话（serve 模式）

状态：设计稿（第 2 轮，吸收 reviewer 意见）。实现待用户指令；本文取代
`docs/webui.md` 的单会话装配与路由部分（§2、§6 以本文为准），快照协议、
摘要链、hub 语义不变。

## 0. 术语

| 术语 | 定义 |
|---|---|
| **CLI 模式** | `sai` 无 `-http` 运行。单机命令行，进程宿主一个会话，**当前路径即项目路径**（`ProjectID(cwd)`），串行单会话 |
| **serve 模式（webui 模式）** | `sai -http <addr>` 运行。常驻 web 服务，覆盖整个 server root 的数据，**只服务注册过的项目**，多会话并行 |

两种模式共享同一 `<serverRoot>/data` 存储；CLI 模式**不需要注册**
（cwd 即项目），注册只约束 serve 模式的可见范围。

## 1. 注册制（决策 #2）

serve 只显示注册的项目。注册表是文件，serve 按需重读——**文件即接口**，
`sai project add` 与运行中的 serve 之间零协调。

- 路径：`<serverRoot>/projects.json`
- 格式：`{"projects": [{"path": "<绝对路径>", "added": "<RFC3339>"}]}`
- 写入：临时文件 + rename 原子替换
- 项目 ID：`ProjectID(path)`（确定性哈希：规范化绝对路径 + 大小写折叠），
  注册表存原始路径、ID 现算——**不落盘派生值**
- CLI 子命令：`sai project add <path>` / `sai project remove <path>` /
  `sai project list`（add 做规范化与去重；remove 只摘注册表，不动数据）
- **注册表损坏的语义**：启动时损坏 → 报错退出（fail fast）；
  **运行中重读发现损坏 → 保留上一份好表并向请求暴露错误，绝不当作空表**
  （空表 = 所有项目静默消失）；`sai project add` 在注册表损坏时**拒绝写入**
  ——"读损坏表 → 加一条 → rename"会把其余项目全部抹掉，是数据丢失路径
- **读取端去重**：注册表手工可编辑，两条规范化后同 ID 的条目由 serve 侧
  合并去重（CLI 侧去重之外的第二道）

**信任边界（不是沙箱）**：注册一个项目 = 授权 serve 进程**以该目录为
工具默认工作目录**执行 shell 工具。同一用户的其他路径、以及本进程覆盖的
其它项目存储都在工具可达范围内——一进程服务所有项目，这是必然，措辞上
不得读成隔离。未注册项目的会话即使存储里有也 404（注册门见 §4）。

**执行目录规则**：会话工具的 cwd 取**注册表当前路径**；记录里的
`session.created.CWD` 只是历史。改注册路径 = 静默改变旧会话的执行目录
——这是有意的语义（注册表是当前事实），文档写死以免误读为 bug。

## 2. 会话锁策略（决策 #1）

- **CLI 模式**：进程生命周期持有（现状即如此，不改）
- **serve 模式**：惰性打开 + 空闲驱逐

### 2.1 单 serve 实例

启动时在 `<serverRoot>/serve.lock`（**专用文件，不锁 `projects.json`**）
取 root 级锁，第二个 serve 进程 fail fast 退出（与注册表损坏的
fail-fast 一致）。专用文件的原因：注册表是 rename 原子替换的，锁会留
在被替换掉的旧 inode 上（Unix flock 锁 inode；Windows 的覆盖重命名
行为依共享模式而定）——两个 serve 可能都认为自己持有 root 锁。会话锁
选 `<dir>/lock` 恰好符合同一原则（从不被替换，段切换只 rename
`log*.jsonl`）。把"两个 serve 处处 409/503"换成一句清晰的启动报错。

### 2.2 驱逐（清扫者每 10s）

```go
type sessionEntry struct {
    session   *agent.Session
    hub       *Hub
    refs      int       // 在途请求计数：进入 ++、退出 --
    idleSince time.Time // 见刷新点
}
```

驱逐条件（**全部在同一 `Manager.mu` 临界区内判定并执行摘除**）：

```
refs == 0 && hub.Subscribers() == 0 && !session.Running() && idle ≥ 30s
```

- **摘除先于 teardown**：判空闲 → 从 map 摘除 → （锁外）`hub.Stop()` +
  `session.Close()`。顺序不能反——先 Close 后摘除的话，opener 会拿到
  已关闭的 entry 继续用
- **`idleSince` 刷新点**（写死四处）：订阅者挂上/摘下、run 开始、
  **run 结束**（`lastRunning && !running` 时置 now——若只在开始时刷新，
  一个 10 分钟无人观看的 run 结束后立刻满足空闲条件，页面重连只能全量
  重放）、每一次被服务的请求（refs 变动处）
- **refs 封住判定读数的 TOCTOU**：`Subscribers()==0` 与 `Running()==false`
  的读取和摘除在同一临界区；在途请求持 refs，扫帚绝不驱逐 `refs>0` 的
  会话。"毫秒级窗口"的说法作废——这是一个 int 的成本换零窗口
- **无活动 run 是体验约束不是安全约束**：`agent.Session.Close()` 会
  cancel 并等 run settle，所以驱逐一个 running 会话不会损坏状态——
  不驱逐它只是为了不打断用户的 run。写明这层区别，免得以后有人以为
  它是竞态防线而"优化"掉
- **锁已验证**：`jsonl.Session.Close()` 即使 `log.Close()` 报错也会
  `lock.Release()`，teardown 失败不会把锁留在进程内

### 2.3 驱逐竞态（第二道防线）

摘除先于 teardown 后，剩余窗口 = "map 里已无 entry、teardown 未完成、
新请求进来惰性打开"——同进程内撞自己的锁（flock/LockFileEx 按句柄）。
处理：

- Manager 维护 `closing map[string]struct{}`（teardown 完成前保留），
  惰性打开失败时**用它区分错误来源**，不解析错误字符串。三类分流：
  在 closing 集里 → 503 `session_closing`（**页面可重试**）；锁冲突 →
  409 `session_locked`（**不可重试**，另一进程持有——错误信息带 pid）；
  **既不在 closing 集又不是锁错误**（段损坏、权限）→ 500 并把错误
  暴露出来——"打开失败即 409"是错的归类
- 顺带修锁错误文案的自撞误导：报错里的 holder pid 在自撞场景下就是
  本进程，"locked by another process" 是错的——文案改中性
  （`held by pid N (this process)` 由调用方语境补充）
- 页面侧语义分清：**只对 503 重试**；SSE 的 503 只能靠 EventSource
  自动重连（JS 拿不到状态码），恰好等于"重试一次"
- **驱逐后重连是正常路径**：`from=<h>` 未命中 → 全量快照重放（Rule A，
  成本是重放，不是错误）

**CLI↔serve 互操作**：CLI 打开 serve 手里闲置的会话 → 锁错误（带 pid）；
等驱逐（≤30s）或停 serve。反向：serve 惰性打开 CLI 占用的会话 → 409
透传给页面。

## 3. 架构

```
web.Manager（取代 web.New(session)）
├── registry    按需读 projects.json（损坏语义见 §1）
├── agents      map[projectID]*agent.Agent   惰性，每项目一个
├── sessions    map[sessionID]*sessionEntry  惰性，含 hub 与 refs
├── closing     map[string]struct{}          驱逐竞态的错误来源判别
└── sweeper     10s 周期，执行 §2.2 的驱逐
```

### 3.1 agent-per-project

`assemble()` 重构为工厂：

```go
func agentFactory(serverRoot, providerName, modelName string)
    func(projectCwd string) (*agent.Agent, error)
```

- provider 配置、MaxIterations、压缩策略：全局（sai.yaml），所有项目共享
- 工具执行器、系统提示词：按项目 cwd 现场装配（cwd = 注册表当前路径）
- **装配先于取锁**：打开会话的顺序是 装配 agent → `OpenSession`（取锁）；
  装配失败返回 500 `project_unavailable`，**不留下已持锁的僵尸 entry**
  （若取锁后才发现失败，立刻 Close 释放）——否则该会话在这条 serve
  生命周期内永久打不开
- `agents` 不驱逐：上限 = 注册项目数，有限
- **模型选择按会话/项目**：本期不做（全局配置先行）

### 3.2 hub 生命周期

- hub 生命周期 = `活动 run OR 有订阅者`（run 可以在无人观看时继续跑，
  hub 的事件消费与落盘无关——现状语义保持）
- hub 暴露带锁的 `Subscribers() int`（fanout 内部已有全部信息）
- run 状态用现成的 `session.Running()`

### 3.3 会话→项目归属

- projectID 来自**存储目录布局**（`<root>/<projectID>/<sessionID>`）
- **不用 `session.created.CWD` 反查**：CWD 不是身份（多项目可同 CWD；
  改名后旧记录仍在），且反查等于用历史数据反向授权未注册路径，违反 §1
- 项目的人类可读路径来自注册表；记录 CWD 只做展示核对

### 3.4 并发

- `Manager.mu` 守护 agents/sessions/closing；惰性打开在锁内做磁盘 IO
  （低流量下可接受；§4 的 `OpenIn` 把 readdir 范围从整个 data root
  收窄到单项目目录，IO 代价随之收窄）
- 同项目多会话并行：锁是会话级的，天然允许；跨项目并行天然允许

## 4. API

全局路由（新增）：

```
GET /api/projects
  → {"projects": [{
       "id":   "pxxxxx…",
       "path": "F:\\work\\paperhub",
       "sessions": [{ "id": "s0000…", "title": "…",
                      "created": …, "modified": …, "active": true }]
     }]}
```

- **只列注册项目**（决策 #2 的字面语义）：未注册目录即使存储里有
  会话也不出现在任何列表里——发现历史不是列表页的职责，知道原路径
  的人 `sai project add` 即可

- `active` = 该会话当前在本 serve 进程内打开（≠ 运行中；页面展示
  "运行中"一律用 run 状态，不用 `active`——惰性打开会让任何一次浏览
  都把它置 true）
- 会话列表来自存储枚举（§5）；`active` 由 Manager 在 API 层合并
- 列表**不含** `records` 计数——计数必须读完所有字节，而页面为了
  `active` 会轮询这个端点；打开会话时 `/api/state` 已给 `count`

会话路由（原路由加前缀，语义不变）：

```
GET  /api/s/<id>/stream?from=<h>     POST /api/s/<id>/send
GET  /api/s/<id>/state               POST /api/s/<id>/steer
GET  /api/s/<id>/history             PUT  /api/s/<id>/steer
POST /api/s/<id>/compact             DELETE /api/s/<id>/steer?item=
POST /api/s/<id>/stop
```

- **注册门在 Manager 层，逐路由**：`jsonl.Store.Open(id)` 扫整个 data
  root 定位会话，"未注册即 404"完全依赖 Manager 检查——每个会话路由
  都过门，且 `-session` 预打开同样过门，配测试
- **store 增加 `OpenIn(projectID, id)`**：解析只在注册项目目录内进行，
  门更硬，同时免去每次惰性打开 readdir 整个 root（且这在 `Manager.mu`
  里做）
- `<id>` 先过 `ValidSessionID` / `ValidProjectID`（store 已有），非法即
  404 而非 500——路径片段不进目录扫描
- 会话 ID 全局随机，理论唯一；但 `Open`/`OpenIn` 取第一个匹配，复制
  目录会造出重名 → 多处命中返回 409（防御一行）
- **错误码全集**（可枚举的契约表，页面与 CLI 按码 switch，不解析
  散文；成功路径不入表——如 `POST /compact` 的 202 异步语义、
  `POST /send`/`steer` 的 200）：

  | 码 | HTTP | 可重试 | 含义 |
  |---|---|---|---|
  | `session_closing` | 503 | 是 | 本进程驱逐 teardown 进行中 |
  | `session_locked` | 409 | 否 | 外部进程持锁（错误信息带 pid） |
  | `run_active` | 409 | 否 | 单 run 槽占用 |
  | `no_run` | 409 | 否 | steer 要求运行中（回退 Send） |
  | `not_queued` | 404 | 否 | 编辑/撤回不存在的排队项 |
  | `stale_anchor` | 409 | 否 | `history?before=` 锚点未知（链重建）；重试同请求仍会 stale，正确动作是丢弃持有记录与链、按 Rule A 全量重放——链路协议的正常信号，页面 Rule A 的唯一触发码 |
  | `nothing_to_compact` | 409 | 否 | `compact`：上下文为空 |
  | `compacting` | 409 | 否 | `compact`：已有压缩在飞（单飞槽） |
  | `project_unavailable` | 500 | 否 | agent 装配失败（路径缺失等） |
  | `empty_input` | 400 | 否 | 空文本 |
  | （无码） | 404 | 否 | 未知会话 / 未注册项目 / 非法 id |

  §2.3 的 CLI↔serve 互操作里"409 锁冲突"即 `session_locked`；CSRF
  守卫（guardAPI）全量适用，未过守卫的请求不进入上表。**边界**：守卫
  与请求解析层的若干失败（`bad json` 400、`content-type must be
  application/json` 415、`missing item` 400 等）走 `http.Error`，
  **body 是纯文本不是 JSON**——客户端不得假定错误响应总能 json
  解析，解析失败时应显示原始 body 而非抛异常
- 旧无前缀路由删除：仓内 `/api/...` 的唯一消费者是 `internal/web/static`
  与进程内工具，无外部客户端（已 grep 核实）；`docs/webui.md` §6 路由表
  加指针声明被本文取代

## 5. 存储枚举（P0，最小集）

```go
type SessionInfo struct {
    ID       string
    Title    string    // 规则见下
    Created  time.Time
    Modified time.Time // 目录内最新 mtime
}

// Store 接口新增：
ListSessions(projectID string) ([]SessionInfo, error)
OpenIn(projectID, sessionID string) (Session, error)
```

- **Title 规则**（reviewer 实测压缩后的磁盘形态后定稿）：
  - 取**最早段**（编号最小的归档段；无归档则活跃段）。归档不可变，
    原始首条 user.input 始终在 `log-0000.jsonl` 里
  - 若最早段前两行是 `session.created` + `context.compacted` →
    会话起点已不在磁盘上（归档被清理的未来形态）→ **Title 留空**；
    绝不取中途输入当标题——标题撒谎比留空更糟
  - 否则第一行是原始 header，紧随的第一条 `user.input` 即标题；
    扫描上限 1000 行保留（正常只需几行）
  - **将来改动的判据**：一旦引入归档清理/截断，本规则立刻退化为
    "永远留空"，那时必须改为落盘一次性事实（session.created 加
    title 字段或 meta 记录）——当前不做的理由是归档不可变且无清理
    策略，扫描是精确的；不动的 schema 不为不存在的清理买单
- **Created 很稳**：段切换时活跃段头部是 `session.created` 的副本
  （同 ItemID、同原始 ts），读活跃段第一行也对
- **容忍截断尾行**：CLI 持锁写时 serve 可以列它——并发追加中的最后
  一行可能不完整，解析失败跳过该行，不算错误
- memory：`Create` 时顺手登记
- **不新增 `ListProjects`**：项目列表的来源是注册表，不是存储扫描

## 6. 页面

- 落地页 = 项目/会话列表；**不自动建会话、不自动选会话**（创建是后置项）
- **切会话 = 全量重置 + 代际守卫**。只"丢弃 state + 重开 EventSource"
  不够：切换瞬间，旧 ES 的最后几帧、在途的 fold、在途的 history 响应
  都会把旧会话的数据落进新会话的 DOM/链/Digest——这不是风险而是
  **每次切换必发生**：
  - `state.gen++`；切换时先 `es.close()` 再建新的；所有帧/handler 回调
    入口检查 `gen !== state.gen` 即丢弃
  - 重置清单显式：`byItem`、`records`、`head`、`d`、`more`、
    `streaming`、`steer-queued` 行
- **hash 路由四件事**（零路由库）：初始带 `#/s/<id>` 直达；
  `hashchange` 处理前进/后退；列表点击**写** hash；未知/已删 id →
  提示 + 回列表（404 与 503 呈现不同）
- **标签页缓存按会话 id 分键**：回切仍走 `from=<h>`，未命中就全量重放；
  不做一份全局缓存
- 多标签同一会话：天然支持（每个标签自己的 EventSource）

## 7. 边界情况

| 情况 | 行为 |
|---|---|
| 注册路径在磁盘上不存在 | 项目仍列出（注册表是真相）；打开其会话 → 装配失败 → 500 `project_unavailable`（顺序见 §3.1，不留僵尸锁） |
| 同一 server root 跑两个 serve | 第二个启动时 root 级锁 fail fast（§2.1） |
| CLI 未注册的 cwd 跑出会话 | 存储里存在，serve 不可见（注册制字面语义）；需要时 `sai project add` |
| 注册后目录被移动 | `ProjectID` 变了 → 旧 ID 目录下的会话在新路径的 CLI 与 serve **都**不可达——数据在磁盘上，CLI 切回旧路径才看得到；re-add 新路径只让项目重新出现且为空 |
| projects.json 损坏 | 启动报错退出；运行中重读损坏 → 保留上一份好表 + 暴露错误；`sai project add` 拒绝写入（§1） |
| 复制会话目录造出重名 ID | `OpenIn` 多处命中 → 409 |

## 8. 阶段与后置项

- **P0**：存储 `ListSessions` + `OpenIn`（jsonl+memory）、`assemble`
  工厂化、`projects.json` + `sai project` 子命令（含损坏语义）
- **P1**：`web.Manager`（注册门、惰性打开、refs/驱逐、closing 集、
  root 锁）、serve.go 重接线
- **P2**：页面（列表、切换代际守卫、hash 路由、active/run 区分）
- **后置**（本轮不做）：页面/CLI 侧创建会话与项目、按会话选模型、
  console `/list` `/tail`、mailbox 通知的多会话路由

## 9. 测试

- **存储**：`ListSessions` 的 Title/Created 跨段正确性（含压缩后会话：
  Created = 原始创建时间、Title = 归档段里的原始首输入、起点缺失 → 空）；
  截断尾行容忍；`OpenIn` 项目域内解析与重名 409；不存在的 projectID
- **web/Manager**：`/api/projects` 形状（注册表+枚举+active 合并、无
  records 计数）；注册门逐路由 404（stream/send/
  steer/compact/state/history 对未注册项目的会话）；驱逐清扫者——并发
  "首请求打开"×清扫者交错 → 断言请求只可能 200 或 503，**绝不出现已
  关闭会话被使用**；驱逐后再请求能重开（锁已释放）；扫帚绝不驱逐
  `refs>0` 或 running 的会话；`-session` 预打开过注册门；**同会话并发
  首请求只开一次**（现在由 `Manager.mu` 内 IO 自动成立，这条测试是
  把 IO 移出临界区的升级路径上的唯一守卫——失败形态是莫名其妙的
  503 而非损坏，更难查）；**503/409 分流直接断言**（teardown 进行中
  → 503 `session_closing`；外部进程持锁 → 409 `session_locked` 带
  pid；段损坏 → 500）
- **注册表**：启动损坏 → 退出；运行中重读损坏 → 保留好表 + 暴露错误；
  两条同 ID 条目合并；`sai project add` 在损坏时拒绝写入
- **CLI**：`sai project add/remove/list` 的规范化、去重、原子写
- **页面**：切换代际守卫（DOM-free 的 `reducer.mjs` 上测"switch 全量
  重置 + 丢弃旧代帧"）
