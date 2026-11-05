# 漏边验证：用运行时调用关系对照静态图（spike，2026-11-05）

> English summary: [validation.en.md](validation.en.md)

**问题**：静态图有没有漏掉真实存在的调用？之前的精度抽查只能发现多余的边，发现不了缺失的边。

**方法**
1. [tools/recall/instrument](../tools/recall/instrument/main.go) 复制 x/tools 并给每个函数入口插桩（4059 个函数，生成文件除外）。每个调用点第一次执行时，记录完整调用栈。
2. 用插桩版 `deadcode`（`./...`、`-test ./...`、`-json`）和 `callgraph`（static / cha / rta / vta 四种算法）分析本项目，共 7 次运行。
3. [tools/recall/compare.py](../tools/recall/compare.py) 把每次调用的“最近自有调用方 → 被调函数”换算成图里的函数 ID，查静态图 `raw.json`（x/tools 的 `./cmd/...`，VTA）里有没有这条边。如果两者之间隔着标准库等外部帧，单独记为“经由外部代码回调”。

**范围**：1638 个不同的运行时调用对，涉及 569 个函数（占图中 2633 个自有函数的约 22%）。

## 结果

| | 条数 | 召回 |
|---|---|---|
| 调用栈里直接相邻（自有函数直接调自有函数） | 1630 / 1631 命中 | **99.9%** |
| 经由外部代码回调（标准库等夹在中间） | 2 / 7 命中 | 28.6% |
| 合计 | 1632 / 1638 | 99.6% |

- 命中的 1632 条里，1139 条由直接边确认，**491 条由接口/函数值（dynamic）边确认**，2 条由注册边确认。也就是说接口分发在这次运行里被充分检验过，**没有一条自有代码内的接口分发被漏掉**。
- 被默认折叠的 265 条低可信边里，没有一条被运行时调用确认。
- 从 main 可达性看：运行过的 569 个函数里，558 个能从实际执行的 `main` 沿静态图走到，**11 个走不到**。

## 漏掉的 17 个对象的原因

| 类别 | 条数 | 例子 | 说明 |
|---|---|---|---|
| A. 自有类型实现了外部接口，由外部代码调用 | 2 | `importerFunc.Import`（被 go/types 调用）、`(*ssa.Function).String`（被 fmt / template 调用） | 值被交给外部代码，外部代码经接口调用。分析器只处理“函数值传给外部函数”，没处理“实现了外部接口的值传出去” |
| B. 反射 | 3+1 | `(*Edge).Line/Column/Dynamic`，经 `text/template` 的 `{{.Line}}` 调用；`pos` 因此也走不到 | 文档里已预期的盲区 |
| C. 包初始化 | 5 | `ssaNamedType`、`edge.info`、`keys.NewString/NewError/NewTag` | 只被包级变量初始化表达式调用；分析器丢弃了 init 和包级初始化，这些函数在图里没有调用方 |
| D. `go:linkname` | 1 | `ssautil.AllFunctions → ssa.isSyntactic` | 无函数体的声明被链接到另一个包，静态分析看不到 |

（B 类的 `pos` 是被 B 类函数调用的函数，所以只算连带影响。）

## 最重要的后果：`role` 标签会误导

这 11 个运行过的函数在图里的角色是：

| 函数 | role |
|---|---|
| `ssaNamedType`、`edge.info`、`isSyntactic` | `unused`（疑似死代码） |
| `keys.NewString/NewError/NewTag`、`importerFunc.Import`、`Edge.Line` | `api`（导出且无调用方） |

图中共 21 个 `unused`，这一次运行就证明其中至少 3 个并非死代码。**“没有调用方 = 疑似死代码”的标签，在 C、D 两类情况下是错的。**

## 局限

- 只有 7 次运行，都是静态分析类工具，只覆盖约 22% 的函数；“99.9%”是在被执行到的那部分里的召回，不代表整个图。
- 同一个调用点只记第一次，同一调用点上的多个目标只靠不同栈记录；goroutine 的起点（无自有调用方）没有计入，所以 `go` 语句引出的边没有验证。
- 同一个调用对可能被多个调用点支撑，“命中”只表示函数对之间有边，不表示具体调用点都对。
- 静态图里被命中的边，不能反过来说明图里其余的边都是对的；这里只验证了召回，没有验证精度（精度见 [precision-audit-2026-11-05.md](precision-audit-2026-11-05.md)）。

## 复现

