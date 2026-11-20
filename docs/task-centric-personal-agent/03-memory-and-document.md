# Memory & Document 详细设计文档

**版本：** V1.2  
**文档状态：** 模块详细设计。数据模型、状态枚举、事件和 API 以 `04-integration-and-mvp-plan.md` 为准，冲突时以 04 为准。  
**更新日期：** 2026-11-19  
**所属系统：** Task-oriented Personal Agent  
**模块：** Memory Service & Document Service  
**设计目标：** 建立可追溯、可更新、可检索、可控的个人长期记忆与任务文档体系。

---

## 1. 设计目标与架构边界

### 1.1 设计目标

系统需要解决四个问题：

1. 如何从长期对话中识别值得保存的信息？
2. 如何区分用户明确表达的事实、系统推断和临时上下文？
3. 如何记录任务执行过程中的重要决策和方案依据？
4. 如何让任务文档自动更新，同时避免不同 Agent 之间产生内容覆盖和版本冲突？

最终，系统应该具备以下能力：

- 记住用户明确确认的个人信息和偏好。
- 在新任务中合理利用历史经验。
- 保留每项重要决策的来源和理由。
- 自动维护任务计划、进度、总结和产物索引。
- 在任务执行过程中持续更新文档。
- 支持文档历史版本、回滚和差异比较。
- 支持用户查看、修改和删除个人记忆。
- 能够从历史任务中检索相关内容，但不随意混入无关任务上下文。

### 1.2 模块职责

| 模块 | 核心职责 | 主要数据 |
|---|---|---|
| Memory Service | 管理长期可复用的信息 | Memory Item |
| Document Service | 管理任务过程和成果 | Document、Document Version |
| Decision Service | 管理重要决策及其依据 | Decision Record |
| Knowledge Retrieval | 检索历史经验和知识 | Memory、Document、Artifact |
| Context Builder | 将相关信息装配给 Agent | Context Snapshot |

Decision Service 可以作为独立逻辑模块实现，但第一阶段不必单独部署。

### 1.3 总体架构

```text
                   Main Agent
                       |
                       v
                Agent Runtime
                       |
              +--------+--------+
              |                 |
              v                 v
         Memory Service   Document Service
              |                 |
              v                 v
        Memory Extractor    Document Writer
              |                 |
              v                 v
        Memory Repository   Document Repository
              |                 |
              +--------+--------+
                       |
                       v
            SQLite（.task-agent/agent.db）
                       |
                 +-----+------+
                 |            |
                 v            v
          FTS5 / 向量(P5)   Blob Store
                 |            |
                 +-----+------+
                       |
                       v
               Knowledge Retrieval
                       |
                       v
                 Context Builder
```

补充：

- 实例目录中的 SQLite 数据库保存业务元数据和结构化记录。
- Markdown 是任务文档的主要内容格式。
- 实例目录中的 Blob Store（`.task-agent/blobs/`）保存不可变的文档版本和文件产物（04 §14.4）。
- 关键词检索使用 SQLite FTS5 的 trigram 分词器（兼容中文）；语义检索在 Phase 5 启用，向量在 Go 中计算。
- Task Engine 通过事件通知 Document Service 和 Memory Service 更新数据。
- Agent Runtime 不直接修改 Memory 或 Document 的底层数据。

---

## 2. Memory Service 设计

### 2.1 什么是记忆

记忆不是将用户每句话保存下来。

一个有效的 Memory Item 应当是能够在未来任务中重复使用的信息。

例如：

用户说：

> 我比较喜欢直接一点的技术文档，希望有架构图、数据模型和实现步骤。

系统可以提取：

```json
{
  "type": "PREFERENCE",
  "content": "用户偏好结构清晰、直接且包含架构图、数据模型和实现步骤的技术文档",
  "scope": "USER_GLOBAL",
  "source_type": "EXPLICIT",
  "status": "ACTIVE"
}
```

而用户说：

> 这次任务先用 PostgreSQL。

这通常只是当前任务的技术约束，不应自动变成用户长期偏好。

### 2.2 记忆分类

建议分为五大类。

| 分类 | 说明 | 示例 |
|---|---|---|
| PROFILE | 用户基本信息 | 职业、角色、常用语言 |
| PREFERENCE | 用户偏好 | 文档风格、交互偏好 |
| FACT | 用户明确提供的事实 | 用户正在开发的项目 |
| EXPERIENCE | 历史任务经验 | 某种技术方案曾经失败 |
| KNOWLEDGE | 可复用知识 | 经验证的解决方案和操作方法 |

第一阶段不建议对个人记忆做过细分类。可以先采用 `type + scope + tags` 组合表达。

### 2.3 记忆作用域

记忆需要明确所属范围。

| Scope | 说明 | 可见范围 | 阶段 |
|---|---|---|---|
| USER_GLOBAL | 用户长期信息 | 所有任务（按相关性检索） | P3 |
| TASK | 某个任务内的信息 | 当前 Task | P3 |
| PROJECT | 某个长期项目的信息 | 关联该项目的任务 | P5（需先引入 projects 数据模型） |

V1.0 中的 EXECUTION 作用域已取消：执行过程的临时信息属于 Execution 上下文和 Checkpoint，不是记忆。Task 和 Execution 的运行状态本身属于业务数据，也不需要重复保存为 Memory。

例如：

- 用户长期偏好：USER_GLOBAL。
- 当前个人 Agent 项目采用的架构决定：PROJECT（P5 之前记录为对应 Task 的决策）。
- 某个任务中选择 PostgreSQL 的原因：TASK 作用域，或直接记录为该 Task 的 Decision。
- 当前模型调用的中间分析结果：不进入记忆。

### 2.4 记忆来源和可信度

记忆必须保存来源属性。

| Source Type | 说明 | 可信度处理 |
|---|---|---|
| EXPLICIT | 用户明确陈述 | 高 |
| CONFIRMED | 用户明确确认系统提取内容 | 高 |
| INFERRED | 系统根据上下文推断 | 不应视为事实 |
| OBSERVED | 系统从用户行为中观察 | 需要进一步验证 |
| IMPORTED | 从用户导入文件中提取 | 保留原始来源 |

建议将来源可信度与记忆状态分开管理。

例如，EXPLICIT 表示来源是用户明确表达，但其内容仍可能有时间范围或上下文限制，并不意味着永久有效。

