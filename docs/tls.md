# 传输加密（TLS）：用法

> 实现：`internal/tlsx`（标准库 `crypto/tls`，**零第三方依赖**）
>
> 背景与选型对比：`docs/encryption-options.md`

---

## 0. 默认关闭

不传任何 `--tls-*` 参数时，一切走**明文 TCP**，行为与以前完全一样。
只在需要时才开启。

## 1. 快速开始

### 1.1 服务端（relay 或执行端监听）

```powershell
# 第一次运行会自动生成自签证书，并把指纹打印出来
simple-agent relay --listen 0.0.0.0:7900 --token 口令 `
  --tls-cert ./tls/cert.pem --tls-key ./tls/key.pem
```

启动日志会给出客户端需要的东西：

```
TLS：已启用服务端加密（证书 ./tls/cert.pem）
TLS：证书指纹 12:07:b1:63:...:61:61
TLS：客户端需要加 --tls-fingerprint 1207b163...6161
```

只想看指纹（不启动服务）：

```powershell
simple-agent --tls-cert ./tls/cert.pem --tls-key ./tls/key.pem --tls-print-fingerprint
```

### 1.2 客户端（展示端 / 执行端注册 relay）

```powershell
simple-agent --connect 10.0.0.5:7900 --token 口令 `
  --tls-fingerprint 1207b163d52f2ed9dc4ff94334f683787c7a7fc7076dfbc3178eced586b86161
```

指纹可以带冒号、大写，都会被规范化：

```powershell
--tls-fingerprint 12:07:B1:63:...   # 与上面等价
```

### 1.3 配置文件

```yaml
tls:
  # 服务端
  cert_file: /var/lib/simple-agent/tls/cert.pem
  key_file: /var/lib/simple-agent/tls/key.pem
  # 客户端（只填要连的那台 relay 的指纹）
  # fingerprint: 1207b163d52f2ed9dc4ff94334f683787c7a7fc7076dfbc3178eced586b86161
```

## 2. 为什么用指纹而不是 CA

自用场景没有域名、也不想买证书。客户端**写死 relay 证书的 SHA-256 指纹**：

- 只有指纹**完全一致**才建立连接；
- 不校验 CA 链——CA 签发的任何证书都不认，**只认这一台 relay**；
- 比 CA 更严格（CA 体系里任何受信 CA 都能签，这里只有一个答案）。

代价：首次连接要人工把指纹从服务端日志抄到客户端配置（类似 SSH 的 `known_hosts`）。

> 注意：`InsecureSkipVerify: true` 在这里**不是"跳过校验"**，而是关闭 CA 链
> 再在 `VerifyPeerCertificate` 里做更严格的指纹比对。

## 3. 证书自动生成

`--tls-cert` / `--tls-key` 指向的文件**不存在时会自动生成自签证书**：

- ECDSA P-256，有效期 10 年；
- `CN` 取主机名，SAN 包含 `localhost` / `127.0.0.1` / `::1`；
- 私钥权限 `0600`；
- 已存在则直接复用（指纹稳定，客户端不用重配）。

想换证书就删掉这两个文件重启。

## 4. 与 token 的关系

**两者互不替代，建议同时用**：

| | 作用 | 防什么 |
| --- | --- | --- |
| TLS（指纹） | 加密通道 + 确认对端身份 | 被监听、中间人 |
| `--token` | 口令校验 | 能连上端口但没有凭据的人 |

顺序是"先 TLS 握手（验指纹），再在**加密通道内**校验 token"——
所以开了 TLS 之后，token 天然不会明文出现。

## 5. 行为细节

| 情况 | 行为 |
| --- | --- |
| 只给 `--tls-cert/--tls-key` | 监听用 TLS；拨号仍是明文（并提示"要加密拨号请加指纹"） |
| 只给 `--tls-fingerprint` | 拨号用 TLS；监听仍是明文 |
| 两者都给 | 监听与拨号都加密（同一进程既当服务端又当客户端时用） |
| 证书准备失败 | **直接退出**，不降级成明文（否则用户会误以为已加密） |
| 指纹长度不对 | 启动时报错退出，不会连上才发现 |
| 指纹不匹配 | 连接失败，错误信息带"可能是证书指纹不匹配"提示 |

## 6. 实现位置

| 文件 | 作用 |
| --- | --- |
| `internal/tlsx/tlsx.go` | `Config.Listen/Dial`、自签证书生成、指纹校验、`SetActive` |
| `internal/wire/tcp.go` | `SetDialFunc` 钩子——让所有 `wire.Dial` 调用点自动走 TLS |
| `cmd/simple-agent/tls.go` | `setupTLS`：按参数装配，必须在任何连接建立之前 |
| `cmd/simple-agent/remote.go` | `--tls-cert/--tls-key/--tls-fingerprint/--tls-print-fingerprint` |
| `internal/config/config.go` | `tls:` 配置段 |

**为什么用包级 `SetActive` 而不是构建参数**：TLS 是"全进程一个开关"，
而 `wire.Dial`、`relay.*`、`host` 各层的调用点很多——把配置穿过所有签名
会让改动面失控。`SetDialFunc` 同理。

## 7. 已知限制

- **只有服务端加 TLS 时，本进程拨号仍是明文**：因为"要不要加密拨号"取决于
  对端是谁，不能靠自己的证书推断。需要时显式加 `--tls-fingerprint`。
- **没有证书轮换机制**：换证书后所有客户端要更新指纹。自用场景可接受。
- **不做双向认证（mTLS）**：服务端不验证客户端证书，客户端靠 token 证明身份。
  若需要 mTLS，`tlsx` 里加 `ClientAuth` 即可，约 30 行。
