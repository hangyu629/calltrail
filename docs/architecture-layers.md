# 三层产出：原始层 → 加工层 → 视图层

[English](architecture-layers.en.md) | [中文](architecture-layers.md)

```
Go 源码 ──calltrail analyze──▶ raw.json ──calltrail annotate──▶ model.json ──calltrail view──▶ viewer.html
          (x/tools)        事实            + 标注 + 规则          一个视图          （以后：archify.json …）
```

`calltrail run` 一次生成三个产出，写进同一个目录：

```bash
calltrail run -out-dir calltrail-out ./...                    # raw.json, model.json, viewer.html
calltrail run -rules calltrail.rules.json -out-dir calltrail-out   # 带入忽略规则
```

| 层 | 文件 | 内容 | 原则 |
|---|---|---|---|
| 1 原始层 | `raw.json`（[internal/graph](../internal/graph/graph.go)） | 调用 x/tools 分析出的结果：包、函数、外部依赖、边（含 `kind`、`flags`、调用点）、role | **只放事实，不做取舍。** 不丢任何外部依赖（`fmt`、`errors` 也在），不含任何展示策略 |
| 2 加工层 | `model.json`（[internal/model](../internal/model/model.go)） | 原始层的超集：每个节点/边追加标注 | **只追加字段，不改不删事实**；自包含，视图层只读它 |
| 3 视图层 | `viewer.html`（[internal/render](../internal/render/render.go)） | 把 model 转成某个前端需要的形式 | 每个视图是 model 的一个只读转换；以后的 Archify 导出是它的兄弟 |

## 加工层加了什么

| 对象 | 追加字段 |
|---|---|
| 包 | `module`、`internal`（路径里有 `internal` 元素）、`tags` |
| 函数 | `module`、`internal`、`tags`（`role:…`、`exported`）、`fan_in`、`hidden` / `hidden_by` |
| 外部依赖 | `module`（标准库是 `std`，第三方是它的 module 路径）、`tags`（`std`/`third`、`cat:db`…、`low-signal`）、`fan_in`、`hidden` / `hidden_by` |
| 边 | `id`（稳定唯一）、`hidden` / `hidden_by` |
| 包 / 函数 / 外部依赖 | 指定了入口时还有 `reachable`（见下） |
| 顶层 | `policy`（隐藏原因的说明，视图用它来生成开关）、`rules`（用户规则） |

### 内置的隐藏策略（原因）

每个被默认隐藏的节点/边都带着原因，视图为每个原因提供一个开关：

| `hidden_by` | 隐藏什么 |
|---|---|
| `low-confidence` | 外部接口上的动态调用、查表分发、仅类层次推测的边（精度抽查里误报集中的类别） |
| `implicit` | 自有类型被转成外部接口后“可能被外部调用”的边（过近似，用来让 role 更准） |
| `low-signal` | `fmt`、`errors`、`strings`、`context`… 这类到处都在调用的标准库依赖；以及只碰这些依赖的 `init` 节点 |

## 忽略规则

规则只说“忽略什么”，字段之间是“且”：

```json
{"rules": [
  {"module": "golang.org/x/sys"},
  {"package": "internal/gen/*"},
  {"tag": "internal"},
  {"function": "(*go/ssa.register).Type"},
  {"function": "internal/util.*"},
  {"package": "cmd", "tag": "internal", "note": "字段同时给出时都要满足"}
]}
```

- **匹配方式**：按路径元素边界的前缀。`golang.org/x/tools` 匹配它的子包，不匹配 `golang.org/x/toolsfoo`；末尾 `*` 表示纯前缀。`module` 对自有包是主 module；标准库的 module 是 `std`。
- **`function` 规则**：只隐藏函数（id 精确匹配，末尾 `*` 表示前缀），不会隐藏它所在的包或任何依赖；`module` / `package` / `tag` 规则对函数通过它所在的包生效。
- **效果**：命中的包连同它的函数、以及接触到它们的边一起隐藏。
- **规则不写进 `hidden`**，而是随 model 带着（`model.rules`），由画图的一方应用。所以在浏览器里可以随时开关、增删，不用重新分析。
- **匹配规则只定义一次**：Go 里是 `Rule.Matches*`，浏览器里的 JS 与它逐字对应；两边用同一批用例核对过（16 个，全部一致）。Go 侧的 `Model.Visible()` 用同样的规则做静态导出（Archify 适配器会用它）。

### 在界面里设置

右上角「过滤」：
- **外部依赖** 总开关；
- 每个隐藏原因一个开关，带数量；
- 忽略规则列表（可勾选启用/禁用，用户规则可删除，规则文件里的规则只能禁用）；
- 添加规则（模块 / 包 / 标签）和两个预设（忽略所有 internal 包、忽略标准库）；
- **导出规则** 得到 `calltrail.rules.json`，下次 `calltrail run -rules` 带入；**导入规则** 反向。

详情面板里也有“忽略此包 / 忽略此依赖”的按钮。用户的设置按 module 保存在浏览器本地（localStorage）。

