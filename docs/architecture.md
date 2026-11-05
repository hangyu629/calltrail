# 架构

calltrail 把 Go 代码里的调用关系**机械地**提取出来，整理成三层产出，再用一个自包含的网页浏览。下面的图都是 [Mermaid](https://mermaid.js.org/)，GitHub 上直接渲染。

目录

1. [设计原则](#设计原则)
2. [整体：三层产出](#整体三层产出)
3. [第 1 层：分析器怎么得到 raw.json](#第-1-层分析器怎么得到-rawjson)
4. [数据模型：raw 与 model](#数据模型raw-与-model)
5. [第 2 层：加工层做什么](#第-2-层加工层做什么)
6. [第 3 层：前端怎么画](#第-3-层前端怎么画)
7. [一次 `calltrail run` 发生了什么](#一次-calltrail-run-发生了什么)
8. [验证工具](#验证工具)
9. [包结构](#包结构)

## 设计原则

1. **结构由程序提取，不由模型推断。** 节点和边只来自编译器级的分析（`go/packages` + SSA + 调用图算法）。
2. **每个断言都有证据。** 每条边带调用点（文件:行号）；不确定的边带标记，而不是悄悄省略或充数。
3. **事实与取舍分层。** 原始层只放事实；“哪些是噪音、哪些先藏起来、用户想忽略什么”是第 2 层的事；怎么画是第 3 层的事。
4. **确定性。** 同一份代码、同一组参数，输出完全一致（稳定 ID、稳定排序、固定的布局随机种子）。
5. **默认少而精。** 不画全图：先给包级总览，或从入口出发聚焦；细节按需展开。
6. **不确定就标出来。** 接口分发、反射、`go:linkname` 等静态分析的盲区在图上可见，并用运行时验证量化过。

## 整体：三层产出

```mermaid
flowchart LR
    SRC["Go 源码<br/>(一个 module)"]
    ENTRY["-entry<br/>文件 / 包 / 函数"]
    RULES["rules.json<br/>忽略规则"]

    subgraph L1["第 1 层：分析器 internal/analyze"]
        direction TB
        A1["go/packages 加载<br/>SSA 构建<br/>调用图 CHA / VTA"]
        A2["只留自有代码<br/>外部调用归成依赖节点"]
        A1 --> A2
    end

    subgraph L2["第 2 层：加工层 internal/model"]
        direction TB
        M1["标注：模块 / internal / 标签<br/>扇入 / 可达性"]
        M2["内置隐藏策略<br/>带原因"]
        M1 --> M2
    end

    subgraph L3["第 3 层：视图 internal/render"]
        direction TB
        V1["自研前端<br/>ELK 布局 + SVG"]
    end

    RAW[("raw.json<br/>事实")]
    MODEL[("model.json<br/>事实 + 标注 + 规则")]
    HTML[("viewer.html<br/>自包含页面")]
    ARCH["Archify 导出<br/>（计划中）"]

    SRC --> A1
    ENTRY -.-> A1
    A2 --> RAW
    RAW --> M1
    RULES -.-> M1
    M2 --> MODEL
    MODEL --> V1
    V1 --> HTML
    MODEL -.-> ARCH
```

| 层 | 文件 | 实现 | 只做什么 |
|---|---|---|---|
| 1 | `raw.json` | [internal/analyze](../internal/analyze)、[internal/graph](../internal/graph) | 记录代码里**有什么**：包、函数、外部依赖、边（含调用点）、role。不丢外部依赖，不含展示策略 |
| 2 | `model.json` | [internal/model](../internal/model) | 在原始层上**只追加字段**：模块、是否 internal、扇入、可达性、默认隐藏及原因；携带用户规则。不改不删事实 |
| 3 | `viewer.html` | [internal/render](../internal/render) | 把 model 变成一个浏览用的页面。每个视图都是 model 的只读转换 |

三层之间只有 JSON 文件相连，所以分析（最慢）可以只跑一次，换规则、换视图只重跑后面的层。

## 第 1 层：分析器怎么得到 raw.json

```mermaid
flowchart TD
    S0["Options：目录 / 包模式 / tags / 算法 / entry"]
    S1["go/packages 加载<br/>类型信息 + 依赖 + module"]
    S2["识别生成文件<br/>（模块内所有包）"]
    S3["构建 SSA<br/>泛型实例化"]
    S4{"算法"}
    S5a["static"]
    S5b["CHA"]
    S5c["VTA<br/>以 CHA 为初始图"]
    S6["去掉合成节点"]
    S7["自有函数集合<br/>闭包并入外层 / 泛型实例折叠成原型<br/>init 合并成 pkg.init / 生成代码剔除"]
    S8["遍历每个函数的每个调用点"]
    S9["边的分类（见下图）"]
    S10["汇总<br/>标记按“所有调用点都满足”才成立<br/>隐式边限量"]
    S11["role 计算 / init 节点 / 外部依赖按 module 合并"]
    S12["入口解析 → meta.entries"]
    S13[("raw.json")]

    S0 --> S1 --> S2 --> S3 --> S4
    S4 --> S5a
    S4 --> S5b
    S4 --> S5c
    S5a --> S6
    S5b --> S6
    S5c --> S6
    S6 --> S7 --> S8 --> S9 --> S10 --> S11 --> S12 --> S13
```

### 一个调用点会变成哪些边

```mermaid
flowchart TD
    SITE["一个调用点"]
    D1{"静态可确定的被调函数？"}
    OWN["自有 → 自有的边"]
    DIRECT["direct"]
    DYN["dynamic<br/>带候选数 + 接口名"]
    CB["callback<br/>被调的是定义在别处的闭包"]
    FB["VTA 完全解不出来<br/>回退到 CHA，标 cha-fallback"]
    EXT["外部边界边<br/>按调用点归类，不按解析出的实现"]
    REG["registers<br/>自有函数值传给外部函数"]
    REFL["registers + reflect<br/>自有值交给 text/template"]
    IMPL["registers + implicit-iface<br/>自有类型转成外部接口 / any<br/>外部代码可能调用它的方法"]
    LINK["direct + linkname<br/>无函数体声明按 go:linkname 链接"]

    SITE --> D1
    D1 -->|"是"| DIRECT
    D1 -->|"否：接口 / 函数值"| DYN
    DYN -->|"VTA 无结果"| FB
    SITE --> OWN
    OWN --> DIRECT
    OWN --> DYN
    OWN --> CB
    SITE --> EXT
    SITE --> REG
    SITE --> REFL
    SITE --> IMPL
    SITE --> LINK
```

边上的**标记**（`flags`）说明它的可信度：

| 标记 | 含义 |
|---|---|
| `external-iface` | 在外部接口（如 `types.Type`、`error`）上的动态调用；自有类型只是碰巧实现了它，精度抽查里误报集中在这一类 |
| `table` | 被调函数取自 map / slice 元素，运行时只会走其中一个 |
| `cha-fallback` | VTA 一个候选也没解出，用类层次分析的全部实现代替（库里常见） |
| `implicit-iface` | 自有类型被转成外部接口后，外部代码可能调用它的方法（过近似） |
| `reflect` | 自有值交给模板，模板按名字调用它的导出方法 |
| `linkname` | `//go:linkname` 链接的无函数体声明 |

函数的 **role** 说明它为什么有（或没有）调用方：`entry`、`init`、`registered`、`implicit`、`generated`、`iface`、`api`、`referenced`、`unused`，空表示被某个函数调用。详见 [graph.go](../internal/graph/graph.go) 里的注释。

## 数据模型：raw 与 model

```mermaid
classDiagram
    class Graph {
        schema_version
        meta
        packages
        functions
        boundaries
        edges
    }
    class Package {
        id
        path
    }
    class Function {
        id
        label
        package
        pos
        role
        implements
        implicit_callers
    }
    class Boundary {
        id
        path
        category
        kind
    }
    class Edge {
        from
        to
        kind
        mode
        flags
        candidates
        sites
    }
    class Model {
        schema_version
        policy
        rules
    }
    class Annotations {
        module
        internal
        tags
        fan_in
        reachable
        hidden
        hidden_by
    }

    Graph "1" o-- "*" Package
    Graph "1" o-- "*" Function
    Graph "1" o-- "*" Boundary
    Graph "1" o-- "*" Edge
    Function "*" --> "1" Package : 属于
    Edge "*" --> "1" Function : from
    Edge "*" --> "1" Function : to（或外部依赖）
    Model --|> Graph : 超集（追加字段）
    Model ..> Annotations : 每个节点和边追加
```

- **函数 ID** 用包内限定名（如 `(*internal/order.Service).Create`），不含行号，代码挪位置时 ID 不变，这样两次结果才能 diff。
- **外部依赖**：标准库按包，第三方按 module 合并成一个节点。
- **边的 `kind`**：`direct`、`dynamic`、`registers`、`callback`、`boundary`；**`mode`**：`call`、`go`、`defer`。

## 第 2 层：加工层做什么

```mermaid
flowchart LR
    RAW[("raw.json")]
    RULEFILE["rules.json<br/>（可选）"]

    subgraph ANN["Annotate"]
        direction TB
        N1["包 / 函数：module、internal、tags"]
        N2["外部依赖：std 或第三方 module、类别、低信号标签"]
        N3["扇入：有多少个不同的自有函数调用它<br/>（猜测的边不算）"]
        N4["可达性：从入口和 init 出发<br/>沿非猜测的边"]
        N5["默认隐藏：每个节点 / 边一个原因"]
    end

    MODEL[("model.json")]
    VIS["Visible(opts)<br/>静态导出用：原因开关 / 规则 / 扇入阈值 / 只看可达"]

    RAW --> N1
    RAW --> N2
    RAW --> N3
    RAW --> N4
    RAW --> N5
    RULEFILE -.-> MODEL
    N1 --> MODEL
    N2 --> MODEL
    N3 --> MODEL
    N4 --> MODEL
    N5 --> MODEL
    MODEL --> VIS
```

### 隐藏的两个来源

| 来源 | 谁决定 | 怎么表示 |
|---|---|---|
| **内置策略** | 加工层 | 写进 `hidden` / `hidden_by`，原因有三个：`low-confidence`（低可信边）、`implicit`（隐式边）、`low-signal`（`fmt`、`errors` 等常见依赖） |
| **规则** | 用户 | **不**写进 `hidden`，随 `model.rules` 带着，由画图的一方应用。所以在浏览器里可以随时开关、增删，不用重新分析 |

规则按路径元素边界匹配前缀：`golang.org/x/tools` 匹配它的子包，不匹配 `golang.org/x/toolsfoo`；末尾 `*` 表示纯前缀；一条规则里的多个字段要同时满足。匹配逻辑在 Go（`Rule.Matches*`）和前端各实现一份，用同一批用例核对过。

## 第 3 层：前端怎么画

```mermaid
flowchart TD
    M[("model.json<br/>内嵌在 viewer.html 里")]
    ST["状态 S<br/>模式 / 展开的包 / 选中 / 焦点 / 阈值 / 规则"]
    LS[("浏览器 localStorage<br/>规则 / 阈值 / 主题")]

    subgraph HIDE["决定画什么"]
        direction TB
        H1["内置隐藏原因<br/>（每个原因一个开关）"]
        H2["用户规则：module / package / function / tag"]
        H3["扇入阈值：外部依赖 / 自有函数"]
        H4["入口可达性"]
        H1 --> H5["可见的包 / 函数 / 依赖 / 边"]
        H2 --> H5
        H3 --> H5
        H4 --> H5
    end

    FOCUS["聚焦：从一个节点出发的 BFS<br/>下游 N 层 / 全部，上游 M 层"]
    BUILD["build：节点 + 聚合边<br/>包 = 嵌套分组"]
    ELK["ELK 布局<br/>分层 + 正交走线<br/>异步，结果按状态缓存"]
    SVG["SVG 渲染<br/>CSS 变量 = 深色 / 浅色"]
    PANEL["侧栏：详情 / 调用点 / 调用方 / 搜索<br/>过滤面板"]

    M --> ST
    LS <--> ST
    ST --> HIDE
    H5 --> FOCUS
    FOCUS --> BUILD
    BUILD --> ELK
    ELK --> SVG
    ST --> PANEL
    PANEL -->|"点击 / 双击 / 添加规则"| ST
    SVG -->|"选中 / 展开"| ST
```

要点：
- **布局结果被缓存**：只在“画出来的图”变了（展开、聚焦、隐藏条件）时才重新布局；选中、搜索不会重排。
- **布局是确定的**：ELK 使用固定随机种子，同一输入两次布局完全一致。
- **深色 / 浅色**：所有颜色走 CSS 变量，节点颜色在绘制时解析，切换主题只重绘不重排。
- **大图保护**：节点很多时 ELK 会阻塞页面，所以先提示再布局，并建议改用聚焦。

## 一次 `calltrail run` 发生了什么

```mermaid
sequenceDiagram
    actor U as 用户
    participant CLI as cmd/calltrail
    participant AN as internal/analyze
    participant MO as internal/model
    participant RE as internal/render
    participant FS as 输出目录

    U->>CLI: calltrail run -C 项目 -entry 入口 -rules 规则 -out-dir 目录
    CLI->>MO: LoadRules（若给了 -rules）
    CLI->>AN: Run(目录, 包模式或 entry, tags, 算法)
    AN-->>CLI: Graph（含 meta.entries）
    CLI->>MO: Annotate(Graph, 规则)
    MO-->>CLI: Model
    CLI->>FS: 写 raw.json
    CLI->>FS: 写 model.json
    CLI->>RE: HTML(Model)
    RE-->>CLI: 内嵌了 ELK 和 model 的单个 html
    CLI->>FS: 写 viewer.html
    CLI-->>U: 打印入口解析结果与统计
```

## 验证工具

分析器的结论靠两种**与调用图算法无关**的办法核对，工具在 [tools/recall](../tools/recall)：

```mermaid
flowchart LR
    GRAPH[("raw.json")]

    subgraph STATIC["静态对照：direct"]
        SA["读语法树里所有能静态确定的调用<br/>go/types"]
        SB["双向核对<br/>源码调用 在图里吗？<br/>direct 边 有源码调用吗？"]
        SA --> SB
    end

    subgraph RUNTIME["运行时对照：instrument + compare.py"]
        RA["给目标的每个函数入口插桩"]
        RB["真实运行（测试 / 命令）<br/>记录每个调用点的调用栈"]
        RC["换算成 图里的函数 ID<br/>最近的自有调用方 → 被调函数"]
        RD["召回：图里有这条边吗？<br/>运行过的函数被标成 unused 吗？"]
        RA --> RB --> RC --> RD
    end

    GRAPH --> SB
    GRAPH --> RD
```

- **静态对照**能发现：漏掉的直接调用、多余的直接边、泛型实例折叠是否正确。
- **运行时对照**能发现：接口分发、回调、反射、初始化等静态分析的盲区。
- 验证记录见 [precision-audit](precision-audit-2026-11-05.md)（抽查多余的边）、[recall-check](recall-check-2026-11-05.md)（用运行时调用栈查漏边）、[generics-validation](generics-validation-2026-11-05.md)（真实泛型项目）。

## 包结构

```mermaid
flowchart TD
    CMD["cmd/calltrail<br/>命令行"]
    AN["internal/analyze<br/>第 1 层：分析器"]
    GR["internal/graph<br/>raw.json 的 schema"]
    MO["internal/model<br/>第 2 层：标注 / 规则 / 过滤"]
    RE["internal/render<br/>第 3 层：前端（内嵌 elkjs）"]
    TOOLS["tools/recall<br/>验证工具"]

    CMD --> AN
    CMD --> MO
    CMD --> RE
    AN --> GR
    MO --> GR
    RE --> MO
    TOOLS --> GR
```

| 路径 | 内容 |
|---|---|
| `internal/analyze` | `analyze.go` 主流程；`implicit.go` 隐式边 / init / linkname；`ifaces.go` 接口与泛型；`entry.go` 入口解析；`classify.go` 辅助 |
| `internal/graph` | schema 与稳定排序 |
| `internal/model` | `model.go` 标注与过滤；`rules.go` 规则文件 |
| `internal/render` | `viewer.html`（前端，单文件）；`render.go` 内嵌与填充；`elk.bundled.js`（[elkjs](https://github.com/kieler/elkjs) 布局引擎，EPL-2.0，见 [ELK-LICENSE.md](../internal/render/ELK-LICENSE.md)） |
| `tools/recall` | `instrument` 插桩；`compare.py` 运行时对照；`direct` 静态对照 |