### 2.5 记忆状态

状态枚举和转换表以 04 §5.7 为准：

| 状态 | 说明 |
|---|---|
| CANDIDATE | 已提取，等待确认或按策略生效 |
| ACTIVE | 当前有效，参与检索 |
| SUPERSEDED | 被更新版本替代 |
| REJECTED | 被用户明确拒绝 |
| EXPIRED | 已过期 |
| DELETED | 已删除（内容清空，保留墓碑） |

V1.0 中的 CONFIRMED 状态已取消：“是否经用户确认”由 `source_type = CONFIRMED` 和 `confirmed_at` 表达，`status` 只表达生命周期（ADR-SI-027）。

状态转换：

```text
提取
 |
 v
CANDIDATE
 |
 +---- 用户确认 / 策略允许 ---> ACTIVE
 |
 +---- 用户拒绝 -------------> REJECTED（写入抑制规则）
 |
 +---- 自动过期 -------------> EXPIRED

ACTIVE
 |
 +---- 被新记忆替代 ---------> SUPERSEDED
 +---- 过期 -----------------> EXPIRED
 +---- 用户删除 -------------> DELETED（写入抑制规则）
```

直接生效规则：

- `source_type = EXPLICIT`（用户明确表达，且符合 §2.6 的长期记忆标准）：直接 ACTIVE。
- `source_type = INFERRED` / `OBSERVED`：必须保持 CANDIDATE，经用户确认后改为 `source_type = CONFIRMED` 并变为 ACTIVE。

不能仅因为模型多次观察到某种行为，就把推断自动升级为用户明确确认的事实。

### 2.6 什么信息值得长期记忆

建议采用如下规则：

**适合长期记忆：**

- 用户明确表示希望系统以后记住的信息。
- 稳定的输出偏好和交互偏好。
- 用户反复确认的长期工作方式。
- 长期项目的重要背景和约束。
- 用户明确批准保存的可复用经验。

**默认不应长期保存：**

- 一次性请求。
- 临时任务状态。
- 未经确认的个人属性推断。
- 不相关的聊天细节。
- 密码、访问令牌和 API Key。
- 不必要的敏感个人信息。

需要注意：不能把一次性的任务选型当作用户长期偏好。

---

## 3. Memory Extraction：记忆提取机制

### 3.1 提取触发时机

不建议每条用户消息都调用一次独立的记忆提取模型。

可以采用事件驱动和批处理结合的方式。

触发条件：

1. 用户明确表达长期偏好。
2. 用户明确要求记住某件事。
3. 用户修正已有个人信息。
4. 任务完成时发现了可复用经验。
5. 用户明确确认某项长期项目约束。
6. 对话中出现值得保存的稳定信息。

普通聊天不必自动生成长期记忆。

### 3.2 提取流程

```text
User Message / Task Event
          |
          v
      Event Filter
          |
          v
    Memory Extractor
          |
          v
    Candidate Memory
          |
          v
   Normalize & Deduplicate
          |
          v
     Conflict Detection
          |
     +----+-----+
     |          |
     v          v
  No Conflict  Conflict
     |          |
     v          v
  Save Draft  Resolve / Ask
     |          |
     +----+-----+
          |
          v
    Memory Repository
```

### 3.3 提取输出协议

Memory Extractor 不应该直接写数据库。

它只生成候选记录：

```json
{
  "items": [
    {
      "type": "PREFERENCE",
      "scope": "USER_GLOBAL",
      "content": "用户偏好直接、结构化的技术文档",
      "source_type": "EXPLICIT",
      "source_refs": [
        {
          "type": "MESSAGE",
          "id": "message-id"
        }
      ],
      "confidence": 0.98,
      "requires_confirmation": false,
      "reason": "用户明确表达了长期文档偏好"
    }
  ]
}
```

说明：

- `confidence` 是系统提取和匹配的置信度，不是事实真实性的绝对概率。
- `source_refs` 必须引用可追溯的原始来源。
- `requires_confirmation` 由业务策略决定，不能单纯依靠模型自行判断。
- 不允许模型创建不存在的来源 ID。

### 3.4 结构化校验

候选记忆入库前应执行：

1. JSON Schema 校验。
2. 类型和作用域校验。
3. 来源有效性校验。
4. 敏感信息过滤。
5. 重复记忆检查。
6. 冲突检查。
7. 写入权限检查。

无法确认的内容应保留为候选，或直接丢弃，而不是强行创建长期记忆。

### 3.5 去重机制

例如已有记忆：

> 用户偏好中文技术文档。

新提取记忆：

> 用户习惯用中文阅读和撰写技术资料。

两者可能是同一事实。

建议采用三层去重：

- 精确去重：normalized_content 哈希。
- 关键词匹配：类型、主体、标签。
- 语义匹配：Embedding 相似度（Phase 5；之前使用 FTS5 检索结果提示潜在重复）。

语义匹配只能识别潜在重复，不能自动决定两条记忆一定相同。

合并时需要保留两个来源的来源链路。

---

## 4. 记忆冲突解决

### 4.1 冲突示例

历史记忆：

> 用户偏好使用 Python。

当前用户明确表示：

> 以后这个项目统一使用 TypeScript。

不能简单认为两条记忆互相矛盾。

因为：

- 一条可能是全局偏好。
- 一条是特定项目约束。

冲突检查必须同时考虑 scope、subject、时间和适用条件。

### 4.2 优先级

建议遵循：

1. 用户针对当前任务的明确指令。
2. 用户针对项目的已确认决策。
3. 用户最新明确确认的全局偏好。
4. 历史确认信息。
5. 系统推断和行为观察。

这不是简单的全局覆盖顺序。

更准确的做法是先判断作用域是否相同、条件是否重叠，再决定是否构成冲突。

### 4.3 记忆更新策略

不要直接覆盖旧记录。

建议保留历史：

```text
Memory A
状态：SUPERSEDED
内容：用户默认偏好 Python

Memory B
状态：ACTIVE
内容：用户在某项目中要求使用 TypeScript
```

如果新信息明确修正了旧信息，则将旧记忆标记为 SUPERSEDED，并保存替代关系。

如果新信息只是某次任务的临时限制，则不修改全局记忆。

### 4.4 何时向用户确认

以下情况应考虑向用户确认：

