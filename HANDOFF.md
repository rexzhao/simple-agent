# Handoff：会话日志模型重构（2025-11-16）

> 给下一个会话的接续说明。当前进度、已知问题、下一步建议。

---

## 1. 一句话现状

会话日志模型重构的**八个阶段全部完成并推送**（`master` 到 `25c82bb`），
Go 侧与 Android 侧都已更新，APK 已装到手机（19:00:35）。

但**执行端还没重新编译** —— 事件格式变了（`message.appended` →
`message.input` / `message.output` / `tool.result`），旧执行端与新展示端不兼容。

---

## 2. 本次重构做了什么

背景：用户提出的核心洞察是**"日志一旦写入就不可变，应该有唯一 id，查看端可以
缓存、只请求缺的部分"**。但原实现有两个矛盾：

1. 分页游标用 `item.key`（渲染产物），里面含 part 索引，流式输出期间会变
   → 真机上表现为"翻到某处就再也翻不动"
2. 同一条消息落两次日志（streaming 空壳 + finalized 覆盖），违反"不可变"

### 概念模型（用户拍板的命名）

| 概念 | 定义 | 现有代码 |
| --- | --- | --- |
| **Round**（大回合） | 用户一次提问 → 全部工具执行完毕 | `driver.Round`（原 `Turn`） |
| **Turn**（小回合） | **一次 LLM 请求**，Round 内自增 | `d.steps` 提升而来 |

### 事件清单（新）

**内容事件**（不可变、可分页）：
```
message.input   { round, turn, role:user, message }
message.output  { round, turn, role:agent, message }   ← 含 thinking/text/tool_use
tool.result     { round, turn, call_id, name, output, is_error }  ← 单条
```

**边界事件**（显式标记，崩溃恢复用）：
```
round.started  { round }
round.finished { round, duration_ms }
turn.started   { round, turn }
turn.finished  { round, turn, duration_ms }
```

**状态事件**（用于重放，不做分页游标）：
`session.created/renamed`、`selection.changed`、`ask.raised/answered`、
`notice.appended`、`todo.write`、`compact.snapshot`

**已移除**：`message.appended` 的 streaming 空壳、批量 `tool.results`

### 崩溃恢复规则

> **没有 `turn.finished` 的 Turn 一律视为中断。**

| 已落盘内容 | 动作 |
| --- | --- |
| 有 output + tool_use，缺 tool.result | 补 `tool.result`（is_error）——否则下次请求被 LLM 协议拒收 |
| 只有 input，没有 output | 不补内容，标记待重发（`Replayed.OpenTurn`） |
| 有 output 但缺 turn.finished | 只补边界事件 |
| 全部完整 | 什么都不做 |

### 分页（阶段 6-7）

- 游标 = **事件 seq**（不是 item key）
- 边界按 item 的 seq 区间对齐，**不切断一条消息**
- `seq` **跨压缩不重置**（快照占用正常 seq，不再用 0）
- `TimelinePageJSON(sessionID string, beforeSeq int64, limit int)`

### 性能（阶段 8）

时间线构建结果按指纹缓存：

| 指标 | 前 | 后 |
| --- | --- | --- |
| 完整 TimelineJSON | 3.27 ms | 1.30 ms |
| 分页 60 条 | 2.91 ms | 0.70 ms |
| 构建耗时 | 2.00 ms | 0.045 ms |

---

## 3. 提交序列

```
25c82bb 阶段 8：时间线构建结果缓存
7d4980e 修 gomobile 导出：TimelinePageJSON 参数 uint64 → int64
cca5efc Android 阶段 7：分页游标改为 seq
d742904 阶段 6：分页游标从 item key 改为事件 seq
9523c0d 阶段 5：压缩后 seq 不再重置
3045cad 阶段 4：崩溃恢复改为基于 turn.finished
9d4d281 阶段 3：事件拆分
c209889 阶段 2（一）：roundId/turnId 落盘 + 四个边界事件
d958c12 阶段 1：重命名 Turn → Round
```

设计文档：`docs/session-log-model.md`（289 行，含完整事件清单、
崩溃恢复规则、分页协议、重放组装、迁移阶段）

---

## 4. 环境（都是现成的，别重新配）

### mini（macOS，构建 Android 用）

```bash
# 一键构建 + 安装
cd ~/sa/work && ./scripts/build-android-mac.sh
# 只改了 Kotlin 时加 --no-aar 快很多（Go 改了必须重建 AAR）
```

| 组件 | 位置 |
| --- | --- |
| JDK 17 | `~/jdk/jdk-17.0.13+11/Contents/Home`（`cat ~/.jdk-path`） |
| Go 1.27.3 | `~/go-sdk/go/bin/go` |
| gomobile | `~/go/bin/gomobile` |
| Android SDK | `~/android-sdk`（platform-tools/android-34/build-tools 34.0.0） |
| NDK | `~/android-sdk/ndk/27.0.12077973` |
| gh | `~/bin/gh`（已登录 rexzhao） |

**踩过的坑（脚本已处理）**：
- `git checkout/pull` 会还原 `android/gradlew` 的执行位 → 脚本每次 `chmod +x`
- `gomobile` 会先清空 `-o` 文件，中途失败留 0 字节 AAR（装上去直接崩）
  → 脚本先写临时文件再替换
- Gradle 的 `outputs` 目录偶尔被清理 → 脚本复制到 `build/app-debug.apk`

### 测试样本

