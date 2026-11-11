# 账号系统（OIDC）：评估结论

> 状态：**已评估，决定暂不做**（用户 2025-11 决定）
>
> 相关：`docs/relay-security.md`（限流、多凭据等替代方案）

---

## 1. 需求（用户提出）

- relay 登录一个账号，执行端与展示端都登录**同一个账号**才能连接；
- 一个 relay 只绑一个账号；
- 希望用**公开的账号系统**（如 Google），且**国内可访问**。

## 2. 可行性：技术可行，零新依赖

实测（`crypto/rsa` + `encoding/json` + `net/http`，全部标准库）完成 OIDC ID Token
（JWT RS256）的完整验证：

```
header: alg=RS256 kid=k1
验签：通过
声明：sub=user-12345 email=me@example.com
篡改检测：通过（验签失败）
```

也就是说**不需要引入第三方 JWT 库**。所需能力标准库都有：
- `crypto/rsa` / `crypto/ecdsa`：验签
- `encoding/json`：解析 header/claims
- `net/http`：拉 JWKS 公钥（`/.well-known/jwks.json`）

## 3. 设计要点（若将来要做）

```
用户浏览器 ──授权码──▶ IdP（Google / Keycloak / Logto）
                          │ ID Token（JWT，sub = 用户唯一标识）
                          ▼
        relay（只信任一个 IdP + 一个 audience）
          · 启动时绑定 allowed_sub（一个 relay 一个账号）
          · 执行端 / 展示端出示的 sub 必须等于它
```

**核心原则：relay 只"验证"，不"存账号"**——不存密码、不建用户表，
只认 IdP 签发的 `sub`。这正好满足"一个 relay 只绑一个账号"。

配置形态（不要写死某一家 IdP）：

```yaml
auth:
  mode: oidc          # none（默认）/ oidc
  issuer: https://accounts.google.com
  client_id: xxx.apps.googleusercontent.com
  allowed_sub: "1049...8823"
```

## 4. 为什么暂不做

### 4.1 "公开账号"与"国内可直连"有冲突

| IdP | 国内直连 | 备注 |
| --- | --- | --- |
| Google | ❌ | 需代理 |
| GitHub | ⚠️ | 不稳定 |
| Microsoft | ⚠️ | 企业账号常可 |
| 微信 / QQ | ✅ | **非标准 OIDC**，要单独对接 |
| Gitee | ✅ | OAuth2，非标准 OIDC |
| 自建（Keycloak/Logto） | ✅ | 但需自己运维 IdP |

### 4.2 headless 执行端没有浏览器

OIDC 授权码流程需要浏览器。无界面的执行端只能用：
- **设备码流程（Device Flow）**：用户拿 code 去网页确认，实现复杂度显著上升；
- 或**预置 refresh token**：等于又回到"管凭据"。

### 4.3 Android 体验会变差

现在填个地址就能连。加 OIDC 后要：打开 App → 跳浏览器 → 登录 → 深链回调 → 连接。
需要完整的 App Links 配置，容易出错。

### 4.4 相对实际场景，收益有限

场景是"自己的几台设备"。现有机制已覆盖：

| 威胁 | 现有防护 |
| --- | --- |
| 别人连不上 | `--token` |
| 抓包看不到内容 | TLS + 指纹 |
| 猜不出口令 | 失败限流（5 次后退避） |

账号系统额外带来的是"不用管口令"和"能吊销单个设备"。后者用 **C 档多凭据**
成本低得多（约 80 行），且不需要浏览器、不依赖外网、手机体验不变。

## 5. 若将来要做，推荐路线

1. **先做 C 档多凭据**（`docs/relay-security.md` §3.C）：可单独吊销，解决绝大部分痛点；
2. 确实需要 SSO 时，按**通用 OIDC** 实现（issuer/client_id 可配），
   不做 Google 专用硬编码；
3. headless 用设备码流程；Android 用 App Links 深链；
4. relay 只存 `allowed_sub`，不做用户表。

## 6. 当前的安全状态（供对照）

| 能力 | 状态 |
| --- | --- |
| 口令认证（relay + 直连执行端） | ✅ 已实现 |
| 传输加密（标准库 TLS + 指纹） | ✅ 已实现 |
| 失败限流（按 IP，指数退避） | ✅ 已实现 |
| 多凭据 / 可吊销 | ❌ 未做（C 档） |
| HMAC 挑战-应答 | ❌ 未做（有 TLS 后已非必需） |
| 账号系统（OIDC） | ❌ 暂不做（本文档） |