## 指定入口（`-entry`）

原始层的 `meta.entry` 记录用户给的原话，`meta.entries` 是解析出的入口函数 id。解析规则：文件 → 它所在的包，文件里声明了 `main` 取 `main`，否则取文件里声明的所有函数；包 → main 包取 `main`，否则取导出的函数；`路径:名字` → 精确指定。原始层**不裁剪**：它仍然是入口所在包及其依赖的完整事实。

“可达”是加工层的标注 `reachable`：从入口出发沿边走到的，走的是**不是猜测**的边（低可信、隐式边不传播，否则类层次推测会把无关的东西带进来）；包初始化（`init` 节点）在 `main` 之前运行，所以也是起点。包的 `reachable` = 里面有函数可达；外部依赖的 `reachable` = 被可达的函数调用。Go 侧导出用 `ViewOptions.OnlyReachable`；没有入口时它是空操作。

## 常见的东西太多？三种办法

`fmt`、`errors` 这类到处都在调用的东西会把图糊成一团。有三层办法，从省事到精细：

1. **顶栏的「隐藏常见依赖」开关**（默认开）。隐藏的是内置清单里的标准库依赖（26 个，如 `fmt`、`errors`、`strings`、`context`、`log`、`flag`，另有 `math/*` 和 `internal/*`；鼠标放上去能看到这份结果里实际有哪些），也就是 `low-signal` 这个原因。图里本来画的是**包级**的依赖节点（不是 `fmt.Println` 这样的单个函数），所以隐藏的是整个包。
2. **按调用次数自动隐藏**（「过滤」里）：
   - 外部依赖：被超过 N 个不同函数调用的就隐藏；
   - 自有函数：被超过 N 个不同函数调用的就隐藏；
   - 0 表示不启用。调用次数是 model 里的 `fan_in`：有多少个不同的自有函数调用或引用它，**猜测的边（低可信、隐式）不算**，低信号依赖的边算（它们是真实调用）。
   - 这个办法是数据驱动的：内置清单是写死的，而 x/tools 里 `os`（158 个调用方）、`path/filepath`（106）、`go/types`（408）这些同样“到处都是”，清单里没有。
3. **「最常被调用」列表**：「过滤」里列出现在还显示的、调用次数最多的 8 个外部依赖和 8 个自有函数，点 ✕ 直接生成忽略规则（依赖按包或模块，函数按函数 id）。

阈值默认关闭：被很多函数调用的有可能正是你关心的（比如一个数据库依赖被所有 repo 调用）。要不要隐藏由你决定。Go 侧导出用 `ViewOptions{MaxFanInBoundary, MaxFanInFunction}` 做同样的事。

## 命令

```
calltrail analyze  [-C dir] [-algo vta|cha|static] [-tags t] [-entry E] [-o raw.json] [patterns...]
calltrail annotate [-rules rules.json] [-o model.json] raw.json
calltrail view     [-rules rules.json] [-o viewer.html] model.json|raw.json
calltrail run      [-C dir] [-algo ...] [-tags t] [-entry E] [-rules rules.json] [-out-dir dir] [patterns...]
calltrail demo     ...  [-o out.html]        # 只要 html
```

`view` 接受 `model.json` 或 `raw.json`（后者先自动加工）；`-rules` 只对 `raw.json` 有效，规则已经在 model 里时拒绝，避免两处规则互相打架。

## 设计取舍

- **分析器不再丢低信号依赖**（原来在分析阶段就把 `fmt` 等去掉了，这是取舍，不该在原始层）。代价：x/tools 的 `graph.json` 从 2602 KB 变成约 3.0 MB，边 7512 → 9060，边界 43 → 69；多出来的几乎都是 `low-signal`，默认隐藏。同时多出 20 个只碰 `flag` 之类的 `init` 节点，由加工层隐藏。
- **展示上的取舍放在加工层**：原来写在前端 JS 里的“默认折叠哪些边”变成了 model 里的 `hidden_by`，前端按 `policy` 生成开关。
- **浅色模式**：见下。

## 浅色模式

默认跟随系统（`prefers-color-scheme`），右上角按钮手动切换，选择记在浏览器里；没手动选过时，系统切换会跟着变。浅色下语义色保持同一含义、只是加深以便在白底上读清（遵循 Archify 的“主题等价”规则）。所有颜色都走 CSS 变量，箭头、网格线、分组底色也跟着换。

## 还没做

- **Archify 适配器**（视图层的第二个视图）：接口已经留好（读 model、用 `Visible()` 过滤），适配器本身没写。
- **规则里的“只显示”**（白名单）：现在只有“忽略”。
- **gRPC / HTTP 入口识别**与“单个接口的完整链路视图”：计划中；入口识别会作为加工层的一种标注（`entrypoint`）加进来。
- 用户在界面里设置的规则只存在浏览器本地；要共享给团队需要导出后放进仓库、用 `-rules` 带入。
- 网页界面目前只有中文。