- 两条全局偏好存在实质冲突。
- 系统准备把推断提升为长期记忆。
- 某个信息将影响未来大量任务。
- 新信息可能导致重要的旧记忆失效。

以下情况通常不需要打扰用户：

- 当前任务的临时技术选型。
- 可以通过任务作用域解决的差异。
- 对任务结果没有实际影响的信息。

确认入口仍然是 Main Agent。

---

## 5. Memory 数据模型

本模块的表结构以 04 §4.3 为准：

| 表 | 用途 | 阶段 |
|---|---|---|
| memories | 统一记忆表：type、scope、内容、来源类型、状态、有效期、替代关系、乐观锁版本 | P3 |
| memory_sources | 记忆来源链路（消息、任务、文档、决策、交互、导入） | P3 |
| memory_suppressions | 删除和拒绝后的抑制规则（只存内容哈希和来源 ID） | P3 |
| memory_relations | 记忆之间的关系：SUPERSEDES、SUPPORTS、CONTRADICTS、RELATED_TO、DERIVED_FROM | P5 |
| memory_embeddings | 向量，记录 Embedding 模型 | P5 |

设计要点：

- 来源关系单独建表，不放在 JSON 字段中。
- 来源摘录只保存追溯所需的最小内容，避免不必要地复制敏感原文。
- Embedding 模型更换时记录模型标识并全量重建，不混用不同模型的向量。
- 记忆删除时清空内容、删除来源和向量，保留墓碑行以维持抑制规则的引用。

### 5.1 数据库约束

- 已删除记忆不参与检索。
- SUPERSEDED 记忆默认不参与普通上下文装配。
- 用户可见的记忆更新必须写入事件（memory.updated 等）作为审计记录。
- 记忆状态更新采用乐观锁。
- 对 `(scope, scope_id, status)` 建立组合索引；`content` 建立 FTS5（trigram）全文索引，由触发器同步。

## 6. Memory Retrieval：记忆检索

### 6.1 检索目标

记忆系统不是将所有 Memory 全部放入 Prompt。

它必须根据当前任务选择最相关、最可信、最合适作用域的信息。

### 6.2 检索流程

```text
Current Task / User Message
           |
           v
   Build Retrieval Query
           |
           v
   Resolve Scope & Filters
           |
           v
    Keyword Retrieval
           |
           v
    Vector Retrieval
           |
           v
     Merge Candidates
           |
           v
  Relevance & Validity Check
           |
           v
        Rerank
           |
           v
    Return Memory Items
```

### 6.3 检索约束

首先进行硬过滤：

- 状态必须有效。
- 作用域必须允许访问。
- 不允许读取其他用户的数据。
- Task Scope 必须匹配当前 Task。
- 对过期和已删除信息进行过滤。

然后才进行语义匹配。

不能让一个语义相似度很高的其他任务记忆绕过权限和作用域限制。

### 6.4 检索评分

可以采用综合排序：

$$
Score = w_s S_{semantic} + w_k S_{keyword} + w_t S_{type} + w_r S_{recency} + w_c S_{confidence}
$$

其中：

- $S_{semantic}$：语义相似度（Phase 5 之前为 0）。
- $S_{keyword}$：关键词匹配度（FTS5 `bm25()` 得分归一化；短查询走 `LIKE` 时按命中计 1）。
- $S_{type}$：记忆类型相关度。
- $S_{recency}$：时间相关度。
- $S_{confidence}$：来源可信度（EXPLICIT、CONFIRMED 高于 IMPORTED，INFERRED、OBSERVED 不会出现在 ACTIVE 结果中）。

权重需要通过实际检索效果调整。

作用域和权限不是评分项，而是先行过滤条件。

### 6.5 返回结构

```json
{
  "items": [
    {
      "memory_id": "memory-id",
      "type": "PREFERENCE",
      "scope": "USER_GLOBAL",
      "content": "用户偏好结构清晰、直接的技术文档",
      "status": "ACTIVE",
      "source_type": "CONFIRMED",
      "relevance_score": 0.91,
      "source_refs": [
        {
          "type": "MESSAGE",
          "id": "message-id"
        }
      ]
    }
  ]
}
```

Agent 应知道这条信息是用户明确确认的，还是系统推断的。

不能只把记忆内容塞入 Prompt 而丢失其可信度和来源信息。

### 6.6 记忆检索的使用原则

Main Agent：

- 检索用户全局信息。
- 检索当前任务关联项目的信息。
- 可以查询历史任务的摘要和经验。

Task Agent：

- 默认检索当前任务记忆。
- 可以按需读取用户全局偏好。
- 不允许默认访问其他任务的完整记忆。
- 查询历史经验时必须通过显式检索接口。

---

## 7. Decision Memory：决策记录系统

这是系统区别于普通聊天机器人的重要能力。

系统不仅要记住最终选择了什么，还要保留为什么选择。

### 7.1 决策类型

V1.0 的“决策类型”混合了“谁做的决定”和“决策属于哪个领域”。V1.1 拆为两个字段（04 §4.3 task_decisions）：

`decided_by`（谁做的决定）：

| 值 | 说明 |
|---|---|
| USER | 用户明确做出或确认的决定，必须有用户消息或交互作为来源 |
| AGENT | Agent 在授权范围内做出的决定（例如不影响用户使用方式的实现细节） |
| SYSTEM | 系统按既定策略做出的决定 |

`category`（决策领域）：

| 值 | 示例 |
|---|---|
| TECHNICAL | 技术架构选型 |
| DESIGN | 产品或设计方案 |
| TASK | 任务执行方案 |
| POLICY | 权限和行为约束 |
| OTHER | 其他 |

被否决的备选方案记录在决策的 `alternatives` 中，不作为独立决策。

### 7.2 决策结构

```json
{
  "decision_id": "decision-id",
  "task_id": "task-id",
  "code": "DEC-001",
  "title": "采用 PostgreSQL",
  "category": "TECHNICAL",
  "decided_by": "USER",
  "status": "CONFIRMED",
  "context": "需要选择核心业务数据库",
  "decision_content": {
    "decision": "使用 PostgreSQL 作为核心业务数据库"
  },
  "alternatives": [
    {"name": "MySQL", "summary": "可以满足基础关系型存储需求", "rejected_reason": "JSON 能力较弱"},
    {"name": "MongoDB", "summary": "文档型数据存储灵活", "rejected_reason": "事务和关系建模不足"}
  ],
  "rationale": [
    "事务和关系建模能力适合核心业务数据",
    "JSONB 可以承载部分灵活结构"
  ],
  "user_statement": "就用 PostgreSQL 吧",
  "source_type": "INTERACTION",
  "source_id": "interaction-id",
  "supersedes_id": null,
  "created_at": "2026-11-19T00:00:00Z"
}
```

