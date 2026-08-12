# 思考配置（budget_tokens）与 max_tokens 自动注入 — 开发计划

> 起因：自定义 Provider 增加模型时，希望用 [models.dev](https://models.dev/api.json)
> 的数据自动填充模型参数（只填 model 名字，列表列出匹配的 provider/models，自动填充，
> 详细参数折叠、需要时展开）。调研后发现 models.dev 的 `reasoning_options` 有两种形态：
> `effort`（字符串档位）和 `budget_tokens`（数值预算），而当前 `reasoning_config` 只支持
> `effort` 型。同时确认 Anthropic Messages 请求体的 `max_tokens` 是必填字段，当前代码
> **没有自动注入**，漏填即 400。
>
> **范围**：本计划覆盖 Anthropic Messages 与 OpenAI（`openai-chat` / `openai-responses` /
> `openai-codex`）两条链路的适配。**Google 系（Gemini 原生适配器）暂不做**，归入后续里程碑。
>
> **引用约定：下文一律以包名 + 函数名 / 结构体名为准，不写死行号。**
>
> 状态：**已实施（M1–M4）**。M1 config 层、M2 适配器层、M3 models.dev 目录端点、
> M4 前端 UI（目录填充 + 折叠 + reasoning type）均已完成并通过全量测试。M5（文档与回归）
> 完成 `docs/configuration.md` 更新与全量测试。Google 系（Gemini 原生适配器）仍不在范围。

---

## 0. 已确认的调研结论（代码盘点）

| 关注点 | 现状 | 位置 |
|---|---|---|
| `max_tokens` 注入（Anthropic） | **无自动注入**，完全靠 `parameters` 手填，漏填即 400 | `internal/model/anthropic_messages/request.go`（`buildRequestBody` 参数原样透传） |
| `output_limit` 用途 | 仅用于自动压缩阈值计算，**不会进请求体** | `internal/execution/agent_runner.go`（`agentRunnerRuntime.outputLimit`） |
| `reasoning_config` 结构 | 仅 `parameter` + `default` + `levels`，值可为 string/number/bool/object | `internal/config/config.go`（`ReasoningConfig`） |
| `ApplyReasoningLevel` | 把 `levels[selected]` 写进 `parameter` 点分路径 | `internal/config/reasoning.go`（`ApplyReasoningLevel`） |
| Anthropic 默认思考映射 | 现为 `output_config.effort`（adaptive Claude 分支） | `internal/config/reasoning.go`（`DefaultReasoningConfig`） |
| Anthropic 请求体 | 平铺 `request.Parameters`，无 `thinking` 结构化注入 | `internal/model/anthropic_messages/request.go` |
| 保存时 Anthropic thinking 注入 | 仅当 `parameter` 以 `output_config.` 开头时自动加 `thinking: {type: adaptive}` | `internal/execution/provider_settings.go`（`saveProviderSettings`） |
| OpenAI Chat 思考字段 | 平铺 `reasoning_effort`（透传，无处理） | `internal/model/openai_chat/request.go`（`buildRequestBody`） |
| OpenAI Responses 思考字段 | 嵌套 `reasoning: {effort: ...}`；`max_tokens→max_output_tokens` 转换已存在，但**无自动注入** | `internal/model/openai_responses/request.go`（`buildParameters`） |
| OpenAI Responses 最小输出 | `max_output_tokens` 有 16 token 下限（`clampMinimumOutputTokens`） | `internal/model/openai_responses/request.go` |
| UI 模型卡片 | `Reasoning config` 是唯一折叠区，其余字段全平铺 | `web/src/components/ProviderManagerDialog.tsx` |

### models.dev 数据覆盖面（实测）

| 字段 | 覆盖 | 映射 |
|---|---|---|
| `limit.context / input / output` | 6149 / 1195 / 6149 个模型 | `context_window` / `input_limit` / `output_limit` |
| `cost.{input, output, cache_read, cache_write}` | 5739 个模型有 cost | `pricing`（input→cache_miss、cache_read→cache_hit、output→output） |
| `cost.tiers` / `cost.context_over_200k` | 315 / 282 | `long_context_threshold` + `long_context` |
| `modalities.input` | 全量（含 audio/video/pdf 组合） | `input`（**只取 text/image**，否则 `NormalizeModelInput` 报错） |
| `reasoning_options[].effort.values` | 2038 个模型 | `reasoning_config.levels`（effort 型） |
| `reasoning_options[].budget_tokens` | 505 个模型 | `reasoning_config.levels`（budget 型，数值） |
| `reasoning_options[].toggle` | 956 个 | 归入 budget 型或单独开关 |

关键点：
- 匹配键必须是 **模型 ID**，不是 models.dev 的 provider 名（私有 base_url 网关在 models.dev 无对应条目）。
- effort 值里有 `none / null / default`，不在 canonical set（`off/minimal/low/medium/high/xhigh/max`）里，填充时 `none→off`、过滤 `null`。
- models.dev 数据是"官方默认值"，UI 需标注来源并提示"网关可能不同，可修改"。

---

## 1. 设计决策

### D1：`max_tokens` 注入策略（向后兼容优先）
- **优先级**：用户显式 `parameters.max_tokens` / `parameters.max_output_tokens` > 自动注入值。
- **注入值来源**：`output_limit`（若配置）；否则默认常量（Anthropic 用 4096，与现有 examples 一致）。
- **注入时机**：各 provider 适配器的 `buildRequestBody` 内，仅在 body 缺省时补默认值。**不落盘到 session**（不改历史快照）。
- OpenAI Responses 走 `max_output_tokens`（clamp 16），Chat 走 `max_tokens`。

### D2：`reasoning_config` 增加 `type` 字段
```yaml
reasoning_config:
  type: budget_tokens        # 新增：effort（默认/省略）| budget_tokens
  parameter: thinking.budget_tokens   # 或 reasoning.budget_tokens（Responses）
  default: 8192              # 数值
  min: 1024                  # 可选，UI 提示
  max: 128000                # 可选，UI 提示
  levels:                    # 可选：档位名 → 数值
    low: 2048
    medium: 8192
    high: 128000
```
- `type` 省略时保持 `effort` 行为，**完全向后兼容**。
- `effort` 型：`levels` 值可为 string/number/bool/object（现状不变）。
- `budget_tokens` 型：`levels` 值必须是**数字**；`ApplyReasoningLevel` 校验并写入数值。

### D3：各 provider 的思考字段结构
| provider 类型 | 思考字段位置 | `max_tokens` 字段 |
|---|---|---|
| `anthropic-messages` | `thinking: {type: enabled, budget_tokens: N}`（结构化） | `max_tokens` |
| `openai-chat` | 平铺 `reasoning_effort`（effort）/ 平铺数值（budget） | `max_tokens` |
| `openai-responses` / `openai-codex` | 嵌套 `reasoning: {effort: ...}` / `reasoning: {budget_tokens: N}` | `max_output_tokens` |

- Anthropic 的 `thinking.*` 点分路径需在适配器内转成原生嵌套结构（`thinking: {type: enabled, budget_tokens: N}`）：
  - `thinking.type: disabled` → 输出 `{type: disabled}` 或省略。
  - `thinking.budget_tokens: N` → `{type: enabled, budget_tokens: N}`。
- OpenAI Responses 的点分路径 `reasoning.effort` 经 `buildParameters` 平铺后天然形成 `{"reasoning":{"effort":...}}`，**无需改 `ApplyReasoningLevel`**，加测试锁定即可。

### D4：models.dev 目录（后端）
- 目录查询返回模型各项参数，供 UI 自动填充。
- 后端缓存（TTL 如 24h），仅在 Go 进程内发起请求，**浏览器不直连 models.dev**。
- 匹配 = 模型 ID 模糊查询，返回候选列表（`provider(models.dev) / model id`）。

---

## 2. 实现步骤

### 阶段 A：config 层（reasoning schema 扩展）

**A1. `internal/config/reasoning.go`**
- `ReasoningConfig` 增加 `Type string` 字段（`json:"type,omitempty" yaml:"type,omitempty"`）。
- 常量：`ReasoningTypeEffort = "effort"`、`ReasoningTypeBudgetTokens = "budget_tokens"`。
- `ApplyReasoningLevel`：`budget_tokens` 型校验 `levels[selected]` 为数字（int/float64/json.Number），否则报错；写值仍走点分路径。
- `DefaultReasoningConfig`：
  - Anthropic adaptive Claude 分支改为 `Type: budget_tokens`、`Parameter: "thinking.budget_tokens"`、数值档位（如 `{low:2048, medium:8192, high:128000}`）、`Default: "high"`。
  - OpenAI Responses/Codex 分支保持 `Parameter: "reasoning.effort"`（点分路径天然嵌套），标注 `Type: effort`。
  - OpenAI Chat 分支保持 `Parameter: "reasoning_effort"`（平铺），OpenRouter 用 `reasoning.effort`。

**A2. `internal/config/config.go`**
- `validateProvider`：`budget_tokens` 型必须有 `parameter`；`default` 必须指向有效数值档位；所有 `levels` 值为数字。
- `ModelProfile.UnmarshalYAML` 兼容 `type` 字段（随 `ReasoningConfig` 反序列化即可）。

### 阶段 B：Anthropic 适配器（max_tokens 注入 + thinking 映射）

**B1. `internal/model/anthropic_messages/request.go`**
- `buildRequestBody`：
  1. 注入 `max_tokens`：若 body 无 `max_tokens`，用 `request.MaxTokens`（新字段）或默认常量。
  2. `thinking.*` 点分参数转原生嵌套结构（见 D3）。
- 为 `internal/model/model.go` 的 `Request` 增加 `MaxTokens int` 字段。

**B2. `internal/execution/agent_runner.go`**
- `runSessionTurn` 构造 `model.Request` 时，对 Anthropic 若 `r.parameters` 无 `max_tokens`，从 `r.outputLimit` 注入 `MaxTokens`（`outputLimit > 0` 时），否则用默认常量。
- 同理对 OpenAI 三类型注入（Chat 用 `max_tokens`，Responses/Codex 用 `max_output_tokens`）。

### 阶段 C：OpenAI 适配器（max_tokens 自动注入 + 结构确认）

**C1. `internal/model/openai_chat/request.go`**
- `buildRequestBody`：若 body 无 `max_tokens`，从 `request.MaxTokens` 注入。
- 确认 `reasoning_effort` / `reasoning.budget_tokens` 平铺行为，加测试锁定。

**C2. `internal/model/openai_responses/request.go`**
- `buildParameters`：已处理 `max_tokens→max_output_tokens`；补：无 `max_output_tokens` 且无 `max_tokens` 时，从 `request.MaxTokens` 注入（clamp 16）。
- 确认 `reasoning.effort` / `reasoning.budget_tokens` 点分路径在 Responses 里形成嵌套 `{"reasoning":{...}}`，加测试锁定。

### 阶段 D：models.dev 目录端点（后端）

**D1. 新增 `internal/execution/model_catalog.go`**
- `ModelCatalogEntry`：`id`、`name`、`provider`(models.dev)、`context_window`、`input_limit`、`output_limit`、`pricing`、`reasoning_options`、`modalities`。
- `FetchModelCatalog(ctx)`：拉取 `https://models.dev/api.json`，内存缓存（TTL 24h），结构化为索引。
- `SearchModelCatalog(ctx, query)`：模型 ID 模糊匹配，返回候选。

**D2. `internal/webapp/server.go` + `provider_settings.go`**
- 新端点 `GET /api/model-catalog?q={query}`。
- service 方法 `SearchModelCatalog`。

**D3. 依赖与安全**
- 复用现有 HTTP client 风格；缓存避免每次打开 UI 拉全量；返回标注 `source: models.dev`。

### 阶段 E：前端 UI

**E1. `web/src/components/ProviderManagerDialog.tsx`**
- 模型卡片新增"目录搜索"：输入模型名 → `SearchModelCatalog` → 候选列表 → 选中填充：
  - `context_window` / `input_limit` / `output_limit`
  - `pricing`（`cost.input→cache_miss`、`cost.cache_read→cache_hit`、`cost.output→output`；`cost.tiers/context_over_200k→long_context`）
  - `input`（**只取 text/image**，过滤 audio/video/pdf）
  - `reasoning_config`：按 `api_type` 生成 `type` + `levels`（effort 恒等映射 / budget 数值映射，`none→off` 归一化）
- **详细参数折叠**：模型卡片除 `Profile/Model ID/API type` 外字段整体收进 `<details>`，默认折叠，填充后自动展开提示。

**E2. `web/src/components/ProviderManagerDialog.tsx`（Reasoning 区）**
- 增加 `type` 选择（`effort` / `budget_tokens`）：
  - `effort`：现有 JSON 映射 + 默认档位。
  - `budget_tokens`：显示 `min/max` + 档位映射（`档位名 → 数值`），JSON 编辑折叠为高级选项。
- `reasoningLevelOptions` / `providerInput` 适配 `type`，`budget_tokens` 校验数值。

**E3. `web/src/components/SessionModelDialog.tsx`**
- 下拉选项保持显示档位名；`budget_tokens` 型提示"档位映射为数值预算"。

### 阶段 F：类型 / 文档 / 测试

**F1. `web/src/types.ts`**
- `ReasoningConfig` 增加 `type?: 'effort' | 'budget_tokens'`；`SessionModelOption` 可增加 `reasoning_type`。

**F2. `docs/configuration.md`**
- 更新 `reasoning_config` schema（`type`、数值档位、Anthropic thinking 结构、OpenAI 嵌套结构）。

**F3. 测试**
- Go：`internal/config/reasoning_test.go` 增加 `budget_tokens` 用例（数值写入、非法值报错、三 provider 默认参数）。
- Go：`internal/model/anthropic_messages/request_test.go` 增加 `max_tokens` 自动注入 + `thinking` 嵌套映射用例。
- Go：`internal/model/openai_chat/request_test.go` 增加 `max_tokens` 注入 + `reasoning_effort` 平铺用例。
- Go：`internal/model/openai_responses/request_test.go` 增加 `max_output_tokens` 注入 + `reasoning:{effort}` 嵌套 + `budget_tokens` 嵌套用例。
- Go：`internal/execution/` 新增 `model_catalog_test.go`（mock 拉取）。
- Web：`web/src/components/ProviderManagerDialog` 相关测试（目录填充、折叠、budget 校验）。

---

## 3. 里程碑

- **M1**：阶段 A（config 层：`reasoning_config.type` + 三 provider 默认参数）。
- **M2**：阶段 B + C（适配器层：`max_tokens` 自动注入 + thinking 结构映射）。
- **M3**：阶段 D（models.dev 目录端点 + 缓存）。
- **M4**：阶段 E（前端 UI：目录填充 + 折叠 + reasoning type + 按 provider 字段注入）。
- **M5**：阶段 F（文档 + 全量测试 + 回归）。

---

## 4. 风险与注意点

1. **`output_config.effort` → `thinking.budget_tokens` 迁移**：现 `DefaultReasoningConfig` 给 adaptive Claude 返回 `output_config.effort`，且 `saveProviderSettings` 会据此自动注入 `thinking:{type:adaptive}`。改成 `budget_tokens` 后需确认该注入逻辑是否仍触发、是否冲突。**这是本计划最大的行为变更点**，需单独测试。
2. **Anthropic `max_tokens` 上限**：注入值不能超过模型上限，否则 API 拒；由 models.dev 的 `limit.output` 保证，UI 提示覆盖。
3. **`budget_tokens` 的 `default` 语义**：现在 `default` 是"档位名"（如 `high`）。`budget_tokens` 型里需统一定义（建议 `default` 存档位名，数值只在 `levels` 里，避免歧义）。
4. **models.dev 数据质量**：仅 ~500 模型有 `budget_tokens`；`limit.input` 仅 ~1195 模型有。缺字段留空，不强填。
5. **向后兼容**：`type` 省略时行为与现状完全一致；`max_tokens` 仅当未配置时注入，历史 session 快照不受影响。
6. **Google 系**：Gemini 原生适配器（`gemini-generate-content`）不在本计划范围，后续里程碑处理；`budget_tokens` 目前无法覆盖 Gemini 原生 `thinkingConfig.budgetTokens`。

---

## 5. 待拍板的架构点

- **`reasoning_config.type` 是否用 `nested` 标志区分 Responses 嵌套结构，还是依赖现有点分路径的天然嵌套行为。** 倾向后者（改动最小、向后兼容），在 M1 加测试锁定。
- **Anthropic adaptive Claude 默认从 `output_config.effort` 改为 `budget_tokens` 是否接受。** 影响现有 session 的默认思考档位，需确认。