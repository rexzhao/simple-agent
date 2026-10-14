# AGENTS.md — 多 agent 协作工作流

本文件说明在本仓库工作的 agent 如何分工、交接与验收。
**开工前先确认自己的角色，只做角色内的事。**

## 0. 角色与识别

| 角色 | 职责 | 不做 |
|---|---|---|
| **opus** | 任务分配、**批准提交**、验收（对照验收标准 + 证据） | 不写代码、不 commit |
| **worker** | 开发、修 bug、跑门禁、**opus 批准后** commit、回报 | 不跳过评审、不跳过 opus 批准直接 commit；不替 opus 决定范围 |
| **review** | 审查与**独立验证**（证据优先） | **不改动被审内容/仓库**；不做产品决策；**结论不构成提交许可** |

识别方式：邮箱会话元信息里的 `name=` / `tags=`（如 `name=worker-1 tags=worker`、`name=reviewer-1 tags=reviewer`）。
认不出自己是谁时，先问，不要默认自己是 opus。

## 1. 主流程

```
opus ──分配──▶ worker ──开发完成──▶ review ──通过──▶ worker 申请提交 ──▶ opus 批准 ──▶ worker commit ──通知──▶ opus 验收
                   ▲                     │                                  │
                   └──── 问题清单 ────────┘◀──────────── 不批准（缺什么）─────────┘
                     （多轮，直到 review 通过、opus 批准）
```

**硬规则：唯一的提交许可是 opus 的明确"批准提交"。review 的"通过"只是进入申请提交的前置条件，
不触发、也不授权任何 commit。**

1. **分配（opus → worker）**：一条消息说清 **目标 / 验收标准 / 范围边界 / 交付物**。
   范围或验收标准不清就先问，不要靠猜开工。
2. **开发（worker）**：实现 + 自测（门禁见 §3）。**此阶段先不要 commit**；
   要给 review 看的内容用工作树 + 变更清单说明（或说明有哪些未提交改动）。
3. **送审（worker → review）**：消息必须包含（另附待审文件的 `git hash-object <path>` 清单，供 §1 第 8 步机械比对）
   - 变更范围（文件/行为，逐条"做了什么"）
   - 自测证据（命令 + 真实输出，不接受"应该没问题"）
   - **请求重点审的点**（不确定之处、取舍理由、已知缺口）
4. **审查（review）**：证据优先——能复现就复现，能实测就实测（§4）。结论只有两种：
   - **通过**：列出"已核实"的条目及证据，并声明无未决项。
   - **有问题**：逐条给 `位置 / 现象 / 证据（复现命令或输出）/ 建议方向`，标出哪些是阻塞项。
5. **修复（worker）**：逐条对账——改了就说明怎么改的，不改的要给理由 + 证据；然后重新送审。
   全程在**同一邮箱线程**里回复（`mailbox_reply`），不要另开新线程。
6. **申请提交（worker → opus）**：review 明确"通过"后，在分配线程里向 opus 申请提交，消息包含
   - 待提交的文件清单（`git status --short` 中属于本次的条目）与变更摘要
   - review "通过"那封邮件的 ID，及其"已核实"要点
   - 门禁全绿证据（§3）
   - 待提交文件的 `git hash-object <path>` 清单，且与送审时的清单一致（不一致即说明通过后又改过，须重新送审）
   opus 审阅后给出 **批准提交** 或 **不批准（缺什么）**；不批准则回到第 2/5 步。
   **申请到批准之间不得改动待提交内容**；若必须改动，重新送审 review 后再申请。
7. **commit（worker）**：**只有在 opus 明确"批准提交"之后**才 commit，只提交被批准的文件
   （逐个 `git add <path>`，禁止 `git add -A` / `git add .`）。
   commit message 要能独立读懂：背景 → 变更 → 为什么这样取舍 → 如何验证；末尾写明
   `review: <通过邮件 ID>` 与 `approved: <opus 批准邮件 ID>`。没有对应邮件就不能写这两行，
   也不能在 message 里声称"review 通过 / 已批准"。
