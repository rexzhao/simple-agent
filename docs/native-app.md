# sai-native：append-only 会话日志与最小原生界面

> 状态：第一阶段。新的会话存储与原生界面已落地；旧的 Web 前端与
> syncengine/SQLite 栈已整体删除。将来如需 Web 界面，在 sessionlog 之上
> 以 `GET log?after=<seq>` + 通知为原则重建，不再复活旧同步层。

## 设计

破坏性重构后的数据模型只有一条规则：**会话的一切持久化事实都是
`<server-root>/data/<project>/<id>/log.jsonl` 里的一行**。没有 update，
没有 delete，没有派生表。项目 ID 是工作目录路径的确定性哈希
（`ProjectID`），同一目录在 CLI 与未来的 Web UI 中归入同一项目，无需
注册。

```
agent.Stream（internal/agent，复用，不感知存储）
    │  eventbus.Publisher（durable 事件）
    ▼
sessionlog.Publisher → log.jsonl（append-only，逐行 fsync 策略）
    │  fold（打开时重建 / 追加时增量推进）
    ▼
sessionlog.State
    ├── Items    显示用的会话条目（用户/助手/工具/错误）
    ├── Context  下一次模型请求的消息历史
    └── Runs     运行状态
    ▼
cmd/sai-native（mygo 原生窗口）
```

### 空会话不落盘

`Store.Create` 是懒创建：不写任何东西到磁盘。首次 `Append` 才物化日志
（先写 `session.created` 再写该记录）。启动、`/clear` 产生的空会话既不占
目录也不出现在 `/resume` 列表里。

### 事件词汇

| 类型 | 载荷 | 说明 |
|---|---|---|
| `session.created` | provider/model/cwd | 会话元数据，每会话仅一行 |
| `user.input` | run/content | 一次用户输入，同时定义其 run 的开始 |
| `assistant.message` | item/content/reasoning/tool_calls | 终态助手消息（旧名 `assistant.ready` 读时兼容） |
| `tool.result` | tool_call_id/content | 工具结果，独立于工具调用，靠 ID 关联 |
| `run.failed` | error | 仅失败时记录；完成 = 无失败记录，中断 = 输入无回答 |
| `compaction.created` | — | 预留（当前无发布方） |

run 生命周期不再有记录：`user.input` 即 run 开始，无失败即完成，中断
留下无回答的输入。turn 边界与流式 checkpoint 不落盘——日志是对话，不是
事件轨迹。中断在工具调用中间的 run 由 chatapp 在下次请求前修补上下文
（合成的 `[tool execution interrupted]` 结果按调用顺序补齐，见
`sanitizeContext`）。


`item` ID 由 agent 在流式开始时生成并在落盘时沿用——transient 与
durable 共享同一身份，不存在映射层。

### 持久化策略

- 每条记录一个 JSON 行；`seq` 单调递增，`ts` RFC3339Nano。
- fsync 策略按类型：`user.input`、`run.settled`、`assistant.ready`、
  `tool.result` 等终态记录逐条 fsync；`assistant.checkpoint` 只 flush 不
  fsync（掉电丢部分流式输出，终态记录会恢复权威内容）。
- 崩溃恢复：打开时逐行校验，末尾撕裂行（无换行符或 JSON 非法）被截断，
  最后一个合法记录的 `seq` 成为续写基点。
- 未知记录类型在 fold 中被忽略（前向兼容）。
- 注意：chatapp 当前不设置 `agent.Options.NextTurnID`，单 run 单 turn，
  因此 `turn.started`/`turn.completed` 实际不会出现在日志里（事件表中
  保留它们是为了词汇完整性）。

## 工具面

agent 只有一个工具：shell。全平台统一 Bash 语法（Windows 用 Git Bash 或
其他 bash.exe，绝不回退 PowerShell/cmd.exe），命令的工作目录是会话 cwd。
读文件、搜索、编辑、构建、git 全部通过 shell 命令完成——工具面最小，能力
不缩水。旧的文件工具（read_file/edit_file/apply_patch 等）与 skills、MCP、
compaction、logging、pricing 配置已随最小化删除。

## 使用（命令行）

```sh
go run ./cmd/sai [session-id]
```

参数：

```text
--server-root dir   配置根目录；解析顺序：--server-root > $SAI_SERVER_ROOT > ./.<app>/sai.yaml（存在时）> <user config>/<app>。<app> 取可执行文件名第一个 - 之前的部分（sai-windows-amd64.exe → sai）
--cwd dir           工具工作目录（默认进程 cwd）
--provider name     覆盖 default_provider
--model name        覆盖 default_model（模型 profile 名）
--full-access       文件工具允许读写 cwd 之外
```

交互：`sai> ` 提示符读入一行输入；记录一经落盘立即打印到 stdout（ watcher
实时尾随）；**仅当模型正在流式输出时** stderr 显示 `
.` `
..` `
...` `
`
（含空帧）循环光标，打印记录前先清掉光标行。等待首 token、工具执行期
间不转。控制信息只走 stderr，stdout 是纯记录流，可直接重定向留存。

命令：

- `/clear` 新会话（空会话不落盘，反复 clear 不产生任何磁盘写入）
- `/resume` 列出已存会话；`/resume <id>` 打开指定会话（先回放其全部
  JSONL 再继续）
- `/stop` 停止当前运行（Ctrl+C 同效；空闲时 Ctrl+C 退出）
- `/help` 命令帮助

## 后续

- compaction fold：`compaction.created` 已落盘，fold 侧按折叠点渲染。
- 同步层：以 `GET log?after=<seq>` + 通知为原则重建 Web 前端
  （见上文重构讨论），替代 syncengine/wsgateway 订阅机器。
- 运行中排队输入（`turn.input` 已有事件，chatapp 侧尚未开放 UI）。