```bash
# 生成新格式长会话（150 轮，902 事件）
rm -rf /tmp/fxdata && mkdir -p /tmp/fxdata/sessions/fixture1
SIMPLE_AGENT_WRITE_FIXTURE=/tmp/fxdata/sessions/fixture1/session.jsonl \
  go test ./internal/driver/ -run TestWriteFixture -v

# 用样本跑分页测试
SIMPLE_AGENT_PERF_SESSION=/tmp/fxdata/sessions/fixture1/session.jsonl \
  go test ./mobile/bridge/ -run "TestTimelinePagingBySeq" -v
```

**注意**：`/tmp/perf-session.jsonl` 是**旧格式**（从真机拉的），
阶段 3 之后不再兼容，用它测分页会失败（不是 bug）。

### relay / 服务器

- relay：`ubuntu@129.211.171.158`，`~/sa/`，端口 7900，token `relaytest2024`
- **本次改动不涉及 relay，不需要重启**
- 但服务器上的二进制是 11/12 构建的，不含 `wire.dialFunc` 崩溃修复
- 新二进制已上传到 `/tmp/simple-agent-new`，尚未替换

---

## 5. 必须先做的事

### 执行端要用新代码重编

事件格式变了。**旧执行端 + 新展示端 = 会话显示为空**（`message.appended`
不再被识别）。

```bash
# 本机（Windows）
$env:CGO_ENABLED="0"; go build -o bin\simple-agent.exe ./cmd/simple-agent

# mini
cd ~/sa/work && CGO_ENABLED=0 ~/go-sdk/go/bin/go build -o ~/sa/agent ./cmd/simple-agent
```

### 旧会话数据读不出来

磁盘上现有的 `session.jsonl`（`message.appended` 格式）在新版本里显示为空。
这是用户明确决定的（不做兼容迁移）。**要提醒用户**，别以为数据丢了。

---

## 6. 待验证（装好执行端后）

装了新 APK 后还没做真机验证。重点看：

1. **打开长会话速度**（分页 + 缓存应该明显改善）
2. **往上翻页能否一路翻到最早**（这是之前反复出问题的地方）
3. **翻页时视野是否稳定**（滚动锚定）
4. **流式输出是否顺滑**
5. **点开过程组**详情是否正常
6. **提问面板选项点击是否有反应**（阶段 8 缓存的第一版指纹漏了选中态，
   导致测试超时，已修，但值得真机确认）

---

## 7. 已知问题 / 遗留

| 问题 | 状态 |
| --- | --- |
| `TestViewerLearnsAllowShellFromHandshake` 偶尔在全量运行时失败、单独跑通过 | 既有并发时序不稳定，与本轮改动无关 |
| 服务器 relay 二进制旧（缺崩溃修复） | 新二进制在 `/tmp/simple-agent-new`，待替换 |
| `attachTailEvents = 200` 限制首次同步 | 这是设计行为；分页补足更早内容 |
| Kotlin 侧仍承担部分分页逻辑 | `earlierPartBySeq`/`mergeProcessDetails`，开销小，可优化 |

---

## 8. 用户讨论过但还没做的

### 分页逻辑移到 Go（用户问过，选了"先不动"）

用户问："分页功能是否可以放到 go 里面做，对 android 的 kotlin 只是获得数据？"

我分析了当前 Kotlin 承担的 4 项职责：

| 职责 | 能否移到 Go |
| --- | --- |
| 保留已翻页的早期 items | ✅ 可以 |
| 合并过程组已加载详情 | ✅ 可以 |
| 游标与"已翻几页"状态 | ✅ 可以 |
| **滚动补偿条数** | ❌ **必须留 Kotlin**（UI 行为） |

**推荐方案**：Go 维护完整列表，但**只返回增量**（`{items: 新增的, prepend: true}`），
Kotlin 只需一行拼接。这样兼顾"Kotlin 极简"和"传输量不增加"。

不建议"Go 每次返回完整列表"——1000 条会话翻到最早时每次要传约 700KB。

### 大脑/工具分离（`docs/brain-tool-split.md`）

更早的设计讨论，用户说"先不管这个，首要任务是移动端表现"。
文档里有完整方案（四阶段演进），随时可以捡起来。

---

## 9. 关键设计决策（用户拍板的，别推翻）

1. **不做旧日志兼容** —— 直接换事件模型，旧数据不保留
2. **压缩后 seq 不重置** —— 单调递增贯穿会话生命周期
3. **消息粒度不拆 part 级** —— 一条 output 一条记录；特别大的内容用资源引用
4. **分页按 Turn 切**（不是 Round，因为一个 Round 可能跑很多 Turn）
5. **`tool.result` 靠 call_id 关联**（与 LLM 协议一致）
6. **崩溃时没 `turn.finished` 一律视为中断**

---

## 10. 常用命令速查

```bash
# 全量检查（vet + staticcheck + test + race 稳定性）
pwsh -File scripts/check.ps1 -NoRace

# 在 mini 上构建安装 APK
ssh mini 'cd ~/sa/work && ./scripts/build-android-mac.sh'

# 拉 Android 的 Go 日志
adb shell run-as com.simpleagent.viewer cat cache/simpleagent/go.log | tail -100

# 检查 APK 里是否包含某个导出方法（gomobile 静默跳过 uint64 那类问题）
unzip -o -q bridge.aar classes.jar -d /tmp/x && cd /tmp/x && unzip -o -q classes.jar -d cls
javap -classpath cls bridge.Client | grep -i 方法名
```

**改源码一律用 edit 工具，不要用 shell 重定向写文件。**
（我用 PowerShell `Set-Content -Encoding UTF8` 把 `replay.go` 的中文注释
全变成乱码，只能 `git checkout` 重做。）
