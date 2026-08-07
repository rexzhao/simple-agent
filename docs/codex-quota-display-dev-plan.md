# Codex 额度显示 — 开发计划

> 起因：需要展示当前 Codex 登录账号的额度 / 用量信息。最初设想是"auth 额外记录
> `chatgpt_account_id` 给额度查询预留"。经实测确认：额度接口 `wham/usage` **只带
> Bearer 即可返回 200**，不强制 `ChatGPT-Account-Id`；而 `chatgpt_account_id` 是
> access token 的 JWT 公共 claim，**需要时可直接从 token 解码，无需落盘持久化**。
> 因此本计划采用"解码不落盘"方案。
>
> **引用约定：下文一律以包名 + 函数名为准，不写死行号。**
>
> 状态：**已实施**（提交包含构建产物）。

---

## 0. 已确认的调研结论（实测证据）

| 项目 | 结论 |
|---|---|
| auth 文件实际内容 | `%APPDATA%/sai/auth/codex.json` 只有 `access_token / refresh_token / expires_at / token_type / token_url / client_id`，**没有 `account_id`，也没有 `chatgpt_account_id`** |
| 运行时 `ChatGPT-Account-Id` | 因 `TokenFile.AccountID` 为空，请求头**从未发送过**（`openai_responses/provider.go` 与 `provider_settings.go` 两处 `if token.AccountID != ""` 均不成立） |
| `chatgpt_account_id` 真实来源 | access token JWT payload 的 claim：`"chatgpt_account_id":"29a91de1-d150-4b81-ad05-7f3c5dab1558"`（另有 `chatgpt_account_user_id`、`poi`(org)、`user_id` 等） |
| `wham/usage` 接口实测 | 只带 `Authorization: Bearer` → **200**，返回完整额度；带正确/错误 `ChatGPT-Account-Id` → 结果一致，**该 header 不参与鉴权** |
| 接口返回体里的 `account_id` | 是 `user-1Qx9rhRkQ9dhHA3jxJd9tr6r`（**用户 id**），不是 `chatgpt_account_id`，也不是 org id |

实测响应样例（`GET https://chatgpt.com/backend-api/wham/usage`，隐藏 token）：

```json
{
  "user_id": "user-1Qx9rhRkQ9dhHA3jxJd9tr6r",
  "account_id": "user-1Qx9rhRkQ9dhHA3jxJd9tr6r",
  "email": "rexzhao.beta@gmail.com",
  "plan_type": "pro",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": { "used_percent": 56, "limit_window_seconds": 604800, "reset_after_seconds": 264817, "reset_at": 1786255839 },
    "secondary_window": null
  },
  "code_review_rate_limit": null,
  "additional_rate_limits": [
    {
      "limit_name": "GPT-5.3-Codex-Spark",
      "metered_feature": "codex_bengalfox",
      "rate_limit": {
        "allowed": true,
        "limit_reached": false,
        "primary_window": { "used_percent": 0, "limit_window_seconds": 604800, "reset_after_seconds": 604800, "reset_at": 1786595823 },
        "secondary_window": null
      }
    }
  ],
  "credits": {
    "has_credits": false, "unlimited": false, "overage_limit_reached": false,
    "balance": "0", "approx_local_messages": [0, 0], "approx_cloud_messages": [0, 0]
  },
  "spend_control": { "reached": false, "individual_limit": null },
  "rate_limit_reached_type": null,
  "promo": null,
  "rate_limit_reset_credits": { "available_count": 1, "applicable_available_count": 0 }
}
```

---

## 1. 目标与非目标

### 目标
- 在已有 Web 的 Provider 管理界面（`ProviderManagerDialog` 的 Codex sign-in 卡片）新增
  **额度显示**：点击刷新，展示 plan、本周用量百分比、重置时间、credits 余额、附加额度列表。
- 复用现有 Codex token 刷新链路（`codexauth.TokenSource`），token 过期自动刷新后再查额度。
- `chatgpt_account_id` 不落盘，需要时从 access token JWT 解码。

### 非目标（本次不做）
- 不修改 `ChatGPT-Account-Id` 请求头语义（`openai_responses/provider.go` 的运行时行为保持不变）。
- 不给 `TokenFile` / `CodexAuthStatus` 增加 `chatgpt_account_id` 持久化字段。
- 不做 CLI 命令（如 `sai quota`）；如后续需要可复用 execution 层方法。
- 不做额度变更告警 / 自动轮询。

---

## 2. 依赖与新增依赖评估