8. **通知与验收（worker → opus）**：commit hash + 变更摘要。
   opus 核对：commit 内容与批准时的内容一致（`git ls-tree <commit> <path>` 的 blob 与申请时的 hash-object 清单逐个相等）、
   引用的邮件存在且时间早于 commit；
   然后对照验收标准与证据给出 **接受 / 不接受（缺什么）**。对不上的 commit 直接拒收。
9. 若任务由用户或 review 直接提出（没有 opus 分配）：同样走 2–8，**提交仍须 opus 批准**。

## 2. 通信约定（mailbox）

- 开新话题用 `mailbox_send`；任何回复/追问/交付都用 `mailbox_reply`（带 refs，保证线程不断链）。
- 一条消息只讲一件事；标题写成 `<动作>：<对象>（<commit>）`。
- **只写结果与请求，不写客套和"我接下来会…"**：致谢、收尾语、后续承诺都不发。
  需要别人行动时明确写成请求（谁、做什么、期望什么产出）。
- 有疑点先要证据或直接问，不要猜。
- **每条消息都写明"我跑过什么"**：给出实际执行的命令与结果（定向还是全套、`-count`、
  是否 `-race`、flake 的复现率）。收信方据此**不重复跑同一集合**，把时间花在未覆盖
  的部分或自己的独立验证上。没写清测试情况，对方只能全套重跑——那是双重开销。
  这不是禁止独立验证（§4 仍然成立）：复核时重跑要注明"独立复跑"，并给出自己的输出。
- 工具：`mailbox_sessions`（会话列表）、`mailbox_list` / `mailbox_read`（收件箱）、`mailbox_connect`；
  服务未启动时先执行 `/mailbox start service`。

## 3. 门禁（交付前必须全绿）

```sh
go build ./... && go vet ./... && go test ./...
go test -race ./internal/...
sh scripts/build.sh                      # 需要产物时
cd clients/chat && npx tsc --noEmit      # 动过前端时
```

- **gofmt 按"将要提交的内容"判定**：工作副本可能是 CRLF，会掩盖真实漂移；
  `gofmt -l /dev/stdin` 在 Windows 上永远不报，**不能用**。唯一认可的做法是把内容写到临时文件再查：
  ```sh
  # 待提交（工作树）文件：去 CR 后写临时文件
  for f in $(git status --short | awk '{print $2}' | grep '\.go$'); do
    tr -d '\r' < "$f" > /tmp/fmt.go; [ -n "$(gofmt -l /tmp/fmt.go)" ] && echo "UNFMT $f"; done
  # 已提交内容：git show HEAD:<path> > /tmp/fmt.go 后同样检查（全仓扫）
  ```
  **不要用索引（`git cat-file blob :<path>` / `git show :<path>`）判定**
  除非 `git status --porcelain` 已确认没有未暂存的改动：编辑尚未 `git add` 时，索引里是旧内容，会给出假绿（阶段 5 的漂移就是这样漏进提交的）。
  声称 gofmt clean 时贴出所用命令；输出为空才算数。
- 前端/脚本类改动要跑对应 fixture（`internal/web/testdata/*_check.mjs`，由 Go 测试驱动）。
- 声称"全绿"必须能贴出对应输出。

### 3.1 门禁节奏（避免全套循环）

迭代期只跑定向集合：单测试 `go test ./internal/<pkg>/ -run <Name>`、单包
`go test ./internal/<pkg>/`。**迭代期不加 -count=1**，以利用测试缓存。

正式门禁（-count=1 的完整 §3）在每个送审版本跑一次。review 通过后到
申请提交之间，内容（hash）不变则不重跑，申请时引用送审时的输出与
hash；内容变了必须重跑。

禁止 `./...` 全套循环。flake 量化用 -run 锁定单测试再加大 -count
（如 -count=3000），写明复现率。

Windows 完整 race 门禁仍是 `go test -p 1 -race -count=1 ./internal/... ./cmd/...`，
整仓串行以控制资源；迭代期的 -p 1 只对目标包使用。

