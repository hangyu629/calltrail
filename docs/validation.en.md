# How the analysis was verified

[English](validation.en.md) | [中文: the detailed records](precision-audit-2026-11-05.md)

This is a summary, with the numbers, of three verification efforts carried out on 2026-11-05. The detailed records are in Chinese: [precision-audit](precision-audit-2026-11-05.md), [recall-check](recall-check-2026-11-05.md), [generics-validation](generics-validation-2026-11-05.md). The tools are in [tools/recall](../tools/recall).

A static call graph can be wrong in two ways: it can contain edges that do not exist (**precision**) and it can miss edges that do (**recall**). Each check below targets one of them, and none depends on the call-graph algorithm being tested.

| Check | Looks for | How |
|---|---|---|
| Precision audit | Extra edges | Sample dynamic edges by number of candidates, read the source, judge each by hand |
| Runtime recall check | Missed edges | Instrument the target, run it, record the call stack of every call site, look each pair up in the graph |
| Static check (`direct`) | Missed and extra *direct* edges | Read every statically resolvable call from the syntax tree and check it against the graph in both directions |

## 1. Precision audit: extra edges

**Target**: golang.org/x/tools `./cmd/...`, VTA, 2127 dynamic (interface / function-value) edges. **Method**: five strata by candidate count (1, 2–3, 4–6, 7–10, 11+), five random edges each (25 in total, fixed seed), judged by reading the call site and the target.

| Verdict | Edges |
|---|---|
| True and meaningful | 16 |
| True, but the target is misleading (closures are folded into the enclosing function) | 2 |
| True, but noise (the callee comes from a map of functions, only one runs) | 2 |
| Wrong or probably wrong | 5 |

Findings:

1. **The number of candidates does not predict wrong edges.** Three of the five wrong edges had three candidates or fewer. A rule "fold edges with more than 5 candidates" would have folded 13 edges, 11 of them real.
2. **"External interface + own implementer" is the risky pattern.** The three sampled edges of this kind were all wrong or probably wrong: an own type happens to implement an external interface (`types.Type`, `fs.DirEntry`), but the value does not flow to the call site.
3. **Folding closures into the enclosing function produces misleading edges** (fake cycles such as `isBasicConvTypes → underIs → isBasicConvTypes`).
4. **Table dispatch** (a map of functions) is data-dependent and should not be mixed with interface dispatch.

Changes: dynamic edges record the interface and get flags (`external-iface`, `table`); calls into folded closures became a separate `callback` kind; the front end folds by flag instead of by candidate count. Re-checking the same 25 edges: every flagged edge matched the manual verdict, the 15 true edges were not flagged, and two wrong edges on *own* interfaces were not recognised (excluding them needs program invariants the analyzer cannot see). On all of x/tools, 265 of 1944 dynamic edges (about 14%) are flagged low-confidence (178 external-interface, 87 table); 184 moved to `callback`.

Limits: a small sample; judged by reading code; the weakest evidence is for `error`-interface edges (32 flagged, 2 inspected); direct edges were not sampled, and this method cannot find missing edges.

## 2. Runtime recall check

**Method**: copy x/tools, instrument 4059 functions, run the instrumented `deadcode` (three forms) and `callgraph` (four algorithms) on a project — seven runs — and record the call stack at each call site the first time it runs. `compare.py` maps each call to *nearest own caller → callee* and looks it up in the graph. Calls with external frames (standard library and so on) in between are counted separately as "via external code".

**First result**: 1638 distinct runtime pairs over 569 functions (about 22% of the 2633 own functions).

| | Found | Recall |
|---|---|---|
| Own caller directly adjacent on the stack | 1630 / 1631 | 99.9% |
| Via external code (callbacks) | 2 / 7 | 28.6% |

491 of the confirmed pairs rest on dynamic edges, and none of the dynamic calls inside own code was missed. None of the 265 low-confidence edges was confirmed by the runs. 11 executed functions could not be reached from `main` along graph edges.

**Why 17 things were missed** (four causes):

| Cause | Example |
|---|---|
| A. An own type implements an *external* interface and external code calls it | `importerFunc.Import` (called by go/types), `(*ssa.Function).String` (called by fmt / template) |
| B. Reflection | exported methods called by `text/template` by name |
| C. Package initialisation | functions called only from package-level variable initialisers |
| D. `//go:linkname` | a body-less declaration linked to a function in another package |

The most important consequence: **roles misled**. Functions that did run were labelled `unused` (probably dead) or `api`; "no caller means probably dead" was wrong in cases C and D.

**Fixes**: package initialisation merged into one `<pkg>.init` node per package; own values converted to external interfaces get implicit edges (`implicit-iface`); values given to templates get `reflect` edges; `linkname` edges; new roles `init`, `implicit`, `referenced`.

| | Before | After |
|---|---|---|
| Design set (same runs): directly adjacent | 1630 / 1631 | 1641 / 1641 |
| Design set: via external code | 2 / 7 | 6 / 7 |
| Design set: executed but unreachable functions | 11 | 0 |
| **Hold-out set** (seven other commands that took no part in the design: stringer, goimports, gotype, ssadump, bundle, godex, digraph; 1565 pairs, 678 functions): directly adjacent | 1532 / 1532 | 1546 / 1546 |
| Hold-out: via external code | 5 / 16 (31%) | 18 / 19 (95%) |
| Hold-out: executed but unreachable functions | 22 | 0 |