以上是结构示例。实际决策内容必须有真实来源，不能让模型补造不存在的用户选择或依据。

### 7.3 决策状态

- PROPOSED：候选方案。
- PENDING_CONFIRMATION：等待用户确认。
- CONFIRMED：已确认。
- REJECTED：已拒绝。
- SUPERSEDED：已被新决策替代。

转换表见 04 §5.5。

是否需要用户确认：

- 需要：`decided_by = USER` 的决策（必须来自用户）；影响用户使用方式、成本、数据范围或外部操作的决策。
- 不需要：只影响内部实现细节的决策，以 `decided_by = AGENT` 直接 CONFIRMED，并在任务总结中告知用户（参考 01 §10.4 的示例）。

### 7.4 决策生命周期

```text
Agent / User 提出方案
        |
        v
   PROPOSED
        |
        v
   记录备选方案
        |
        v
  是否需要用户确认
     /       \
   是         否
   |          |
   v          v
PENDING    CONFIRMED
   |
   v
Main Agent 提问
   |
   v
User Answer
   |
   +----> CONFIRMED
   |
   +----> REJECTED
```

### 7.5 决策不可简单覆盖

例如：

第一次决策：

> 采用 LangGraph 作为 Agent 编排框架。

后续决策：

> 根据运行时需求调整为 LangGraph + Temporal。

第二项并不一定否定第一项，而是对架构职责做了细化。

因此决策记录必须：

- 保留历史。
- 明确替代关系。
- 支持关联多个决策。
- 保存决策的来源和理由。

不要简单把最新内容写入原记录并删除旧内容。

---

## 8. Document Service 设计

### 8.1 文档系统定位

Document Service 负责管理任务生命周期中的可读工作记录。

它需要解决：

- 一项 Task 有哪些文档？
- 文档如何自动生成？
- 文档何时更新？
- 多个 Agent 如何协作编辑？
- 如何避免覆盖用户修改？
- 如何查询历史版本？
- 如何将最终结果形成可交付产物？

### 8.2 文档与结构化数据的边界

必须明确：

**SQLite 数据库是业务状态的事实来源，Markdown 是任务过程和成果的可读表达。**

例如：

| 数据 | 事实来源 |
|---|---|
| Task 状态 | tasks 表 |
| Task Node 状态 | task_nodes 表 |
| 用户决策 | task_decisions 表 |
| 当前任务进度 | Task Engine 派生数据 |
| 任务总结 | summary.md |
| 方案依据 | decisions.md |
| 最终设计文档 | Task Artifact |
| 执行事件 | events 表 |

`summary.md` 不负责替代 Task 状态字段。

如果两者不一致，应该以数据库中的业务状态为准，并重新生成文档视图。

### 8.3 文档分类

建议建立三类文档。

#### A. 系统级文档

记录用户长期背景、偏好和系统知识。个人长期记忆以数据库中的结构化数据为准；Markdown 视图只在导出时生成（逻辑路径 `memory/profile.md`、`memory/preferences.md`），用于人工阅读，不作为编辑入口。

#### B. Task 工作文档

每个 Task 自动建立以下文档（逻辑路径，对应 `documents.logical_path`；实际存储布局见 04 §14.4）：

```text
tasks/{task_id}/
  task.md
  plan.md
  summary.md
  decisions.md
  progress.md
  sessions/{session_number}.md
  artifacts/...
```

#### C. 用户交付文档

例如：

- 技术架构设计文档。
- 研究报告。
- 项目方案。
- 会议总结。
- 代码文件。
- 数据分析报告。

交付文档应保留独立 Artifact ID 和版本。

---

## 9. Task 文档内容规范

### 9.1 task.md

任务主文件，记录任务的稳定信息。

```markdown
# Task Title

## Goal
任务目标

## Background
任务背景

## Constraints
用户提出的约束

## Scope
本任务包含和不包含的内容

## Success Criteria
任务完成标准

## Created At
创建时间

## Related Decisions
关联决策 ID

## Related Artifacts
关联产物 ID
```

任务状态仍由 Task Engine 维护。

可以在文档中生成状态快照，但不能把它当作权威状态。

### 9.2 plan.md

记录执行计划：

```markdown
# Task Plan

## Objective
任务目标

## Execution Steps

### Step 1
- Description
- Dependencies
- Expected Output
- Status

### Step 2
- Description
- Dependencies
- Expected Output
- Status
```

Plan 应保留版本。

当计划发生重要变化时，不应悄悄覆盖原内容。

### 9.3 summary.md

任务摘要分为：

- 当前摘要。
- 已完成工作。
- 已确认结论。
- 未解决问题。
- 下一步计划。
- 重要产物。

建议采用结构化章节，便于 Context Builder 提取。

### 9.4 decisions.md

记录所有重要决策。`decisions.md` 由 `task_decisions` 表渲染生成（RENDERED），不直接编辑：

```markdown
# Decisions

## DEC-001: 数据库选型

- **Status:** CONFIRMED
- **Category:** TECHNICAL
- **Decided By:** USER
- **Date:** 2026-11-19
- **Supersedes:** None

### Context
需要选择核心业务数据库

### Decision
使用 PostgreSQL

### Rationale
- 支持事务
- 支持 JSONB
- 适合任务关系建模

### Alternatives
- MySQL：JSON 能力较弱
- MongoDB：事务和关系建模不足

### User Statement
> 就用 PostgreSQL 吧

### Impact
后续数据模型使用 PostgreSQL 的 JSONB 和事务能力
```

每条记录都对应数据库中的 Decision ID，修改决策必须通过 Decision Service（新决策替代旧决策），然后重新渲染。

### 9.5 progress.md

记录任务执行进度。

