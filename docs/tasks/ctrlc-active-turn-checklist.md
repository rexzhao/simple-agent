# Ctrl+C Active Turn Checklist

归属里程碑：M11 Reliability。

目标：明确并实现交互式 `sai chat` / agent active turn 的 Ctrl+C 行为。用户在模型请求、tool
执行或 MCP 调用等当前轮次仍在执行时按 Ctrl+C，应取消当前轮次并回到 chat 输入状态，而不是退出整个
CLI 进程。已经完成的外部副作用不回滚。idle 输入状态下 Ctrl+C 可继续沿用现有 CLI / terminal
行为；短时间重复 Ctrl+C 如果发生在同一 active turn 取消完成前，仍应聚焦于取消当前轮次，不直接退出
chat session；`--quit` 单轮运行取消 active turn 后可以结束进程，因为没有 REPL 输入状态可返回。

## Documentation

- [x] 在 `docs/milestones.md` 的 M11 中区分 active-turn Ctrl+C、idle Ctrl+C 和 `--quit`
  Ctrl+C 行为。
- [x] 在 M11 文档中记录 active turn 取消完成前短时间重复 Ctrl+C 不直接退出 chat session。
- [x] 在 `docs/checklist.md` 中避免已有已勾选 M11 项目过度声明该新澄清行为已经实现。
- [x] 新增本任务 checklist，记录文档、实现和验证步骤。

## Implementation

- [ ] 梳理当前 CLI signal handling，明确 active turn、idle input 和 `--quit` 路径的现有边界。
- [ ] 为交互式 `sai chat` active turn 建立 per-turn cancel 流程，Ctrl+C 取消当前轮次后恢复到
  prompt。
- [ ] 确保同一 active turn 取消完成前，短时间重复 Ctrl+C 不升级为 hard exit 或 session/process
  cancel。
- [ ] 确保 active turn cancel 不回滚已完成 tool、shell、MCP 或其他外部副作用。
- [ ] 确保 cancel 后不追加成功 assistant history，也不把 partial assistant 输出伪造成完成轮次。
- [ ] 确保 idle 输入状态 Ctrl+C 不被误当作 active-turn cancel；保留现有退出行为，除非实现时发现更严格约定。
- [ ] 确保 `--quit` active turn Ctrl+C 取消当前轮次后可结束进程，并保持可读错误或取消状态。
- [ ] 确保 logger、provider request、shell 工具和 MCP stdio 子进程仍通过 context cancel / close
  边界收尾。

## Validation

- [ ] 添加 CLI 测试：交互式 active turn 收到 Ctrl+C 后取消当前轮次、回到 prompt，并允许继续输入下一轮。
- [ ] 添加 CLI 或集成测试：同一 active turn 取消完成前短时间重复 Ctrl+C 不直接退出 chat session，取消完成后仍回到 prompt。
- [ ] 添加测试：active turn cancel 后不追加成功 assistant history。
- [ ] 添加测试：idle 输入状态 Ctrl+C 保持现有退出行为或项目约定行为。
- [ ] 添加测试：`sai chat --prompt ... --quit` active turn Ctrl+C 取消后可以结束进程，且不伪造成功
  assistant history。
- [ ] 添加或更新 provider / tool / MCP / logger cancel 测试，覆盖本需求触发的取消路径。
- [ ] 运行 `go test ./...`。
- [ ] 运行 `git diff --check`。
