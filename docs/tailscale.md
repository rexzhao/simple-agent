# 内置 Tailscale（可选）

把 [tsnet](https://tailscale.com/kb/1244/tsnet) 编进程序后，**本进程自己就是 tailnet 上的一个节点**：
对面设备不需要安装 Tailscale 客户端也能互相连接（执行端 / 展示端 / relay 都支持）。

适用场景：机器上装不了或不想装 Tailscale 客户端（公司机器没权限、安卓不想装 VPN 应用、固件里跑不了客户端）。

---

## 0. 它是可选的

不配置时**行为和以前完全一样**：所有拨号/监听仍走 `net.Dial` / `net.Listen`。
只有显式打开（`--tailscale` 或配置里的 `tailscale.enable`）才会拉起 tsnet。

启用失败也不致命：打一条日志后退回普通网络，程序照常可用。

## 1. 用法

```powershell
# 执行端：注册 relay 走 tailnet（对面机器不用装客户端）
simple-agent --headless --relay 100.x.y.z:7900 --tailscale --ts-authkey tskey-auth-xxx

# 展示端：经 relay 连执行端，自己也是 tailnet 节点
simple-agent --connect 100.x.y.z:7900 --route my-pc --tailscale --ts-authkey tskey-auth-xxx

# relay 自己挂在 tailnet 上（只对 tailnet 可见，不占主机端口）
simple-agent relay --listen :7900 --token 口令 --tailscale --ts-authkey tskey-auth-xxx
```

也可以用配置文件（`simple-agent.yaml`）：

```yaml
tailscale:
  enable: true
  hostname: my-pc
  # 更安全的做法：密钥放环境变量 TS_AUTHKEY（优先于这里）
  state_dir: /var/lib/simple-agent/tailscale/executor
```

或者只用环境变量：`TS_AUTHKEY=tskey-auth-xxx`。

## 2. 节点身份是稳定的（关键）

tsnet 的节点身份（tailnet 里那台设备 + 它的 100.x.y.z）保存在**状态目录**里。
本项目的约定：

| 角色 | 默认状态目录 |
| --- | --- |
| 执行端 | `<data_dir>/tailscale/executor` |
| 展示端 | `<data_dir>/tailscale/viewer` |
| relay | `<data_dir>/tailscale/relay` |

三条规则：

1. **目录必须持久化**——换个目录就等于换一台新设备，tailnet 里会多出陌生节点。
2. **每个角色用不同目录**——否则执行端与 relay 会互相覆盖身份。
3. **目录里已有身份时，`auth_key` 可以不填**——重启后仍是同一个节点。

实测验证：同一状态目录连续启动两次，`tailscaled.state` 的 sha256 完全一致（身份未被重建）。

节点默认名是 `主机名-角色`（例如 `my-pc-executor`），避免同一台机器上的执行端与 relay 重名。
名字会被规范化成 DNS 安全形式（大写转小写、空格/下划线转 `-`、非 ASCII 丢弃）。

## 3. 开启后仍然监听本机回环

启用 tailscale 时，执行端/relay 会**同时监听主机回环与 tailnet**：

- 本机调试继续用 `127.0.0.1:7900`
- 远程设备用 `<节点名>:7900` 或 `100.x.y.z:7900`

只监听 tailnet 会让本机展示端突然连不上，这是刻意避免的。

## 4. 首次登录

- 有 `auth_key`：自动登录，无需干预。
- 没有 key 也没有历史身份：会一直等人工授权，日志会明确提示：
  ```
  tailscale：executor 没有授权密钥、也没有已保存的身份（…/tailscale/executor）——这次登录会一直等人工授权
  ```
  此时**主机回环监听仍然正常工作**（不会因为 tailnet 没登录就不可用）。

拿到 IP 后日志会打印：
```
tailscale：节点已上线，IP 100.x.y.z（其它设备可用 <节点名>:端口 访问）
```

## 5. 代价（实测）

| 指标 | 不含 tsnet | 含 tsnet |
| --- | --- | --- |
| `simple-agent` 二进制 | 25.8 MB | **53.3 MB** |
| Android AAR（4 ABI） | 25 MB | **68 MB** |
| 依赖模块数 | 约 20 | 约 610 |
| 是否需要 cgo | 否 | **否**（`CGO_ENABLED=0` 可编译） |

`CGO_ENABLED=0` 依然成立，因此交叉编译、静态链接都不受影响。

> **Android 说明**：`mobile/bridge` 里已经留了 `ConnectTailscale(...)` 接口，但目前
> 默认不启用（AGP 会把 68MB 的 AAR 打进 APK，实测 APK 会到 273MB）。
> 想开的时候，在 `CoreClient.connect` 里调用该方法并把 `enableTailscale` 传 true 即可。

## 6. 实现位置

| 文件 | 作用 |
| --- | --- |
| `internal/tsnetx/` | tsnet 封装：`Server`（Dial/Listen/ListenBoth）、状态目录与节点名规则、合并监听器 |
| `internal/relay/transport.go` | `Dialer` / `Listener` 函数类型，默认退回 `net.Dial` / `net.Listen` |
| `internal/relay/client.go` | 各函数新增 `*Dialer` 变体（旧签名转发到 nil dialer，调用方无需改动） |
| `cmd/simple-agent/tailscale.go` | 三端共用的装配：`startTailscale` / `tailscaleTransport.listen` |
| `internal/config/config.go` | `tailscale:` 配置段 |

设计上的关键取舍：**传输层用函数类型而不是接口**。全仓库的拨号点都只需要
`(network, addr) -> (net.Conn, error)`，函数足够简单，也最好测（一个假函数就能换成进程内管道）。
