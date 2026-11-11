# 加密隧道：现成库选型（实测）

> 状态：**仅评估，未实现**
>
> 配套：`docs/relay-security.md` 的 D 档（传输加密）

---

## 0. 结论先说

| 方案 | 依赖 | 是否纯 Go | Android 可编译 | 工作量 | 推荐 |
| --- | --- | --- | --- | --- | --- |
| **标准库 `crypto/tls`** | **0 个新依赖** | ✅ | ✅ | ~120 行 | **推荐** |
| `flynn/noise`（Noise 协议） | +1 直接依赖（+`x/crypto`） | ✅ | ✅ | ~150 行 | 可选，若是想避开 TLS 的证书体系 |
| `wireguard-go` | 大（~10+） | ✅ | 需验证 | 大 | 不推荐（tsnet 就是它，已移除） |

**推荐标准库 `crypto/tls`**：零新依赖、Android 与三平台都实测通过、
且"证书指纹校验"这套用法可以直接替代 CA 体系（不用买证书、不用配 CA）。

---

## 1. 实测结果

### 1.1 标准库 `crypto/tls`

已验证可编译运行（`crypto/tls` 全部在标准库）：

```go
// 服务端
cert, err := tls.LoadX509KeyPair(certFile, keyFile)
ln, err := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})

// 客户端：不走 CA，只认写死的指纹（TOFU 流程）
cfg := &tls.Config{
    InsecureSkipVerify: true,
    VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
        sum := sha256.Sum256(rawCerts[0])
        if hex.EncodeToString(sum[:]) != wantFingerprint {
            return errors.New("证书指纹不匹配")
        }
        return nil
    },
}
```

**代价**：
- 需要生成并保存一对自签证书（`crypto/x509` + `crypto/ecdsa`，约 40 行工具函数）；
- 首次连接要把服务端指纹给用户确认一次（TOFU，类似 SSH 的 `known_hosts`）。

**为什么不用 CA 链**：自用场景没有域名、不想依赖公共 CA；指纹写死等价于
"只信任这一台 relay"，比 CA 更严格（CA 签的任何证书都不认）。

### 1.2 `flynn/noise`（Noise 协议框架）

**实测通过**：XX 模式三次握手 + 双向加密 + 篡改检测：

```
c->s 加密 34 字节（原文 18），解密: "hello noise tunnel"
s->c 解密: "pong"
篡改检测: 通过（解密失败）
PASS: noise 握手 + 双向加密 + 完整性校验均可用
```

依赖情况（实测 `go list -m all` 共 12 个，但**编译进二进制的只有 2 个**）：

```
github.com/flynn/noise v1.1.0       ← 真正引入的
golang.org/x/crypto v0.58.0         ← curve25519 + chacha20poly1305 + blake2s
（kr/pretty, kr/pty, kr/text, check.v1 等都是测试依赖，不进二进制）
```

- 纯 Go，`CGO_ENABLED=0` 可编译；
- **android/arm64 与 darwin/amd64 实测编译通过**。

**与 TLS 的取舍**：

| | `crypto/tls` | `flynn/noise` |
| --- | --- | --- |
| 新依赖 | 无 | 2 个（noise + x/crypto） |
| 证书管理 | 需要（自签 + 指纹） | 需要（双方静态公钥，等价复杂度） |
| 协议开销 | TLS 1.3 握手 ~1 RTT | Noise XX 1.5 RTT |
| 生态/审计 | 标准库，最广泛 | 小众但设计干净，被 Tailscale/WireGuard 生态使用 |
| 与"按包收发"的契合 | 需要自己分帧（TLS 是流） | **天然按消息**，`Encrypt/Decrypt` 直接对应一个包 |

**Noise 唯一的实质优势**：它是**按消息**加密的（`CipherState.Encrypt` 一次一个消息），
而 TLS 是**流**，套在现有 `wire.Conn`（按包收发）下面需要多一层长度前缀分帧——
不过 `wire` 本来就已经有 4 字节长度前缀，所以这点差异在实践中不明显。

### 1.3 `wireguard-go`（不推荐）

- 依赖重（tsnet 内部就是它，之前引入 tailscale 时依赖膨胀到 610 个模块）；
- 需要 TUN 设备或 userspace 网络栈，在 Android 上还要 VpnService 权限；
- 对"relay 与客户端之间加密"这个需求来说**明显过重**。

---

## 2. 建议的实现形态（若选 TLS）

在 `wire.NewTCPConn` **之前**插一层，包格式完全不变：

```go
// 服务端
raw, _ := ln.Accept()                 // *tls.Conn
packetConn := wire.NewTCPConn(raw)    // 上层无感知

// 客户端
raw, _ := tls.Dial("tcp", addr, cfg)
packetConn := wire.NewTCPConn(raw)
```

落地位置只需改三个"监听/拨号"入口：

| 位置 | 改动 |
| --- | --- |
| `cmd/simple-agent/remote.go` 的 `startListener` | `net.Listen` → `tls.Listen` |
| `cmd/simple-agent/headless.go` 的 `listenHeadless` / `runRelayServer` | 同上 |
| `internal/wire/tcp.go` 的 `Dial` | `net.Dial` → `tls.Dial`（配置从参数传入） |

工作量估算（含测试）：

| 块 | 行数 |
| --- | --- |
| 证书生成/加载（`crypto/x509` 自签 + 存盘） | ~60 |
| 指纹校验（客户端） | ~30 |
| 四个入口接线 | ~30 |
| 首次连接确认指纹（TOFU）的交互/配置 | ~40 |
| 测试（握手成功/指纹不符/明文兼容） | ~80 |

合计约 240 行（比 `docs/relay-security.md` 里"120 行"的估计更实际——
那部分只算了接线，没算证书与 TOFU）。

---

## 3. 与既有 token 的关系

加密之后 token 仍建议保留（纵深防御）：

- TLS 防"监听"与"中间人"；
- token 防"能连上但没凭据的人"（比如同一台机器上的其它用户，
  或指纹被接受后换了客户端）。

两者的顺序：**先 TLS 握手（验指纹），再在加密通道内做 token 校验**。
这样 token 天然不会明文出现，`docs/relay-security.md` 的 A 档（HMAC 挑战-应答）
在做了 TLS 之后可以**省掉**——这是 TLS 的额外收益。

---

## 4. 兼容策略

- 新增 `--tls` / `--tls-cert` / `--tls-key` / `--tls-fingerprint`（或配置项）；
- 默认**不启用**，保持现有明文行为（不破坏已有部署）；
- 服务端可同时开两个监听端口（明文 + TLS），便于渐进迁移；
- 协议层无变化，所以"明文客户端"和"TLS 客户端"不需要两套代码。