```markdown
# Progress

## Current Status
RUNNING

## Completed
- 需求分析
- 总体架构设计

## In Progress
- Agent Runtime 设计

## Blocked
- None

## Pending
- Task Engine 设计
- Memory & Document 设计
```

progress.md 是数据库状态的可读投影，由 Task、Task Node 和事件数据渲染生成（RENDERED）。

每次关键进度变化后可以更新，但不需要每一次模型调用都写入文档。

### 9.6 sessions 文档

每个 Task Session 记录：

- 本次执行目标。
- 用户新增要求。
- 本次重要决策。
- 已完成操作。
- 重要中间结论。
- 尚未解决问题。

避免将所有原始消息全文复制进去。

原始消息仍然保留在 Conversation Service。

---

## 10. Document 数据模型

本模块的表结构以 04 §4.3 为准：

| 表 | 用途 | 阶段 |
|---|---|---|
| documents | 文档元数据：类型、逻辑路径、写入策略、当前版本（兼作乐观锁）、最后作者类型 | P3 |
| document_versions | 不可变版本：存储位置、内容哈希、变更类型、作者 | P3 |
| document_operations | 变更操作记录：基础版本、操作类型、幂等键、结果 | P3 |
| task_artifacts | 交付产物（V1.0 的 document_artifacts 合并于此） | P3 |

设计要点：

- 版本记录不可变，`(document_id, version)` 唯一。
- 文档内容不存数据库，存 Blob Store；数据库只存元数据和存储引用。
- Markdown 交付物同时是 Document（可版本化编辑）和 Artifact（可下载）：`task_artifacts.document_id` 指向文档。
- PDF、DOCX、XLSX 等二进制文件只作为 Artifact，原文件存 Blob Store。

## 11. 文档写入和版本控制

### 11.1 核心问题

多个 Agent 可能同时更新同一份文档。

例如：

- Task Agent A 更新 summary.md。
- Task Agent B 更新 decisions.md。
- Reviewer Agent C 修改最终方案。

如果没有写入控制，就可能出现：

1. 内容互相覆盖。
2. 旧版本覆盖新版本。
3. 用户修改丢失。
4. 文档与决策数据库不一致。

### 11.2 推荐方案：Document Writer 单写入者

所有文档修改统一经过 Document Service。

```text
Task Agent A ----+
                 |
Task Agent B ----+----> Document Service
                 |           |
Reviewer Agent --+           v
                         Change Queue
                              |
                              v
                        Version Manager
                              |
                              v
                         File Storage
```

Agent 不直接写 Blob Store，也不直接修改数据库里的文档内容。

### 11.3 版本控制

采用乐观并发控制。

修改请求必须携带 `base_version`：

```json
{
  "document_id": "document-id",
  "base_version": 7,
  "operation_type": "PATCH",
  "patch": {
    "section": "Open Issues",
    "content": "新增一个待确认问题"
  }
}
```

如果当前版本仍然是 7：

- 接受修改。
- 生成 version 8。
- 保存内容和变更记录。

如果当前版本已是 8：

- 拒绝基于旧版本直接覆盖。
- 返回冲突。
- 让 Agent 获取最新版本并重新生成补丁。

### 11.4 修改方式

建议支持三种操作：

**APPEND**

在指定章节追加内容，适用于进度、日志和决策记录。

**PATCH**

修改指定章节或结构化区块，适用于摘要和计划更新。

**REPLACE**

替换整份文档，仅允许在明确授权的重写、修订或文件生成流程中使用。

普通 Agent 默认不允许对任务主文档执行无条件的 REPLACE。

### 11.5 不同文档的写入策略

写入策略记录在 `documents.write_policy`，Document Service 据此拒绝不允许的操作：