The fixes were designed from the failures of the first set, so only the hold-out set counts as independent. It also caught a bug in the fix itself: a *method* named `init` was being treated as package initialisation.

**Interface methods.** Four of the six remaining `unused` functions existed only to satisfy an interface (marker methods such as `nodeImpl()`). Methods that implement an interface but have no caller now get the role `iface`, and every method lists the interfaces it implements. `unused` went from 6 to 3, and none of the functions that ran is labelled `unused`.

**Still missed**: a value stored in a *field* of template data and called by `fmt` through `Stringer`; and calls where the edge is drawn at the registering function rather than at the function that triggers the callback (`flag.Var` in `init` registers, `flag.Parse` triggers) — a deliberate meaning of the edge, not an error.

## 3. Generics and libraries: the official MCP SDK

**Target**: `github.com/modelcontextprotocol/go-sdk` v1.4.0 — 29 packages, 696 own functions, 53 generic declarations (`AddTool[In, Out]`, `featureSet[T]`, `binder[T, State]`, ...). The run needed edits in a throw-away copy only: dependency versions pinned to what was available offline, a test-only dependency dropped, and one build tag adjusted.

**Static check** (every call in the syntax tree against the graph):

| Project | Source calls found in the graph | Of which generic | Direct edges backed by a source call |
|---|---|---|---|
| go-sdk | 717 / 717 | 162 / 162 | 704 / 724, plus 20 explained, 0 unexplained |
| x/tools `./cmd/...` | 3833 / 3833 | 49 / 49 | 3814 / 3814 |

The 20 "extra" direct edges in go-sdk are calls to a method on a *type-parameter* value inside a generic function (the source calls an abstract method of the constraint; after instantiation it is a direct call) — correct, and a sign that instances fold onto the right methods.

The check found three real problems, all fixed: generated files in packages that are only *dependencies* were not filtered; methods of generic types that are never instantiated had no outgoing edges; and fixing the first one made a hand-written helper that is called only from generated code look unused, which led to the role `generated`. The first two have tests that were confirmed to fail when the fix is removed; the third is covered by a test but was not mutation-checked separately.

**Runtime check** (go-sdk's own tests, 739 functions instrumented, 3160 stacks):

| Algorithm | Directly adjacent | Via external | Generic | Edges |
|---|---|---|---|---|
| VTA (default) | 650 / 674 (96.4%) | 2 / 23 | 216 / 217 | 1422 |
| CHA | 674 / 674 (100%) | 4 / 23 | 217 / 217 | 2584 |

**VTA systematically misses interface dispatch in a library.** go-sdk is a library; the concrete types behind its interfaces are created by test code and handed in, so inside the library nothing ever passes a concrete type in and VTA's type flow has no candidate. VTA needs a closed world, and a library has none.

Changes: a **CHA fallback** used only where VTA resolves *nothing at all* (flag `cha-fallback`, hidden by default; a first version that fell back when VTA found no *own* candidate added 1318 edges on x/tools, mostly the old "own type implements an external interface" false positives, so it was narrowed); more methods external code calls implicitly (`UnmarshalJSON`, `Is` / `As` / `Unwrap`); values reached through exported fields, elements and interface-typed fields; own-interface values converted to `any`.

| go-sdk, final | |
|---|---|
| Directly adjacent | 671 / 674 (99.6%) |
| Via external code | 23 / 23 |
| Generic-related runtime calls | 217 / 217 |
| Executed functions labelled `unused` | 0 |
| Static check | 717 / 717 (generic 162 / 162) |

No regression on x/tools: 1640 / 1640 and 1545 / 1545 on the two run sets, 7 / 7 and 19 / 19 via external code, and 3833 / 3833 in the static check.

**Cost, and how it was bounded.** Implicit edges are over-approximations. They grew from 200 to 2095 on x/tools (+28% of all edges), though 77% pointed to functions that already have callers and change no role. Now no implicit edge is drawn to a function that is called by name, at most three are drawn to any other, and `implicit_callers` keeps the true count: 2095 → 165 implicit edges (−92%) and 9442 → 7512 edges on x/tools, 171 → 43 on go-sdk, with identical roles and identical recall.

## Overall limits

- Everything above is bounded by what was run: seven commands (about 22% of the functions) for the first runtime check, seven more for the hold-out, one test suite for go-sdk. Recall numbers are recall *of what ran*, and their confidence intervals are wide.
- Three direct calls in go-sdk are still missed: VTA resolved *part* of those call sites, so the fallback does not trigger. The call graph alone cannot reveal that.
- Calls are compared as caller→callee pairs, not per call site; `go` statements were not verified (a goroutine's first frame has no own caller).
- Not modelled: reflection by name outside templates, cgo exports, push-style `go:linkname`, dispatch through type-parameter constraints in never-instantiated generic code.