- 新增 HTTP 请求：`GET https://chatgpt.com/backend-api/wham/usage`。
- **JWT 解码不引入新依赖**：`go.mod` 目前只有 `websocket / yaml / sqlite` 等。仅需
  `encoding/base64` + `encoding/json` 对 access token 的中间段做 payload 解码即可（不校验
  签名——token 来自我们自己的 auth 文件，只做 claim 提取）。
- 请求客户端复用 `execution.providerHTTPClient`（自动带 provider 的 proxy / 并发限制）。

---

## 3. 数据模型设计

### 3.1 额度响应类型（`internal/execution` 新增，贴近 `wham/usage` 实际 schema）

```go
type CodexUsage struct {
    UserID    string `json:"user_id"`
    AccountID string `json:"account_id"`   // 注意：这是用户 id，不是 chatgpt_account_id
    Email     string `json:"email"`
    PlanType  string `json:"plan_type"`

    RateLimit *CodexUsageWindowSet `json:"rate_limit"`
    AdditionalRateLimits []CodexUsageAdditional `json:"additional_rate_limits"`

    Credits *CodexUsageCredits `json:"credits"`
}

type CodexUsageWindowSet struct {
    Allowed        bool     `json:"allowed"`
    LimitReached   bool     `json:"limit_reached"`
    PrimaryWindow  *CodexUsageWindow `json:"primary_window"`
    SecondaryWindow *CodexUsageWindow `json:"secondary_window"`
}

type CodexUsageWindow struct {
    UsedPercent         int64 `json:"used_percent"`
    LimitWindowSeconds  int64 `json:"limit_window_seconds"`
    ResetAfterSeconds   int64 `json:"reset_after_seconds"`
    ResetAt             int64 `json:"reset_at"`   // unix 秒
}

type CodexUsageAdditional struct {
    LimitName     string `json:"limit_name"`
    MeteredFeature string `json:"metered_feature"`
    RateLimit     *CodexUsageWindowSet `json:"rate_limit"`
}

type CodexUsageCredits struct {
    HasCredits          bool   `json:"has_credits"`
    Unlimited           bool   `json:"unlimited"`
    OverageLimitReached bool   `json:"overage_limit_reached"`
    Balance             string `json:"balance"`
    ApproxLocalMessages []int  `json:"approx_local_messages"`
    ApproxCloudMessages []int  `json:"approx_cloud_messages"`
}
```

> 说明：只映射前端需要的字段；`spend_control / promo / rate_limit_reset_credits` 等先不建模，
> 需要时再补。未知字段用 `json.RawMessage` 或直接忽略均可（解码宽容）。

### 3.2 JWT claim 解码（`internal/codexauth` 新增）

```go
// Claims 从 access token 中提取的公开 claim（不校验签名）。
type Claims struct {
    ChatGPTAccountID    string `json:"chatgpt_account_id"`
    ChatGPTAccountUserID string `json:"chatgpt_account_user_id"`
    UserID              string `json:"user_id"`
    POI                 string `json:"poi"`
    PlanType            string `json:"chatgpt_plan_type"`
    SessionID           string `json:"session_id"`
}

// DecodeClaims 解码 JWT 的 payload 段（base64url, 自动补 padding）。
func DecodeClaims(accessToken string) (Claims, error)
```

实现要点：
- 按 `.` 切分，取第 2 段；`base64.RawURLEncoding` 解码，长度不足 4 的倍数时按标准补 `=`。
- 解码失败返回带上下文错误；缺失字段返回空串不报错。
- 单测覆盖：真实格式 token（含 `chatgpt_account_id`）、缺字段、坏 base64、非 3 段。

---

## 3.5 界面显示方案（UI 规格）

### 位置
在现有 **Provider 管理弹窗**（`ProviderManagerDialog`）的 **Codex sign-in 卡片**
（`codex-auth-card`）内，账号行（`Account: <code>`、`Expires:`）下方新增一个 **Usage 区块**。
不新增独立页面；quota 信息跟随当前选中的 provider 显示。

### 交互状态机
| 状态 | 显示 |
|---|---|
| 未登录（`signed_out` / 无 auth） | 不显示 Usage 区块（登录后才有额度可查） |
| 已登录（`signed_in` / `expired`） | 显示「Usage」刷新按钮 + 上次结果（若有） |
| 点击刷新 / 进行中 | 按钮变 loading（`Fetching…` 禁用态），沿用 `discovering` 那套模式 |
| 查询失败 | 按钮下显示 `settings-error` 行（错误信息脱敏，不含 token） |
| 查询成功 | 显示数据行（见下） |

数据存组件 state：`const [codexUsage, setCodexUsage] = useState<CodexUsage | null>(null)`；
`const [usageLoading, setUsageLoading] = useState(false)`。切 provider 时重置为 null。

### 展示内容（成功态，从上到下）