| 文档 | write_policy | 说明 |
|---|---|---|
| task.md | PATCH | 结构化 PATCH |
| plan.md | PATCH | PATCH + 版本 |
| summary.md | PATCH | 由模型生成结构化 PATCH；用户要求重写时允许 REPLACE |
| decisions.md | RENDERED | 由 task_decisions 渲染 |
| progress.md | RENDERED | 由 Task、Task Node 和事件渲染 |
| sessions/*.md | APPEND_ONLY | 只追加 |
| 交付文档 | REPLACE_ALLOWED | 每次生成新版本，不覆盖旧版本 |

RENDERED 文档只能由系统渲染器写入（`change_type = RENDER`），Agent 和用户都不能直接修改。

### 11.6 用户编辑优先级

用户直接修改文档之后，系统不能自动用之前的 Agent 版本覆盖。

建议：

- 保存用户修改为新版本。
- 将修改标记为 USER_EDITED。
- 后续 Agent 读取最新版本。
- Agent 提交修改时必须基于最新版本。
- 用户明确要求重新生成时，才允许覆盖指定范围。
- 用户在界面中编辑（`DocumentService.UserEdit`，04 §6.7），不直接修改 `.task-agent/` 目录（04 §3.8）。
- 文档最新版本的作者是用户时，Agent 基于更早版本提交的修改一律返回版本冲突。

用户编辑的内容不能只被当作普通模型生成文本。

---

## 12. 文档自动生成机制

### 12.1 生成触发点

不需要每个 Agent Step 都生成一版完整文档。

建议在以下事件触发更新：

| 事件 | 更新内容 |
|---|---|
| task.created | 创建 task.md |
| task.planned | 创建或更新 plan.md |
| decision.confirmed / decision.superseded | 重新渲染 decisions.md |
| task_node.completed | 重新渲染 progress.md |
| task.waiting_user | 更新 summary.md 和 progress.md |
| artifact.created | 更新 Artifact 索引 |
| task.paused | 更新摘要和当前状态快照 |
| task.resumed | 重新渲染 progress.md |
| task.completed | 生成最终 summary.md |
| task.failed | 记录失败原因和可恢复状态 |

### 12.2 文档生成流程

```text
Task Event
    |
    v
Document Event Consumer
    |
    v
Determine Target Documents
    |
    v
Load Latest Document Version
    |
    v
Build Update Context
    |
    v
Generate Structured Patch
    |
    v
Validate Patch
    |
    v
Check Base Version
    |
    v
Commit New Version
    |
    v
Publish document.updated
```

其中：

- 任务状态变化由 Task Engine 产生。
- 文档更新由 Document Service 执行。
- Agent 可以提出修改建议，但不能直接提交底层存储。

### 12.3 文档生成协议

```json
{
  "document_id": "document-id",
  "base_version": 5,
  "operation_type": "PATCH",
  "change_summary": "更新任务阶段总结",
  "changes": [
    {
      "section": "Completed Work",
      "operation": "APPEND",
      "content": "已完成任务状态机设计"
    },
    {
      "section": "Next Steps",
      "operation": "REPLACE",
      "content": "进入 Memory & Document 详细设计"
    }
  ]
}
```

输出经过校验后才进入写入流程。

### 12.4 自动摘要要求

摘要至少应包含：

- 任务目标。
- 已完成内容。
- 已确认的关键决策。
- 仍然有效的限制条件。
- 当前阻塞。
- 下一步计划。

摘要不能把 Agent 的猜测包装成已确认事实。

### 12.5 幂等性

同一个任务事件可能被重复投递。

因此：

- 每个文档更新必须有 event_id。
- 同一个 event_id 只能成功处理一次。
- 相同输入应得到相同业务结果。
- 文档写入应保存 operation_id。
- 重复事件不应生成重复的决策和产物条目。

---

## 13. Knowledge Base：历史任务知识检索

### 13.1 为什么需要知识库

长期记忆不适合保存所有任务内容。

例如用户完成一个大型项目后，系统不应该把项目里的每段分析都升级为用户长期记忆。

更合适的方式是：

- 用户偏好进入 Memory。
- 项目执行过程进入 Task Document。
- 可复用经验进入 Knowledge Base。
- 当前任务信息进入 Task Context。

### 13.2 知识来源

可以从以下内容构建知识库：

- 已完成任务的 summary.md。
- 已确认的技术决策。
- 用户批准保存的经验。
- 经过审核的最终产物。
- 用户导入的知识文件。

默认不应将全部原始聊天记录直接转为知识库。

### 13.3 知识条目结构

```json
{
  "knowledge_id": "knowledge-id",
  "title": "长任务恢复设计经验",
  "content": "使用持久化 Workflow 保存等待状态...",
  "category": "ENGINEERING",
  "source_task_id": "task-id",
  "source_artifact_id": "artifact-id",
  "status": "ACTIVE",
  "review_status": "APPROVED"
}
```

知识库在 Phase 5 实现，表结构见 04 §4.3 `knowledge_items`。

### 13.4 入库策略

建议采用：

```text
Task Completed
     |
     v
Knowledge Candidate Extraction
     |
     v
Deduplicate
     |
     v
Check Reusability
     |
     v
Review / Approval
     |
     v
Knowledge Base
```

对于自动提取的知识，可以先进入候选区。

不需要每条知识都向用户询问，但影响长期技术方向、个人偏好或重要事实的内容应有明确确认机制。

### 13.5 检索优先级

当前任务上下文优先于历史知识：

1. 当前用户明确指令。
2. 当前 Task 约束和已确认决策。
3. 当前 Task Document。
4. 用户长期记忆。
5. 相关历史 Task。
6. 通用 Knowledge Base。

历史知识只能帮助完成当前任务，不能悄悄覆盖当前明确要求。

---

## 14. Context Builder 集成

Memory 和 Document 最终都需要进入 Agent Runtime 的上下文。

但不能简单把两个系统的全部数据拼接起来。

### 14.1 上下文装配流程

```text
Execution Request
       |
       v
Load Task Context
       |
       v
Retrieve User Memory
       |
       v
Retrieve Project Memory
       |
       v
Retrieve Related Decisions
       |
       v
Retrieve Relevant Documents
       |
       v
Apply Scope & Permission Filters
       |
       v
Apply Token Budget
       |
       v
Build Agent Context
```

### 14.2 Agent 上下文建议

Main Agent：

- 用户长期偏好。
- 当前对话。
- 当前活动 Task 摘要。
- 必要的历史任务摘要。
- 待处理交互信息。

Task Agent：

- 当前 Task 目标。
- 当前 Task 的计划和摘要。
- 当前 Task 的确认决策。
- 当前 Task 的历史执行记录。
- 相关用户长期偏好。
- 按需检索的历史知识。

### 14.3 上下文引用

注入 Agent 的信息应附有来源 ID。

例如：

```text
[USER MEMORY: memory-001]
用户偏好结构清晰、直接的技术设计文档。

[TASK DECISION: decision-012]
采用 Main Agent + Task Agent 架构。

[TASK DOCUMENT: document-003, version 8]
当前任务摘要。
```

这样可以支持：

- Agent 解释结论来源。
- 对错误记忆进行修正。
- 对决策进行追溯。
- 避免把生成内容误认为用户输入。

### 14.4 Token 预算建议

Token 预算的分配比例以 04 §10.5 为准。

记忆检索应优先考虑相关性，而不是尽可能多地装入内容。

## 15. API 设计

本模块不提供 HTTP 接口（ADR-SI-024）。V1.0 中的接口改为进程内 Go 接口：

| V1.0 接口 | 现在的接口 |
|---|---|
| POST /internal/memories/candidates | `MemoryService.SaveCandidate`（04 §6.5） |
| POST /internal/memories/search | `MemoryService.Search`；Agent 通过内置工具 `retrieve_context` 调用 |
| POST /internal/decisions | `DecisionService.Propose`（04 §6.6）；Agent 通过 `propose_decision` 调用 |
| POST /internal/decisions/{id}/supersede | `DecisionService.Supersede` |
| POST /internal/documents | `DocumentService.Create`（04 §6.7） |
| POST /internal/documents/{id}/operations | `DocumentService.Apply`；Agent 通过 `submit_document_update` 调用 |
| 记忆、决策、文档的对外接口 | 界面直接调用上述服务，操作清单见 04 §12.2 |

记忆检索参数（`MemoryQuery`）示例：

```go
MemoryQuery{
    Text:   "用户的技术文档偏好",
    Scopes: []MemoryScope{ScopeUserGlobal, ScopeTask},
    TaskID: taskID,
    Limit:  10,
}
```

## 16. 隐私、安全和数据治理

### 16.1 用户控制权

个人 AI 助手必须允许用户查看和管理长期记忆。

建议提供：

- 查看全部记忆。
- 搜索记忆。
- 修改记忆。
- 删除记忆。
- 查看记忆来源。
- 导出记忆。
- 清空特定作用域的记忆。

用户明确提出删除的信息，不应因为后续任务总结又被自动重新写入。

### 16.2 敏感信息策略

默认不把以下内容作为普通长期记忆：

- 密码。
- API Key。
- 访问令牌。
- 私钥。
- 银行账户及支付凭据。
- 无关的高度敏感个人信息。

用户导入的文件和第三方工具返回内容也必须经过数据分类和安全检查。

### 16.3 权限控制

建议采用：

```text
Instance（实例目录）
    |
    +-- User Memory
    |
    +-- Project Memory
    |
    +-- Task Documents
    |
    +-- Task Artifacts
```

所有检索和读取请求都必须先校验 Scope 和授权关系（例如 Task Agent 只能读取自己 Task 的作用域），再执行搜索。不同实例的数据在不同的数据库文件中，天然隔离。

### 16.4 删除机制

删除 Memory 时需要处理：

1. Memory 主记录。
2. Memory Source。
3. Memory Embedding。
4. Memory Relation。
5. 检索缓存。
6. 可能存在的派生索引。

对于文档，删除策略应区分：

- 当前版本是否隐藏。
- 历史版本是否保留。
- 用户是否要求永久删除。
- 是否存在备份和保留策略。

不能只删除数据库的一条记录，却继续在向量索引中检索到相同内容。

### 16.5 数据保留

建议区分：

| 数据 | 建议保留策略 |
|---|---|
| 用户确认的长期记忆 | 持续保存，直至用户修改或删除 |
| 临时候选记忆 | 设置过期时间 |
| Task 文档 | 任务保留周期内保存 |
| 执行日志 | 可配置保留周期 |
| 重要决策 | 随任务长期保存 |
| 外部工具临时结果 | 按必要性和任务策略保存 |

长期记忆和原始执行日志不能采用相同的保留策略。

---

## 17. 技术实现建议

### 17.1 技术选型

| 领域 | 方案 | 说明 |
|---|---|---|
| 结构化存储 | SQLite | Memory、Decision、Document 元数据 |
| 关键词检索 | SQLite FTS5（trigram 分词） | 不依赖中文分词；短查询退化为 LIKE |
| 语义检索 | Go 内存中计算余弦相似度（Phase 5） | 个人数据量足够，不引入扩展 |
| 文档格式 | Markdown | 可读、可编辑、便于版本管理 |
| 文件存储 | 实例目录内的 Blob Store | 不可变的文档版本和产物 |
| 文档差异 | Go 实现的 Myers diff（unified diff） | 历史版本比较 |
| 事件传递 | `events` 表 + 持久化消费者 | 可靠更新文档和记忆（04 §7.3） |
| 后台处理 | 持久化消费者 goroutine | 提取、渲染、导出和重建 |

### 17.2 是否使用 Git

建议分阶段处理。

**MVP 阶段：**

- SQLite 管理版本元数据。
- Blob Store 保存不可变版本内容。
- Document Service 计算内容哈希和版本差异。

**后续阶段：**

当用户需要频繁直接编辑 Markdown，或希望完整导出工作区时，可以增加 Git 仓库作为可选的同步和版本浏览机制。

不建议第一阶段同时维护 SQLite、Blob Store 和 Git 三套互相竞争的文档事实来源。

### 17.3 是否使用向量数据库

不使用。Phase 5 的语义检索把向量以 BLOB 存在 `memory_embeddings` 表中，启动时（或首次检索时）加载到内存，在 Go 中计算余弦相似度。个人数据量（千到万级条目）下性能足够，也不需要 SQLite 扩展（纯 Go 驱动无法加载 C 扩展）。

只有当数据量达到实际瓶颈时，再考虑近似最近邻索引。

## 18. 关键时序流程

### 18.1 用户表达长期偏好

```text
User
 |
 v
Main Agent
 |
 v
Conversation Stored
 |
 v
Memory Extractor
 |
 v
Candidate Created
 |
 v
Source & Scope Validation
 |
 v
Memory Repository
 |
 v
Main Agent 可在合适时机确认
 |
 v
Memory ACTIVE
```

### 18.2 任务决策记录

```text
Task Agent
 |
 v
Decision Proposal
 |
 v
Decision Service
 |
 v
Persist Decision
 |
 v
Main Agent 判断是否需要用户参与
 |
 +---- Need Confirmation
 |        |
 |        v
 |    User Answer
 |        |
 |        v
 |    Confirm Decision
 |
 +---- No Confirmation
          |
          v
     Apply Policy
 |
 v
Update decisions.md
 |
 v
Refresh Task Context
```

### 18.3 任务文档自动更新

```text
Task Engine Event（events 表）
 |
 v
事件总线通知
 |
 v
Document Event Consumer（按 consumer_offsets 续读）
 |
 v
Load Current Version
 |
 v
Generate Structured Change
 |
 v
Version Check
 |
 v
Commit Document Version
 |
 v
Publish document.updated
 |
 v
Context Cache Invalidation
```

### 18.4 历史知识应用到新任务

```text
New Task
   |
   v
Main Agent
   |
   v
Build Retrieval Query
   |
   v
Search Memory
   |
   v
Search Historical Tasks
   |
   v
Search Knowledge Base
   |
   v
Filter by Scope and Relevance
   |
   v
Pass Relevant Context to Task Agent
```

---

## 19. 异常处理和一致性

### 19.1 Memory 异常

| 问题 | 处理 |
|---|---|
| 重复提取 | 使用来源 ID 和幂等键 |
| 提取内容格式错误 | Schema 校验失败，不写入 |
| 与已有记忆冲突 | 标记冲突，进入处理流程 |
| Embedding 失败 | 保留结构化记录，稍后重建 |
| 用户删除后再次提取 | 检查删除记录及抑制规则 |
| 来源不存在 | 拒绝写入 |

用户删除后的抑制规则尤其重要。

如果系统仍然从相同原始消息中反复提取同一记忆，就会造成“删了又出现”的问题。

因此需要记录用户明确拒绝或删除某类信息的规则，并在候选提取时执行检查。

### 19.2 Document 异常

| 问题 | 处理 |
|---|---|
| 两个 Agent 同时编辑 | 乐观锁冲突 |
| 版本不匹配 | 重新加载并重新生成 Patch |
| 磁盘写入失败（磁盘满、权限） | 操作记为 FAILED，消费者不推进 `last_seq`，稍后重试，并在界面提示 |
| 重复事件 | 幂等处理 |
| 文档与 Task 状态不一致 | 以数据库为准，重新生成投影 |
| Agent 生成错误 Markdown | 结构校验和内容校验 |
| 用户修改后 Agent 覆盖 | 拒绝旧版本写入 |

### 19.3 一致性要求

Memory、Document 和 Task Engine 不需要全部放在一个数据库事务中。

建议采用：

- 核心业务数据事务提交。
- 事件与业务数据同事务写入 `events` 表，持久化消费者按 `last_seq` 续读（04 §7.3）。
- 下游模块异步更新。
- 必要时进行幂等重试。
- 定期检查投影和索引一致性。

例如 Task COMPLETED 已成功提交，但 summary.md 尚未更新时，Task 仍然是 COMPLETED；Document Service 应根据未处理事件完成文档更新。

---

## 20. 测试方案

### 20.1 Memory 测试

- 用户明确要求记住的信息能够保存。
- 临时任务内容不会自动成为长期偏好。
- 系统推断不会被错误标记为用户确认。
- 相同记忆不会无限重复创建。
- 冲突记忆能够保留历史。
- 用户删除后不会通过普通流程重新出现。
- Task Scope 的信息不会无条件进入其他任务上下文。
- 检索结果能够返回来源。

### 20.2 Decision 测试

- 每个重要决策都有关联来源。
- 候选决策不能自动成为用户已确认决策。
- 用户回答准确更新对应决策。
- 新决策能够替代旧决策但不删除历史。
- 决策能够被文档和产物引用（跨任务关联在 Phase 5 引入）。

### 20.3 Document 测试

- 新 Task 能自动生成任务文档。
- 关键任务事件可以更新文档。
- 文档更新能够生成新版本。
- 并发编辑不会丢失内容。
- 用户编辑内容不会被 Agent 覆盖。
- 历史版本能够查看和恢复。
- 重复事件不会重复写入。
- 文档存储失败后能够恢复更新。

### 20.4 Context 测试

- Main Agent 能够读取适用的用户偏好。
- Task Agent 默认无法读取其他 Task 的完整上下文。
- 当前用户明确指令优先于历史偏好。
- 文档和记忆引用能够追溯来源。
- 上下文超过预算后压缩不会丢失关键决策。

### 20.5 核心验收标准

| 能力 | 验收标准 |
|---|---|
| 记忆提取 | 能区分明确表达和推断 |
| 记忆管理 | 可查询、修改、删除和导出 |
| 来源追溯 | 每条有效记忆有来源链路 |
| 决策管理 | 重要决策有选择依据 |
| 自动文档 | 关键任务事件可触发文档更新 |
| 版本控制 | 所有修改可追溯 |
| 并发安全 | 不丢失用户编辑和 Agent 更新 |
| 上下文检索 | 不发生未经授权的跨任务读取 |
| 数据一致性 | 结构化数据为业务事实来源 |

---

## 21. 实施路线

本模块的实施任务已合并到统一任务清单 `05-implementation-backlog.md`：

| 原阶段 | 对应任务 |
|---|---|
| 基础记忆和任务文档 | M3-01 Blob Store、M3-02 Document Service、M3-03 任务文档、M3-05 Decision Service、M3-06 Memory 数据模型与服务 |
| 自动提取和自动更新 | M3-03 事件驱动更新、M3-07 Memory 提取 |
| 知识检索 | M3-08 记忆检索（FTS5）、M5-01 语义检索、M5-03 Knowledge Base |
| 完善管理能力 | M3-09 界面、M3-10 导出与备份、M5-02 记忆冲突处理 |

## 22. 架构决策记录

| 编号 | 决策 | 原因 |
|---|---|---|
| ADR-MD-001 | Memory 与 Document 分离 | 长期信息与任务记录用途不同 |
| ADR-MD-002 | 记忆采用结构化记录 | 支持查询、更新和治理 |
| ADR-MD-003 | 用 source_type 区分 Explicit、Inferred 和 Confirmed，status 只表达生命周期 | 避免推断被当成事实 |
| ADR-MD-004 | 记忆使用多层 Scope | 防止跨任务污染 |
| ADR-MD-005 | 决策记录独立管理 | 保留选择依据和历史 |
| ADR-MD-006 | Markdown 作为主要任务文档格式 | 可读、可编辑、可导出 |
| ADR-MD-007 | SQLite 数据库为业务事实来源 | 避免文档状态与数据库冲突 |
| ADR-MD-008 | Document Service 统一写入 | 避免多 Agent 并发覆盖 |
| ADR-MD-009 | 文档采用不可变版本 | 支持追溯和回滚 |
| ADR-MD-010 | 使用事件日志 + 持久化消费者更新文档和记忆 | 保证事件处理可靠性 |
| ADR-MD-011 | 初期使用 FTS5 trigram 关键词检索，Phase 5 在 Go 中做向量检索 | 无外部依赖，兼容中文 |
| ADR-MD-012 | 用户拥有记忆查看和删除能力 | 保障个人数据控制权 |

---

## 23. 最终架构结论

Memory & Document 是个人 AI 助手形成长期连续性的基础。

两个模块必须保持清晰边界：

**Memory Service 负责回答：**

- 用户有哪些长期偏好？
- 哪些信息已经确认？
- 某条信息来自哪里？
- 当前哪些记忆仍然有效？
- 哪些历史经验值得在新任务中复用？

**Document Service 负责回答：**

- 一个任务经历了什么？
- 为什么选择当前方案？
- 哪些工作已经完成？
- 产生了哪些文档和文件？
- 某份文档是如何逐步演变的？

**Decision Service 负责回答：**

- 选择了什么？
- 为什么这样选择？
- 当时有哪些备选方案？
- 谁做出了决定？
- 后来是否被新的决定替代？

最终保持以下原则：

1. 对话保存交互历史。
2. Task 保存业务生命周期。
3. Memory 保存长期可复用信息。
4. Decision 保存关键选择及依据。
5. Document 保存任务过程和成果。
6. Artifact 保存可交付文件。
7. Context Builder 负责按需组合这些信息。

**系统不应该追求记住所有内容，而应该准确保留未来完成任务所需要的信息，并且能够解释这些信息从哪里来。**