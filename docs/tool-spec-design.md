# 工具定义：从硬编码到节点上报

> 状态：设计（阶段 1：**只上报，不远程执行**）
>
> 上游：`docs/brain-tool-split.md`

---

## 1. 结论先说

用户提的做法是对的，而且**改造成本比预期低**：

> 执行端注册时就说明自己的 schema 列表

因为 `ToolDef` 里除了 `Run` 函数，其余字段都已经可序列化——`Parameters` 本身就是 JSON：

```go
type ToolDef struct {
    Name        string
    Description string
    Guidance    string
    Parameters  any                                  // ← 已经是 JSON Schema
    Run         func(ctx, args, tc) (ToolReturn, error)  // ← 唯一的不可传输项
}
```

所以要做的事只有一件：**把 `ToolDef` 拆成「可传输的描述」和「本地的执行」两半。**

---

## 2. 数据结构

### 2.1 拆分

```go
// ToolSpec 是可传输的工具描述：注册时上报、模型看到的就是它。
type ToolSpec struct {
    Name        string          `json:"name"`
    Description string          `json:"description"`
    // Guidance 是面向模型的使用指导（什么时候该用、别用 shell 绕过去）。
    // **一起传**：它是工具行为的一部分，远端工具不给这段，模型就会缺约束。
    Guidance   string          `json:"guidance,omitempty"`
    Parameters json.RawMessage `json:"parameters"`
}

// ToolDef 是本地实现 = 描述 + 执行函数。
type ToolDef struct {
    ToolSpec
    Run func(ctx context.Context, args Args, tc *ToolContext) (ToolReturn, error) `json:"-"`
}
```

内嵌 `ToolSpec` 而不是复制字段：现有 8 个工具定义只需要把字段位置上移一层，`Builtin()`、`Guidance()`、`Find()` 的用法基本不变（它们只用 Name/Guidance）。

### 2.2 上报时带上工作区

```go
type Executor struct {
    HostID string `json:"host_id"`
    Name   string `json:"name,omitempty"`
    Token  string `json:"token,omitempty"`
    // 新增：
    Tools     []tools.ToolSpec `json:"tools,omitempty"`
    Workspace string           `json:"workspace,omitempty"`
}
```

**工作区放在 `ToolSpec` 外**（你未选的那一项，我按推荐做法定了）：

- 它是**节点级**属性，不是每个工具各自的
- schema 保持通用（"relative to the session working directory"），不因节点而变
- 中枢把它拼进 system prompt，模型才知道"这台机器的目录是什么"

理由：如果动态改写 schema 把真实路径塞进去，每个节点的 schema 都不一样，缓存、diff、工具列表对比都会变复杂，收益却不明显。

### 2.3 命名空间

多个工具节点时，暴露给模型的名字带前缀：

```
read              ← 只有一个工具节点时（保持现状，不破坏现有提示词）
builder-pc/read   ← 有多个节点时
```

**为什么不一上来就加前缀**：单节点场景占绝大多数，加了前缀会让 system prompt 变长、模型更容易写错名字。只在真正需要区分时才加。

> 待验证：模型对 `node/read` 这种带斜杠的工具名处理是否可靠。备选写法 `node__read`（双下划线）。这一点等实现到多节点时再实测。

---

## 3. 协议改动

复用现有的 `relay.register`，加两个字段（与 `catalog.models` 完全对称）：

```
executor → relay:  relay.register {host_id, name, token, tools: [...], workspace}
relay → viewer:    relay.executors [{host_id, name, tools, workspace}]
```

**为什么放注册帧而不是新消息**：
- 工具列表在进程生命周期内稳定（`Builtin(allowShell)` 是启动期确定的）
- 与 `catalog.models` 的做法一致，不用引入新协议

> 注意：`AllowShell` 是启动期开关。如果将来要做"运行时切换 allow_shell"，才需要独立的刷新接口。现在不需要。

---

## 4. 阶段 1 的范围（只上报，不执行）

这一阶段**不改变任何现有行为**，只是把已有信息暴露出来：

| 改动 | 说明 |
| --- | --- |
| `ToolDef` 拆出 `ToolSpec` | 机械改动，8 个工具定义各挪一层 |
| `relay.register` 带 `tools` + `workspace` | 协议加字段，向后兼容（老执行端不发就是空） |
| 展示端能查到工具清单 | `mobile/bridge` / uiapp 各加一个只读接口 |
| 界面显示 | 能看到"这台机器提供哪些工具、工作区在哪" |

**不做**：远程执行 `RunTools`、多节点组合、跨节点续传。

### 为什么先做这一步

1. 风险接近零：不碰执行路径，现有测试全绿
2. 立刻能验证协议设计：schema 传输是否有问题（体积、特殊字符、`any` 序列化）
3. 能看到实际形态：界面上列出工具，你就知道这个信息呈现方式对不对

---

## 5. 后续阶段（预告）

| 阶段 | 关键设计点 |
| --- | --- |
| 2. 远程执行 | `RunTools` 走 wire：`tool.call` / `tool.result`；按前缀路由到节点 |
| 3. 多节点组合 | 中枢组装工具列表（加前缀）、system prompt 拼装、失败语义 |
| 4. 网状 relay | 中枢连多个 relay；relay 本身不感知角色 |

---

## 6. 需要留意的两个实现细节

### 6.1 `Parameters` 的序列化往返

现在 `Parameters: schema(map[string]any{...})` —— 是 Go 的 `map[string]any`。
改成 `json.RawMessage` 后，构造处要改成先 Marshal 再存：

```go
raw, _ := json.Marshal(schema(map[string]any{...}, "file_path"))
Parameters: raw
```

好处是：**只有序列化，没有反序列化**。远端拿到的是原样的 JSON，直接喂给模型，不需要还原成 Go 结构——这避免了一整类 round-trip 失真问题。

### 6.2 `Guidance` 的拼装位置

现在 `tools.Guidance(list []*ToolDef)` 遍历拼成 system prompt 的一段。远端工具的 guidance 要能进同一个段落，所以这个函数要能接受 `[]ToolSpec`（或统一成只看 `ToolSpec` 字段）。

因为 `ToolDef` 内嵌了 `ToolSpec`，把函数签名改成接受接口或 `[]ToolSpec` 都不难。

---

## 7. 待确认

1. 界面呈现：工具清单是放在"执行端信息"里，还是单独一个"能力"面板？
2. 单节点不前缀、多节点才前缀——这个取舍是否接受？