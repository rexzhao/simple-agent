# Android 展示端

状态：**可用**（模拟器端到端验证通过：连接 relay → 新建会话 → 发消息 → markdown / 过程组 / 工具卡片）

这是 [远程设计](../docs/remote-design.md) 的 Android 实现：它只是**展示端**，
真正的执行端（模型凭据、工具执行、会话日志）仍然跑在电脑上。

---

## 0. 一句话架构

```
Compose 界面（Kotlin，只画）
      │  只传 JSON 字符串 / 字节
Go 内核（gomobile AAR）
      │  host.Remote + driver.Mirror + timeline / render / markdown
      │  wire.TCPConn（裸 TCP，不是 HTTP）
      ▼
relay（可选，固定 token 认证） ──▶ 执行端 simple-agent
```

**为什么把内核做成 Go 库而不是用 Kotlin 重写协议**：展示端要做的事（镜像会话、
按 `(epoch, generation, seq)` 续传、合并流式增量、把工具调用排成过程组、解析
markdown）在 `internal/` 里已经有实现且有测试。重写一份必然与桌面分叉，而
"手机和电脑看到的不一样"正是远程设计里最不能出的问题。

## 1. 目录

| 路径 | 作用 |
| --- | --- |
| `mobile/bridge/` | Go↔JVM 的绑定层。**只暴露 string / []byte**（gomobile 的类型限制） |
| `mobile/bridge/viewmodel.go`<br>`timeline.go`<br>`markdown.go` | 会话 → 可渲染视图模型（跨进程 JSON 结构） |
| `internal/timeline/` | 会话分组语义（正文 / 过程组 / 回合结束行、过程组标题）。**桌面与 Android 共用** |
| `android/app/src/main/java/.../data/` | Kotlin 侧镜像类型（`@Serializable`）与 `CoreClient` |
| `android/app/src/main/java/.../ui/` | Compose 界面 |
| `scripts/build-android.ps1` | 生成 `android/app/libs/bridge.aar` |

## 2. 构建

前置：Go 1.27+、Android SDK + NDK、JDK 17+、`gomobile`：

```powershell
go install golang.org/x/mobile/cmd/gomobile@latest
go install golang.org/x/mobile/cmd/gobind@latest
```

然后：

```powershell
# 1) 生成 Go 内核（四个 ABI：arm64 / arm / amd64 / 386）
pwsh scripts/build-android.ps1

# 2) 打包 APK
cd android
$env:ANDROID_HOME="F:\Android\Sdk"; $env:JAVA_HOME="F:\Android\Studio\jbr"
.\gradlew.bat :app:assembleDebug
# 产物：android/app/build/outputs/apk/debug/app-debug.apk
```

Gradle 里已经接了 `bindGoKernel` 任务：`mobile/` 或 `internal/` 下的 `.go` 变化时
会自动重新 bind，**不需要手动记着**（见下面「踩过的坑」）。

## 3. 使用

执行端与 relay（电脑上）：

```powershell
# relay：对外监听并开启固定 token
simple-agent relay --listen 0.0.0.0:7900 --token 你的口令

# 执行端：注册到 relay（同一 token）
simple-agent --headless --relay 127.0.0.1:7900 --token 你的口令 --name 我的电脑
```

手机上：填 relay 地址 + token → 连接 → 侧栏「新建」建一个会话 → 发消息。

也可以直连执行端（不走 relay，此时没有 token 认证）：

```powershell
simple-agent --headless --listen 0.0.0.0:7801
```

> **安全**：token 只保护 relay 这一段，且**不加密流量**。执行端直连（`--listen`）
> 仍然对所有能连上的人开放，请只在回环/内网/隧道（Tailscale、WireGuard）里使用。

## 4. 已实现

- 连接页（地址 / route / token）、断线检测
- 会话列表（抽屉）、新建会话、切换会话
- 对话流：用户气泡、助手正文、**本地提示**、回合结束行（已完成/失败/已中止 + 耗时 + tokens）
- markdown：标题 / 段落 / 列表 / 引用 / 分隔线 / 代码块 / 表格 / 行内样式（粗体斜体删除线代码链接）
- **过程组**：折叠标题（"已完成分析，执行了命令，调用工具" 与桌面同一份文案逻辑）、
  展开后是思考 + 工具卡片（状态、参数可展开、输出、错误标红）
- 流式输出（逐字增长）、忙时插话队列（可移除）、停止
- 模型 chip：列表来自**执行端**，可切换模型与思考档位
- 图片：按需从执行端拉取 blob、落盘缓存、点开全屏缩放

## 5. 未实现（下一步）

- 回答提问面板（`AskView` 已由 Go 提供，界面还没画）
- 重命名 / 删除会话
- 从手机上传图片（粘贴/选图 → `blob.put`）
- 加载更早的归档（`loadOlder`）
- 文件选择与多会话并发流式（逻辑上支持，未做界面）

## 6. 踩过的坑（都已在代码里注释）

1. **AAR 过期**：改了 Go 代码但没重新 bind，APK 里跑的还是旧逻辑（表现为
   "relay token 明明对却被拒绝"）。现在 Gradle 的 `bindGoKernel` 会在 Go 源码变化时
   自动重建。
2. **gomobile 会先清空 `-o` 指定的文件**：直接指向 `libs/bridge.aar` 时，构建中途
   失败会留下 0 字节 AAR，APK 里就没有 `libgojni.so`，装到手机上直接崩在启动阶段。
   现在先写临时文件再替换。
3. **gomobile 需要 `javac`**：Gradle 调它时必须在 PATH 上有 JDK，否则报
   `exec: "javac": executable file not found`。
4. **x86 模拟器要 `android/386`**：只绑 arm64/arm/amd64 时，32 位 x86 模拟器上
   会以 `dlopen failed: library "libgojni.so" not found` 崩溃。
5. **relay 的 hello 探测**：展示端先发 hello 分辨"对面是执行端还是 relay"，
   relay 必须在**收到 hello 时就校验 token 并回执行端列表**（不能跳过不回，
   否则客户端一直等第一条回包）。token 走 `wire.Hello.Credentials`。
6. **常量输入框被键盘盖住**：`imePadding` 要加在输入卡（`bottomBar`）上，
   不是加在内容区。

## 7. 测试

Go 侧（含绑定层的端到端测试）：

```powershell
go test ./mobile/bridge/ ./internal/timeline/ ./internal/relay/ ./internal/host/
```

`mobile/bridge/bridge_test.go` 用进程内管道起"执行端 + 展示端"，验证 Android
拿到的 JSON：会话列表、时间线条目类型、markdown 块结构、过程组标题、运行时状态、
以及变更通知。它不需要模拟器，是 Android 侧的"前哨测试"。
