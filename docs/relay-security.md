# relay 访问保护：现状与改进方案

> 状态：**仅设计，未实现**（用户要求先出方案）
>
> 相关：`docs/remote-design.md` §9（安全的预留）、`internal/relay/relay.go`

---

## 1. 现状（v1，已实现）

relay 用**一个固定 token** 保护，执行端与展示端连上来时在首包里带上它。

```go
// internal/relay/relay.go
type Relay struct {
    // Token 非空时要求所有连上来的执行端与展示端都带同一个固定 token。
    Token string
}

type tokenPayload struct {
    Token string `json:"token,omitempty"`
}

func sameToken(got, want string) bool {
    return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}
```

校验点在 `Relay.Serve` 的**首包**（分发之前）：

| 首包类型 | 校验字段 |
| --- | --- |
| `relay.register`（执行端） | `Executor.Token` |
| `relay.route`（展示端） | 请求里的 `token` |
| `relay.list`（查询列表） | `tokenPayload.Token` |
| `hello`（展示端探测对端） | `wire.Hello.Credentials.Token` |

执行端直连（`--listen`）用另一套同样的口令校验（`internal/host` 的 `Local.authorize`）。

**已经做对的部分**：

- 用**定长比较**（`subtle.ConstantTimeCompare`），不能靠响应时间逐字节试探；
- 校验发生在分发之前，未认证的人连"有哪些执行端"都查不到；
- 拒绝时回一条带原因的帧（客户端能区分"token 不对"和"连不上"）；
- 执行端侧拒绝后**关闭连接**（不关的话后续 `catalog`/`cmd` 会被继续处理，等于认证被绕过——这个已经修过）。

## 2. 弱点

| # | 弱点 | 后果 | 严重度 |
| --- | --- | --- | --- |
| W1 | token 在首包**明文**传输 | 同网段抓包即可获得，之后可冒充任意一端 | 高（在不可信网络上） |
| W2 | **无速率限制** | 可无限次尝试；弱口令几分钟被穷举 | 高 |
| W3 | 单一共享口令 | 一台设备泄露就要全员换口令 | 中 |
| W4 | 明文存于配置文件 | 配置泄露 = token 泄露 | 中 |
| W5 | 失败**无日志/无告警** | 有人扫描你不知道 | 中 |
| W6 | 无重放防护 | 抓到的首包可原样重放 | 低（现状下重放同一 token 无额外收益，但换成挑战-应答后必须有） |
| W7 | 不加密流量 | 会话内容（含工具输出、代码）可被监听 | 高（不可信网络） |

> 注：W1/W7 是同一类根因（缺少加密），A 档只解决"口令不外泄"，D 档才解决"内容不外泄"。

## 3. 方案

四档可以独立上线，按投入排序。

### A. 握手挑战-应答（HMAC）

**解决**：W1（口令不再明文传输）、W6（防重放）。

协议（新增两条 relay 层消息）：

```
client → relay : {kind:"relay.auth.begin", role:"executor"|"viewer"}
relay  → client: {kind:"relay.auth.challenge", nonce:"<32 字节 base64>", ts:<unix 秒>}
client → relay : {kind:"relay.auth.response",
                  hmac:"<base64(HMAC-SHA256(key=token, msg=nonce|role|ts))>",
                  ts:<unix 秒>}
relay  → client: {kind:"relay.auth.ok"} 或 {kind:"relay.auth.error", error:"..."}
```

校验规则：

1. `|now - ts| <= 30s`（时钟偏差容忍窗口）；
2. nonce 必须**未用过**（relay 侧留最近 N 秒的 nonce 集合，过期即丢）；
3. HMAC 用定长比较。

改法要点：

- `Relay` 增加 `nonces map[string]time.Time` + 定期清理（或带时间戳的 ring）；
- `authorize` 换成三步握手，**原 `Token` 字段保留为兼容开关**（见 §5 兼容策略）；
- 执行端与展示端的 `wire.Hello.Credentials` 换成先走 `relay.auth.*` 再发 hello；
- 直连执行端（`--listen`）复用**同一套**逻辑（抽到 `internal/auth` 共享），避免两处实现分叉。

工作量：约 100 行 + 测试（挑战成功 / 错误口令 / 过期 ts / 重放 nonce / 时钟偏差边界）。

### B. 失败限流与审计日志

**解决**：W2（暴力尝试）、W5（无感知）。

规则：

- 按 **源 IP** 计数：连续失败 N 次（默认 5）后进入退避，延迟 `1s × 2^(n-N)`，上限 5 分钟；
- 期间**仍然接受**连接但慢速拒绝（避免给攻击者"是否命中"的即时反馈差异）；
- 或更简单：直接拒绝并计入黑名单 TTL（默认 10 分钟）；
- 成功后清零计数；
- 日志：`relay：认证失败 ip=1.2.3.4 role=viewer total_failures=12`，仅在**首次进入退避**和**解除**时各打一条（避免日志洪水）。

工作量：约 60 行 + 测试（达到阈值后确实被拖慢/拒绝、成功后计数清零）。