```bash
go run ./tools/recall/instrument -src <x/tools 副本> -dst <插桩目录>
cd <插桩目录> && go build -gcflags='golang.org/x/tools/...=-l' -o <bin>/deadcode ./cmd/deadcode   # callgraph 同理
CALLTRAIL_TRACE=trace.txt <bin>/deadcode ./...                                                          # 多次运行会追加
python3 -I tools/recall/compare.py raw.json trace.txt golang.org/x/tools
```

---

## 修复与复核（同日）

**分析器改动**（[implicit.go](../internal/analyze/implicit.go)、[analyze.go](../internal/analyze/analyze.go)）

| 针对 | 做法 |
|---|---|
| C 包初始化 | 每个包的“包级变量初始化 + 所有 init()”合并成一个 `<pkg>.init` 节点（role=`init`），它的调用像普通函数一样出边。包与包之间的初始化顺序调用不画 |
| A 自有类型被外部接口调用 | 自有类型被转换成**外部**接口（含 `error`）时，给接口要求的方法加一条 `registers` 边，标 `implicit-iface`；转成 `any` 时只处理 `String/Error/GoString/Format/MarshalJSON/MarshalText/MarshalBinary` 这几个被 fmt、json 动态调用的方法 |
| B 反射（仅模板） | 自有值传给 `text/template` / `html/template` 的调用，其导出方法加 `registers` 边，标 `reflect` |
| D `go:linkname` | 无函数体的声明按 `//go:linkname` 链接到本 module 内另一个函数，加一条 `direct` 边，标 `linkname` |
| role | 新增 `init`、`implicit`（外部代码经接口/反射调用）、`referenced`（不调用但被当作值使用）；`unused` 只留下以上都不满足的 |

前端：“折叠低可信边”扩展为“折叠低可信 / 隐式边”，折叠 `external-iface`、`table`、`implicit-iface`；`reflect`、`linkname` 不折叠。

**设计集复核**（同一份 deadcode / callgraph 轨迹，修复是照着它的失败案例做的，所以只算“自测”）

| | 修复前 | 修复后 |
|---|---|---|
| 直接相邻调用召回 | 1630 / 1631 | **1641 / 1641** |
| 经由外部回调召回 | 2 / 7 | **6 / 7** |
| 运行过却从 main / init 走不到的函数 | 11 | **0** |

**独立验证集**：用**没有参与设计的**另外 7 个命令（stringer、goimports、gotype、ssadump、bundle、godex、digraph）在一个小样例项目上运行，得到新的轨迹（1565 个调用对，678 个函数）。

| | 修复前 | 修复后 |
|---|---|---|
| 直接相邻调用召回 | 1532 / 1532 | **1546 / 1546** |
| 经由外部回调召回 | 5 / 16（31%） | **18 / 19（95%）** |
| 运行过却走不到的函数 | 22 | **0** |

这次独立验证还抓到修复过程中引入的一个 bug：名为 `init` 的**方法**（如 `(*ProcessEnv).init`）被当成包初始化函数排除了，已修复。

**图的变化**（x/tools `./cmd/...`）：`unused` 从 21 降到 6；`registers` 边从 5 增到 214（201 条隐式接口、6 条反射、7 条显式注册）；新增 21 个 `init` 节点；`api` 从 417 降到 315，其中 97 个改标 `implicit`。

## 仍然漏的

| 例子 | 原因 |
|---|---|
| `doCallgraph → (*ssa.Function).String` | 值放在模板数据的**字段**里，由 `fmt` 经 `Stringer` 调用。要追踪“自有值作为结构体字段传给外部”才能发现，没做 |
| `ssadump.doMain → (*BuilderMode).Set` | 注册发生在 `flag.Var(...)`（init 里），回调却在 `flag.Parse()`（`doMain` 里）触发。图里的边是 `init → Set`，运行时的最近调用方是 `doMain`。这是“边画在注册者上，而不是触发者上”的固有语义，不算漏 |

剩下的 6 个 `unused` 里有 4 个是为了满足接口而存在、从不被调用的标记方法（如 `nodeImpl`），严格说不是死代码；`role` 还没有区分这类函数。

## 局限

- 隐式边（`implicit-iface`）是过近似：每次转成外部接口都会连到该接口要求的方法，并不意味着真的会被调用。它让 `role` 更准，但边本身可信度低，所以默认折叠。
- 独立验证集也只有 7 个命令、一个很小的样例项目；召回 95% 的置信区间很宽。
- 按名字的反射（模板以外）、cgo 导出、push 形式的 `go:linkname` 仍然没有建模。

---

## 标记接口方法（同日）