**额度窗口全部动态渲染**：不写死"周额度"或"5h 额度"，而是对 `rate_limit` 下
**任意存在的窗口**统一渲染。`used_percent` 来自接口，窗口时长按 `limit_window_seconds`
换算文案；`secondary_window` 存在就显示，不存在就跳过。

```
┌─ Codex sign-in  [Signed in]  ─────────────────────────┐
│  Account: 29a91de1…（截断短 id）   Expires: …          │
│                                                        │
│  Usage                              [Refresh usage]    │
│  Plan: Pro                                             │
│  Window · 7 days:   ▓▓▓▓▓▓▓░░░  58%   (重置于 8/9 14:10)
│  Window · 5 hours:  ░░░░░░░░░░   0%   (重置于 …)       │  ← 服务端返回才显示
│  Credits: balance 0   (无单独 credits)                 │
│  GPT-5.3-Codex-Spark:  0%                             │
└────────────────────────────────────────────────────────┘
```

具体字段映射（来自 `CodexUsage`）：

1. **Plan**：`plan_type` 大写展示（`pro` → `Pro`）。
2. **额度窗口 `rate_limit`**（动态，不写死周期）：
   - 遍历 `primary_window` 和（若存在）`secondary_window`，每个窗口渲染一行：
     - 窗口时长文案：`limit_window_seconds`（秒）→ 人性化（< 3600 用分钟，< 86400 用小时，
       否则用天；如 `604800` → `7 days`，`18000` → `5 hours`）；
     - 进度条 + `used_percent%`；
     - 重置时间：`reset_at`（unix 秒）→ `new Date(1000*reset_at).toLocaleString()`；
     - 若该窗口 `limit_reached` 为 true，进度条红色并加「Limited」徽标。
   - 某窗口缺失或为 null 时跳过，不占位。
3. **Credits** `credits`：
   - `has_credits` 为 true 时显示 `balance`（`Balance` 是字符串原样显示）；
   - 否则显示「无单独 credits」；`unlimited` 为 true 显示「Unlimited」。
4. **附加额度** `additional_rate_limits[]`：每行 `limit_name: 百分比%`
   （取该项 `rate_limit.primary_window.used_percent`；若该项也有 `secondary_window`，
   同样按第 2 条规则补充显示）。
5. 若 `rate_limit` 整体缺失或 `allowed=false`，显示对应提示行（"无额度数据"/"额度受限"），
   不崩溃。

> 说明：这样设计的好处是——服务端**未来新增任何窗口周期（5h、10h、日窗口等）时，
> 界面自动跟随显示，无需改代码**。当前实测只有 `primary_window`（7 天），所以现在只会
> 显示一行窗口。

### 进度条样式（新增少量 CSS，复用现有 design token）
- 复用 `settings-section` / `codex-auth-card` 排版；新增 `.usage-row`、`.usage-meter`、
  `.usage-meter > span`（填充）、`.usage-badge`（`Limited`/`Unlimited`）等类。
- 进度条：外层 2px 边框 + 内层按百分比填充，颜色 `var(--accent)`；超 90% 或 `limit_reached`
  用 `var(--danger)`。
- 与现有 `:root` 的 `--accent / --danger / --success / --muted / --border` 保持一致，
  字号沿用卡片内 9.5–10px 规格。

### 交互细节
- 仅在 `codexAuth?.status === 'signed_in' || 'expired'` 时显示「Refresh usage」按钮；
- 按钮禁用条件：`usageLoading` 或 `!savedCodexProvider`；
- 刷新时不阻塞卡片其他操作；失败不清空上次成功结果（仅显示错误行）。

---

## 4. 改动清单（文件级）

### 4.1 `internal/codexauth`
- 新增 `jwt.go`：`Claims` 结构 + `DecodeClaims`（见 3.2）。
- 新增 `jwt_test.go`：解码单测。

### 4.2 `internal/execution`
- 新增 `codex_usage.go`：
  - `func (s *Service) CodexUsage(ctx context.Context, providerName string) (CodexUsage, error)`
  - 流程：
    1. `s.codexProvider(providerName)` 取 provider（复用现成校验：必须是 openai-codex、有 auth_file）。
    2. `(&codexauth.TokenSource{Store: codexauth.Store{Path: provider.AuthFile}, HTTPClient: client}).AccessToken(ctx)` 取 token（内部自动刷新）。
    3. `codexauth.DecodeClaims(token.Token)` 取 `ChatGPTAccountID`（可选 header；拿不到不报错）。
    4. 组请求 `GET <usageURL>`，`Authorization: Bearer <token>`，若 claim 非空加
       `ChatGPT-Account-Id: <claim>`，`User-Agent: codex-cli`，`Accept: application/json`。
    5. 非 2xx 时错误信息做脱敏（沿用 `codexauth` 的脱敏思路，不回显 token）。
    6. `json.NewDecoder` 解码为 `CodexUsage`。
  - **usageURL 解析策略**：优先从 `provider.BaseURL` 推导——把末尾 `/backend-api/codex`
    替换为 `/backend-api/wham/usage`（兼容镜像/代理 base_url）；推导失败则回落到
    `codexauth.DefaultUsageURL = "https://chatgpt.com/backend-api/wham/usage"`（新增常量）。
