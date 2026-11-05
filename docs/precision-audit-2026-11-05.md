# 接口/函数值边的精度抽查（spike，2026-11-05）

**对象**：golang.org/x/tools v0.52.0 的 `./cmd/...`，VTA。2127 条动态（接口/函数值）边。
**方法**：按候选数分 5 层（1 / 2-3 / 4-6 / 7-10 / 11+），每层随机抽 5 条（seed=42，共 25 条），对着源码看调用点和目标，逐条判断“这个调用点运行时是否真可能调到该目标”。
**局限**：样本小（95% 置信区间很宽）；判断靠读代码，#4 #5 #6 是推断而非证明；直接调用边未抽查。

| # | 层 | 候选 | 调用点 | 结论 | 原因 |
|---|---|---|---|---|---|
| 1 | 1 | 1 | ssa/func.go:321 `instr.(setNumable).setNum` | 真 | 接口仅一个实现 |
| 2 | 1 | 1 | fuzz-generator/generator.go:1426 `r.AddrTaken()` | 真 | 所有 parm 嵌入同一个方法 |
| 3 | 1 | 1 | fuzz-generator/generator.go:833 `SetAddrTaken` | 真 | 同上 |
| 4 | 1 | 1 | ssa/interp/ops.go:846 `…Type().Underlying()` | **疑似误报** | 接口是外部的 `types.Type`，唯一的自有实现 `gcimporter.anyType` 不会流到这里 |
| 5 | 1 | 1 | gopathwalk/walk.go:271 `d.IsDir()` | **疑似误报** | 外部接口 `fs.DirEntry`；`txtar.fileinfo` 实现了它，但这里的值来自真实文件系统 |
| 6 | 2-3 | 2 | ssa/builder.go:518 `xt.String()` | **疑似误报** | 外部接口 `types.Type`；`interp.opaqueType` 只在解释器里创建 |
| 7 | 2-3 | 3 | ssa/func.go:493 `outer.Type()` | 真 | `lookup` 确实可能返回 Alloc |
| 8 | 2-3 | 3 | vta/internal/trie/builder.go:460 `r.find` | 真 | `r` 可能是 branch |
| 9 | 2-3 | 2 | go/loader/util.go:52 `ctxt.OpenFile(file)` | 真，**但目标有误导** | 实际调的是 `OverlayContext` 内部的闭包，闭包被并入外层后显示成“调用 OverlayContext” |
| 10 | 2-3 | 3 | vta/graph.go:630 `c.Common()` | 真 | Go/Defer/Call 都实现 |
| 11 | 4-6 | 5 | ssa/typeset.go:100 `f(u)` | 真，**但目标有误导** | 实际调的是 `isBasicConvTypes` 里传给 `underIs` 的闭包，同 #9 |
| 12 | 4-6 | 4 | ssa/builder.go:1151 `loc.typ()` | 真 | lvalue 的多个实现 |
| 13 | 4-6 | 6 | typedefparm.go:78 `p.target.HasPointer()` | 真 | target 可以是任意 parm |
| 14 | 4-6 | 6 | generator.go:947 `a.eltype.TypeName()` | 真 | 数组元素可以是任意 parm |
| 15 | 4-6 | 6 | 同上，目标 mapparm | 真（合理） | 数组元素可以是 map |
| 16 | 7-10 | 7 | generator.go:1604 `p.NumElements()` | 真 | p 是任意 parm |
| 17 | 7-10 | 8 | vta/graph.go:473 `e.Tuple.Type()` | **误报** | `Extract.Tuple` 一定是元组类型，`*ssa.Function` 不可能 |
| 18 | 7-10 | 8 | vta/graph.go:486 `f.X.Type()` | 真 | 闭包捕获的结构体变量，FieldAddr.X 可以是 FreeVar |
| 19 | 7-10 | 8 | ssa/sanity.go:117 `e.Type()` | 真 | phi 的边可以是函数值常量 |
| 20 | 7-10 | 7 | vta/graph.go:142 `l.val.Name()` | **误报** | `nodeFromVal` 对 Global 返回 `global{}`，不会构造 `local` |
| 21 | 11+ | 37 | rta/rta.go:268 `instr.Operands` | 真 | 37 个就是全部指令类型 |
| 22 | 11+ | 16 | vta/propagation.go:196 `node.Type()` | 真 | 节点接口的实现 |
| 23 | 11+ | 80 | ssa/interp/interp.go:519 `ext(fr, args)` | 真但**噪音** | 按函数名查表，运行时只会调其中一个，静态上 80 个都“可能” |
| 24 | 11+ | 15 | vta/utils.go:51 `n.Type()` | 真 | 节点接口的实现 |
| 25 | 11+ | 80 | 同 #23 | 真但**噪音** | 同 #23 |