### C. 多凭据 + 哈希存储

**解决**：W3（不可吊销）、W4（明文存储）。

配置形态：

```yaml
relay:
  # 每个客户端一个凭据，可单独吊销
  clients:
    - name: phone
      # argon2id 或 bcrypt 哈希；用 `simple-agent relay --hash-token` 生成
      token_hash: "$argon2id$v=19$m=65536,t=3,p=2$..."
    - name: laptop
      token_hash: "$argon2id$..."
```

要点：

- 认证成功后把 `name` 记进连接上下文，后续日志能打出"是谁"；
- 失败日志只在**凭据名匹配到但口令错**时打出名字（口令完全不匹配时不泄露"这个名字存在"）；
- **哈希算法用标准库就够**：Go 1.24+ 的 `crypto/pbkdf2` 已在标准库中
  （`crypto/pbkdf2.Key(sha256.New, token, salt, iter, keyLen)`）。
  推荐 PBKDF2-HMAC-SHA256 + 每凭据独立随机 salt + 迭代 ≥ 60 万次（OWASP 2023 建议值）。
  这样 C 档**不需要引入任何第三方依赖**。
  - 说明：`golang.org/x/crypto` 里的 argon2id 抗 GPU 更强，但标准库没有；
    对"自己几台设备"的场景，PBKDF2 + 强口令 + B 档限流已经足够。是否升级到 argon2
    取决于你是否接受新增依赖。
  - **不要**用裸 `SHA-256(salt||token)`：GPU 下每秒可试数十亿次，配合弱口令等于没有。

工作量：约 80 行（含 PBKDF2 编解码）+ 配置解析 + 一个生成哈希的子命令（`relay --hash-token`）。

### D. 传输加密（TLS）

**解决**：W7（内容可被监听）、并顺带彻底解决 W1。

用**标准库 `crypto/tls`**，不引入第三方依赖：

```go
// relay 侧
cert, _ := tls.LoadX509KeyPair(certFile, keyFile)   // 或自签自动生成
ln := tls.Listen("tcp", addr, &tls.Config{Certificates: []tls.Certificate{cert}})

// 客户端侧：校验服务端证书指纹（不用 CA）
cfg := &tls.Config{
    InsecureSkipVerify: true,          // 关掉 CA 链校验
    VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
        // 只接受配置里写死的指纹
        return checkFingerprint(raw[0], wantFingerprint)
    },
}
```

要点：

- **证书分发**是主要成本：首次连接时把服务端指纹打印出来，用户手工确认（TOFU，类似 SSH）；
- 包格式不变（`wire.NewTCPConn` 之前包一层 TLS），协议层零改动；
- 若要双向认证（mTLS），客户端也带证书，则 §C 的多凭据可以省掉。

工作量：约 120 行（含指纹确认流程）+ 证书生成/存储。

> 已实测确认：`crypto/tls` 的 `LoadX509KeyPair` / `tls.Listen` /
> `InsecureSkipVerify + VerifyPeerCertificate` 这套组合在本项目 Go 版本下可直接编译使用，
> 全部在标准库，**不引入第三方依赖**。

## 4. 建议顺序

| 顺序 | 内容 | 收益 | 成本 |
| --- | --- | --- | --- |
| 1 | **B 限流 + 日志** | 挡住绝大多数扫描与穷举 | 60 行 |
| 2 | **A HMAC 挑战-应答** | 口令不再明文传输 | 100 行 |
| 3 | **C 多凭据** | 可吊销、可审计 | 80 行 + 可能新增 `x/crypto` |
| 4 | **D TLS** | 彻底防监听 | 120 行 + 证书流程 |

**A + B 合计约 160 行**，不需要任何第三方依赖，是性价比最高的一档。
D 单独做的收益最大（解决内容泄露），但证书分发流程要设计好，否则用户体验会明显变差。

## 5. 兼容与迁移

- **不破坏现有部署**：保留 `relay --token` 语义；新增的挑战-应答作为**可选**模式
  （`relay --auth hmac` 或配置项），旧客户端仍可用明文 token 连（但要打一条告警日志）。
- 协议带版本：`relay.auth.begin` 里带 `proto` 字段，relay 可同时接受新旧流程。
- 执行端与展示端升级后自动优先尝试新流程。

## 6. 不做的事

- **不做账号体系**（用户量级是"自己的几台设备"，不需要用户注册/权限模型）；
- **不做 HMAC + TLS 之外的自研加密**（自研加密协议的风险远大于收益）；
- **不把 token 写进日志**（任何分支都不打印完整 token，只在前缀/哈希后打印）。

## 7. 附：与"用外部隧道"的关系

如果 relay 只监听回环、通过 SSH 端口转发或 WireGuard 隧道访问，那么 W1/W7 都由隧道解决，
此时 A/D 的紧迫性下降，**B（限流）仍然有价值**（挡住本机/内网里的误连与扫描）。

即：**隧道是更省事的方案，代码内的保护是纵深防御**。两者不冲突。