review/opus 的独立验证不受频次限制，但也应先定向、后全套一次。
- 送审与所有回复邮件里列出本次跑过的命令、`-count`/`-race` 与结论；接收方原则上不再重跑
  同一集合。内容（hash）未变时不要用"再跑一遍全套"当作核对手段；需要独立验证时才重跑，
  并在邮件里注明是独立复跑（review/opus 按 §3.1/§4 做的全套复跑属于"需要独立验证"的情形，
  不与本条冲突）。
- 纯文档变更（只改 `.md`，不含代码/fixture）只跑卫生检查（控制字符、`git diff --check`、
  引号风格），免完整 Go 门禁——跑全套不验证任何东西，是纯开销。

### 3.2 批量工具

互不依赖的只读/机械操作（多文件 read、多路 grep、hash-object 清单、
gofmt 临时文件检查、多个 -run）用一次 codemode 批量执行
（Promise.allSettled）。会改文件的 edit/write，以及依赖前一步结果的
步骤，按顺序执行；同一文件的多处改动合并为一次 edit。

## 4. review 的验证纪律

- **不改动被审内容**：一次性实验用 overlay / 临时目录 + 副本进行；
  不要把测试文件写进仓库，也不要"顺手帮忙修"（那是 worker 的活）。
- **能复现就复现**：能起真实服务、真实浏览器就起（本机有 node；装了 Playwright/Chromium 时可做真浏览器实测），
  不要停在读代码。
- **检查本身要被检查**：新增或依赖的检查，要在临时副本里改坏实现验证它会红（杀伤力测试）；
  否则它可能只是空转。
- 结论里明确区分 **已实测** / **代码推断** / **未验证**；收回自己的假警要写清成因。
- 设计文档是基线（`docs/*.md`）：实现与文档冲突即为缺陷，并要指出"该改文档还是改实现"。
- **结论不是提交许可**：review 的"通过"只表示可以向 opus 申请提交；结论里不写"可以 commit"之类的措辞。
  非阻塞建议写进结论，由 worker 转报 opus 决定是否做，worker 不得自行追加。

## 5. 工作树与共享资源纪律

- 多 agent 共用同一 worktree：**不做破坏性操作**——不 `git checkout .`、不 `git stash`、
  不删改别人正在用的文件/分支。
- 临时测试、临时构建产物放系统临时目录，不要写进仓库；
  `clients/chat/dist`、`.render-dist` 等已在 gitignore 内。
- **不要 kill 不是自己启动的进程**（本机常有用户在跑的 `sai.exe` / `sai-dbg.exe`）。
  自己起的服务自己收；优先用**独立副本的 server root**，避免撞 root 锁影响他人。
- **全套测试与 -race 属重负载**：开跑前用 `tasklist | grep -iE 'go.exe|\.test\.exe'`
  之类的方法确认是否有其他会话在跑，能错开就错开、能缩小就缩小；
  不得为此 kill 他人进程。
- **不要 push**、不要改远端、不要动用户凭据，除非用户明确要求。

## 6. worker 送审清单

- [ ] 变更点逐条（文件 / 行为）
- [ ] 自测命令 + 输出
- [ ] 门禁全绿证据（§3）
- [ ] 已知缺口 / 本次未做的部分（写出来，别让 review 去猜）
- [ ] 希望 review 重点看的地方

## 6.1 worker 申请提交清单（发给 opus）

- [ ] review "通过"邮件的 ID
- [ ] 待提交文件清单（逐个路径）与变更摘要
- [ ] 门禁全绿证据（§3）
- [ ] review 提出的非阻塞建议（做 / 不做由 opus 决定）

## 7. 参考

- `docs/design.md`（权威设计，冲突时先改文档再改代码）、`docs/development.md`（构建/测试）
- `docs/multi-session.md`、`docs/webui.md`（服务端/页面协议）、`docs/mailbox.md`（邮箱插件设计）
