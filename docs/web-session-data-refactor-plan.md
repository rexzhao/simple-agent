# Web 会话数据层彻底重构计划

> 历史方案：本文的 Session detail/items/snapshot HTTP 设计已由 Stage G2 的
> WebSocket resource/command clean break supersede。旧 endpoint 仅保留作设计背景，
> 当前 HTTP surface 仅包含 bootstrap、ticket、upgrade、Blob 与 session image read。

## 目标

建立一条唯一、可验证的会话数据链路：后端返回同一逻辑快照，前端按 session identity 和单调版本归并，React/DOM 仅消费状态而不充当业务数据源。

## 一致性不变量

1. Conversation 的 `sessionID`、detail、history、active run 和所有 command target 必须一致。
2. 异步结果写入其发起时绑定的 session，不读取完成时的全局 selection。
3. 同一 session 的旧请求不能覆盖新请求；同一 project 的旧列表不能覆盖新列表。
4. run 事件必须同时匹配 `sessionID + runID`。
5. `run.settled` 后，只有 durable history 已达到 `last_seq` 才能移除 transient overlay；同步失败进入 reconciling 并保留内容。
6. history rewrite/compaction 后不得永久保留未经服务端确认的陈旧条目。
7. DOM 只维护滚动位置；业务身份由显式 `sessionID` 提供。

## 工作包

### A. 后端聚合快照协议

- 新增 `GET /api/sessions/{sessionID}/snapshot`。
- 一次 session store load 生成 detail 与 chat page，避免两个 HTTP 请求拼出混合状态。
- snapshot 返回 `session_id`、`revision`、`session`、`history`。
- revision 使用服务端可比较的 session 状态标识；items 继续使用 `last_seq/newest_seq` 作为 durable settlement 水位。
- 增加 Go handler/service 测试。

### B. 前端规范化 Session Store

- 新增 reducer 驱动的 `useSessionStore`，统一持有：
  - `sessionsByID`
  - project -> active/archived session ID 索引
  - session -> history window
  - session -> refresh generation/loading/error
- snapshot、列表和分页只能通过 reducer action 归并。
- project list 请求按 project generation 丢弃旧响应。
- history 按 item ID/seq 去重；服务端窗口 epoch/revision 变化时替换而不是保留陈旧 prefix。
- 取消 `useSessionHistory` 中独立的 detail/page/cache 权威状态。

### C. 运行态归并

- 扩展 run registry：settled 后进入 `reconciling`。
- durable snapshot 达到 `settled.last_seq` 后才删除 run。
- stream error 只清理捕获的 run ID，不能清理该 session 后来启动的 run。
- resync/settled 统一走 session store refresh。

### D. 显式身份边界

- `Conversation` 必须接收 `sessionID`。
- 所有 callback 都接受或绑定该显式 ID。
- 移除不完整的自定义 `React.memo` comparator。
- detail/history 与 sessionID 不一致时不渲染旧数据。
- DOM scroll memory 继续按显式 sessionID 保存，不再从延迟到达的 detail 推断。

### E. 验证

- TypeScript check、Vitest、Go test、Web build。
- 新增竞态测试：
  - session 快速切换；
  - project list 乱序返回；
  - snapshot 乱序返回；
  - settled refresh 失败时保留 overlay；
  - 旧 stream 失败不能删除新 run；
  - snapshot identity mismatch 被拒绝。

## Review 流程

1. 完成实现和本地测试后，启动 medium reasoning 子 agent 独立审查。
2. 每轮一次性等待最多 3 分钟，不高频轮询。
3. 对 blocking/major 问题修复并重新测试。
4. 继续发起 review，直到 reviewer 明确给出 PASS。