## 汇总

| | 条数 |
|---|---|
| 真且有意义 | 16 |
| 真，但因闭包并入外层而目标有误导 | 2 |
| 真，但是“查表分发”的噪音 | 2 |
| 误报或疑似误报 | 5 |

**按候选数分层**：

| 候选数 | 误报 | 备注 |
|---|---|---|
| 1 | 2 / 5 | |
| 2-3 | 1 / 5 | |
| 4-6 | 0 / 5 | 1 条闭包误导 |
| 7-10 | 2 / 5 | |
| 11+ | 0 / 5 | 2 条查表噪音 |

## 结论

1. **候选数不是精度指标。** 5 条误报里 3 条候选数 ≤ 3；当前“候选数 > 5 就折叠”的规则折叠掉的 13 条里 11 条是真边，只拦住 2 条误报。它只是降噪手段，不能当作可信度。
2. **“外部接口 + 自有实现”是高风险模式。** 抽到的 3 条（#4 #5 #6）全部疑似误报：自有类型碰巧实现了外部接口，但值不会流到这个调用点。分析器应在动态边上记录接口的声明包，让它可被过滤或降权。
3. **闭包并入外层函数会制造误导性的边。** 回调场景下，“A 调 B”实际是“A 调用了 B 里定义的闭包”，会出现 `isBasicConvTypes → underIs → isBasicConvTypes` 式的假环。需要把这类边单独标出来。
4. **“查表分发”（函数值 map）应标记为数据相关**，不应与接口分发混为一谈。
5. 抽样里没有出现“漏边”，但本次方法只能发现多余的边，不能发现缺失的边；漏边需要另行验证（例如用运行时覆盖数据对比）。

## 修复后复核（同一批 25 条样本，同一份 x/tools 数据）

分析器改动：动态边记录接口声明包（`ifaces`），并打 `external-iface` / `table` 标记；闭包引起的边改为独立的 `callback` 类型。前端改为按标记折叠，不再按候选数。

| 样本 | 现在的标记 | 与人工判断 |
|---|---|---|
| #4 #5 #6（外部接口，疑似误报） | `external-iface` | 一致 |
| #9 #11（闭包并入外层造成的误导） | `callback` | 一致 |
| #23 #25（查表分发） | `table` | 一致 |
| 其余 15 条判为真的边 | 无标记 | 无误标 |
| #17 #20（自有接口上的误报） | 无标记 | **未能识别**：靠代码不变量才能排除，分析器看不出 |

全量 x/tools 数据：动态边 1944 条（原 2127，其中 184 条改为 `callback`）；265 条（约 14%）被标为低可信——外部接口 178、查表 87。外部接口里以 `go/types.Type`（100）和 `error`（32）为主。

**已知不确定**：随机看了 8 条 `external-iface` 的边，多数像误报，但没有逐条验证；`error` 接口那 32 条依据最少（只看了 2 条）。标记只会让这些边默认折叠，不会删除，界面上有数量提示和开关。
