# Mailbox MCP Checklist

这份清单用于跟踪 M23：在前台 CLI 进程内启动本地 mailbox MCP server，
让其他 agent 通过 MCP 投递任务；CLI 空闲时作为唯一 worker 从 mailbox 取任务并执行。

## Scope

- [ ] `sai --mailbox-mcp 127.0.0.1:PORT` 在当前 CLI 进程内启动本地 MCP mailbox server。
- [ ] mailbox MCP 只绑定 localhost / 127.0.0.1；不默认监听 `0.0.0.0` 或远程地址。
- [ ] mailbox MCP 是本地输入适配器，不恢复旧 HTTP/WS product layer，不提供 project/session
  管理 API。
- [ ] CLI 保持唯一 worker；mailbox 只负责任务投递、状态、最终结果和取消。
- [ ] stdin 行为保持现状：纯 CLI 不新增 turn 运行中的应用层输入队列或队列 UI。
- [ ] mailbox task result 只暴露最终 assistant 输出、状态和错误；不暴露 text deltas、tool
  events、raw execution events、hidden/debug item 或中间过程。
- [ ] mailbox queue 第一版为进程内内存状态；不持久化，不跨 CLI 进程共享。
- [ ] mailbox MCP 和 subagent runtime mailbox 概念分离；前者服务外部 MCP clients，后者服务
  parent/child agent runtime event delivery。

## MCP Tools

- [ ] `mailbox_post(prompt)` 创建 queued task，返回 `task_id` 和当前状态。
- [ ] `mailbox_get(task_id)` 返回任务状态、最终结果或错误。
- [ ] `mailbox_wait(task_id, timeout_ms)` 等待任务进入 terminal state 或超时；超时不取消任务。
- [ ] `mailbox_cancel(task_id)` 取消 queued task；对 running mailbox task 取消当前 turn context，
  不退出 CLI。
- [ ] terminal task 的 cancel 幂等返回当前状态。

## Acceptance Criteria

- [ ] 其他 MCP client 可以 initialize、list tools、call mailbox tools。
- [ ] CLI idle 时从 mailbox 取 queued task 执行，并在控制台正常输出该 turn 的流式内容。
- [ ] CLI 正在处理 stdin turn 或 mailbox turn 时，新的 mailbox task 保持 queued。
- [ ] `mailbox_wait` completed/failed/cancelled 时返回 terminal state；timeout 时返回当前状态和
  `timed_out: true`。
- [ ] `mailbox_cancel` queued task 直接变 `cancelled`。
- [ ] `mailbox_cancel` running mailbox task 只取消当前 task/turn，不关闭 MCP server，不退出 CLI。
- [ ] mailbox result 中不包含中间流式 delta、tool status、raw provider error、hidden/debug item
  或 tool result 正文。
- [ ] 本地验证覆盖 mailbox queue lifecycle、MCP initialize/tools/list/tools/call、wait timeout、
  cancel queued、cancel running、最终结果脱敏/只返回 final assistant output。
- [ ] `go test ./...` 通过。
- [ ] `git diff --check` 通过。

## Smoke Evidence

实现切片完成后记录命令、测试范围和关键行为证据。