**问题**：剩下的 6 个 `unused` 里有 4 个是为满足接口而存在、从不被调用的方法（如 `nodeImpl()` 标记方法），不是死代码。

**做法**（[ifaces.go](../internal/analyze/ifaces.go)）
- 收集程序里所有具名、非泛型的接口：自有的全部收，外部的只收**导出**的（别的包的私有接口没人能用，只会是噪音）。
- 对每个自有方法，找出它的接收者类型**完整实现**、且包含这个方法的接口，记在 `implements`（最多 4 个：先自有接口，再方法最少的）。
- 没有调用方、也不是 `implicit`、但实现了接口的方法，role 标为 `iface`（优先于 `api`）。前端详情里列出“实现的接口”，并注明“不是死代码”。

**结果**（x/tools `./cmd/...`）：`unused` 6 → **3**；`api` 315 → 212；`iface` 106；557 个方法带有 `implements`。

**用运行轨迹核对 role**（两份轨迹：设计集与独立验证集）：

| role | 运行过的 / 图中总数（独立验证集） |
|---|---|
| `unused` | 0 / 3 |
| `iface`、`api`、`referenced`、`init` | 均为 0 |
| `implicit` | 14 / 97 |

没有一个运行过的函数被标成 `unused`；原来 `api` 里运行过的函数现在都在 `implicit`。

**剩下的 3 个 `unused`**
| 函数 | 实际情况 |
|---|---|
| `cmd/splitdwarf.describe` | 真的没人调用，只出现在一行注释里 |
| `go/ssa.printDomTreeDot` | 真的没人调用，调试用，只出现在注释里 |
| `(internal/graph.transpose).unwrapPreservingNodes` | ~~误标~~（**此处原结论有误，已在下面“支持泛型”一节更正**）：在这个程序里确实没有调用方 |

（原文此处写“最后一个是误标，调用在静态图里没有边”，这个判断是错的，见下节。）

---

## 支持泛型（同日）

**先更正上一节的错误结论。** 此前认为 `transpose[NodeID].unwrapPreservingNodes` 在 `compact.go:45` 被调用、图里漏了边。用 SSA 实测后发现不是：

- 调用点 `g2.unwrapPreservingNodes()` 的接口 `nodePreserving` 要求方法返回 `Graph[int]`。
- 整个 `./cmd/...` 里只实例化过 `transpose[string]`，它不满足 `nodePreserving`。
- SSA 里这个方法实例唯一的入边来自编译器生成的指针包装函数，而该包装函数本身没人调用。

所以这个调用在**这个程序里**到不了它，`unused` 是对的，并不是误标，也没有漏边。（它是否是死代码取决于程序用哪些类型实例化泛型，换一个程序可能就会被调用。）

**真正缺的泛型支持有两处**，已用 [analyze_test.go](../internal/analyze/analyze_test.go) 和 [testdata/generics](../internal/analyze/testdata/generics/main.go) 固定下来：

| 缺口 | 做法 |
|---|---|
| `implements` 跳过泛型类型和泛型接口 | 泛型类型没有自己的方法集，改为检查源码里**实际出现的实例化**（`Stack[int]`、`Container[int]`）；接口名不带类型实参；仍带类型参数的实例（泛型代码内部的 `Stack[T]`）不算 |
| 从未被实例化的泛型类型，其方法根本不在图里 | 额外按声明列出泛型类型的方法，保证不会悄悄消失（它们没有实例，也就没有出边） |

测试项目覆盖的情形（原先已正确的和新增的都在内）：同一个泛型函数的多个实例折叠成一个节点；经泛型接口 `Container[int]` 的动态分发解析到泛型类型 `Stack` 的方法；传给泛型 `Map` 的闭包产生 `callback` 边；泛型类型上的标记方法 `seal()` 标为 `iface`；从未实例化的 `Orphan[T].Hello` 仍然出现在图里。加测试前，其中 3 项是失败的。

**在 x/tools 上的实际影响很小**：只多出 1 个函数（一个从未被实例化的泛型类型的方法）、3 个方法多了 `implements`，role 没有变化，两份运行轨迹的复核结果不变。原因是 `./cmd/...` 里几乎没有泛型代码，所以这次的验证主要靠新写的测试项目，而不是真实项目。

**仍然没做**：
- 从未被实例化的泛型方法没有出边（它们没有实例，调用图里没有节点），所以它们调用的函数可能被误标为没有调用方。
- 泛型接口只按源码里出现过的实例化去匹配；通过类型参数约束（`T Container[int]` 之类）间接满足的关系不处理。