- 新增 `codex_usage_test.go`：httptest 假端点，覆盖成功解析、401 错误脱敏、非 codex provider 拒绝。

### 4.3 `internal/webapp`
- `provider_settings.go` 新增 handler：
  `func (s *Server) handleCodexUsage(w http.ResponseWriter, r *http.Request)`
  - 调 `s.service.CodexUsage(r.Context(), r.PathValue("providerName"))`；
  - 错误走 `writeServiceError`；成功 `writeJSON(w, http.StatusOK, usage)`。
- `server.go` `routes()` 注册：
  `s.mux.HandleFunc("GET /api/providers/{providerName}/codex-usage", s.handleCodexUsage)`
- `server_behavior_test.go` 增加行为测试：对非 codex provider 返回 422 `request_failed`；
  对 codex provider + 假端点返回 200 且 JSON 结构正确。

### 4.4 `web/src`
- `api.ts` 新增：
  `codexUsage: (providerName: string) => request<CodexUsage>(`/api/providers/${encodeURIComponent(providerName)}/codex-usage`)`
- `types.ts` 新增 `CodexUsage / CodexUsageWindowSet / CodexUsageWindow / CodexUsageAdditional / CodexUsageCredits` 类型。
- `ProviderManagerDialog.tsx`：按 §3.5 的 UI 规格实现 Usage 区块（新增 state、`refreshUsage` 回调、
  渲染进度条/字段行、错误行）。
- `styles.css`：新增 `.usage-row / .usage-meter / .usage-badge` 等少量样式（§3.5）。

---

## 5. 测试计划

| 层 | 用例 |
|---|---|
| `codexauth` | `DecodeClaims` 正常解码出 `chatgpt_account_id`；缺字段返回空；坏 base64 / 非 3 段报错；补 padding 正确 |
| `execution` | `CodexUsage` 成功解析完整响应；401 时错误信息不含 token；`DecodeClaims` 失败时仍能继续（header 缺失）查询；非 codex provider 报错；usageURL 从 base_url 正确推导 + 回落到默认值 |
| `webapp` | `GET /api/providers/{name}/codex-usage` 对 openai-codex provider 返回 200 结构正确；对非 codex provider 返回 422 `request_failed` |
| 回归 | `go test ./...`；`web` `npm run build`（前端类型检查） |

---

## 6. 手工验证步骤

1. 启动应用，进入 Provider 管理，选中 `codex` provider（已登录）。
2. 点击 Usage 刷新，确认显示 `plan_type: pro`、`used_percent: 56`、重置时间、`balance: 0`。
3. 对照 `curl 'https://chatgpt.com/backend-api/wham/usage' -H "Authorization: Bearer $TOKEN"`
   的原始输出，确认 UI 数据一致。
4. 断开网络/改坏 auth 文件，确认错误提示不含 token、不崩溃。
5. 确认 auth 文件 `codex.json` 内容**没有新增字段**（验证"不落盘"）。

---

## 7. 风险与注意事项

- **接口稳定性**：`wham/usage` 是 ChatGPT 内部接口，schema / 鉴权可能变化。实现上解码要宽容
  （未知字段忽略、核心字段缺失显示占位）。
- **隐私**：响应含邮箱。只在本地 UI 展示，不进日志；日志/错误信息一律脱敏。
- **多账号**：一个 provider 一个 auth 文件，额度按当前 auth 文件账号查询；多 provider 各自独立。
- **不改变现有 `ChatGPT-Account-Id` 运行时语义**：本次只在新查询接口里加该 header（可选），
  运行时请求路径不动。
- **并发限制**：`providerHTTPClient` 的并发限制只作用于该 client 的请求；额度查询是低频操作，无压力。

---

## 8. 后续可扩展（不在本次范围）

- CLI 命令 `sai quota`：直接复用 `execution.CodexUsage`。
- 额度自动刷新 / 用量告警。
- 若未来确需在运行时发送 `chatgpt_account_id`，可改为在 `codexResponsesTokenSource` 解码 JWT
  claim 填充 `AccessToken.AccountID`（单独变更、单独验证，不混入本次）。