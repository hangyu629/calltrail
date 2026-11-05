# Architecture

[English](architecture.en.md) | [中文](architecture.md)

calltrail extracts the call relations of Go code *mechanically*, organises them in three layers of output, and browses them in a self-contained web page. The diagrams below are [Mermaid](https://mermaid.js.org/); GitHub renders them directly.

Contents

1. [Design principles](#design-principles)
2. [Overview: three layers of output](#overview-three-layers-of-output)
3. [Layer 1: how the analyzer produces raw.json](#layer-1-how-the-analyzer-produces-rawjson)
4. [Data model: raw and model](#data-model-raw-and-model)
5. [Layer 2: what the model layer does](#layer-2-what-the-model-layer-does)
6. [Layer 3: how the front end draws](#layer-3-how-the-front-end-draws)
7. [What happens in one `calltrail run`](#what-happens-in-one-calltrail-run)
8. [Verification tools](#verification-tools)
9. [Package layout](#package-layout)

## Design principles

1. **Structure comes from a program, not from a model.** Nodes and edges come only from compiler-grade analysis (`go/packages` + SSA + a call-graph algorithm).
2. **Every assertion has evidence.** Every edge carries its call sites (`file:line`); uncertain edges carry a flag instead of being silently dropped or passed off as certain.
3. **Facts and decisions are separate layers.** The raw layer holds facts only; "what is noise, what to hide first, what the user wants to ignore" belongs to layer 2; how to draw belongs to layer 3.
4. **Deterministic.** The same code and flags give identical output (stable IDs, stable ordering, a fixed random seed for layout).
5. **Few and precise by default.** No full-graph dump: start from a package overview or focus from an entry; details on demand.
6. **Mark what is uncertain.** Interface dispatch, reflection, `go:linkname` and other blind spots of static analysis are visible in the graph and have been quantified with runtime checks.

## Overview: three layers of output

```mermaid
flowchart TB
    SRC["Go source (one module)"]
    ENTRY["-entry<br/>file / package / function"]
    RULES["rules.json<br/>ignore rules"]

    subgraph L1["Layer 1 · analyzer · internal/analyze"]
        A["go/packages load → SSA → call graph (CHA / VTA)<br/>keep own code only, fold external calls into dependency nodes"]
    end
    RAW[("raw.json<br/>facts")]

    subgraph L2["Layer 2 · model · internal/model"]
        M["annotations: module / internal / tags / fan-in / reachability<br/>built-in hiding policy (each item has a reason)"]
    end
    MODEL[("model.json<br/>facts + annotations + rules")]

    subgraph L3["Layer 3 · view · internal/render"]
        V["built-in front end: ELK layout + SVG<br/>focus / filter / dark and light"]
    end
    HTML[("viewer.html<br/>self-contained page")]
    ARCH["Archify and other views<br/>(planned)"]

    SRC --> A
    ENTRY -.-> A
    A --> RAW
    RAW --> M
    RULES -.-> M
    M --> MODEL
    MODEL --> V
    V --> HTML
    MODEL -.-> ARCH
```

| Layer | File | Implementation | Does only this |
|---|---|---|---|
| 1 | `raw.json` | [internal/analyze](../internal/analyze), [internal/graph](../internal/graph) | Records what **is** in the code: packages, functions, external dependencies, edges (with call sites), roles. Drops no external dependency, holds no presentation policy |
| 2 | `model.json` | [internal/model](../internal/model) | **Only adds fields** to the raw layer: module, internal?, fan-in, reachability, hidden by default and why; carries the user's rules. Never changes or removes a fact |
| 3 | `viewer.html` | [internal/render](../internal/render) | Turns the model into a page to browse. Every view is a read-only transformation of the model |

Only JSON files connect the layers, so the analysis (the slow part) runs once; changing rules or views reruns only the later layers.

## Layer 1: how the analyzer produces raw.json

```mermaid
flowchart TD
    P1["1 · Load<br/>go/packages: type info, dependencies, module<br/>detect generated files (every package of the module)"]
    P2["2 · SSA and call graph<br/>build SSA with generics instantiated<br/>algorithm: static / CHA / VTA (initial graph = CHA)<br/>remove synthetic nodes"]
    P3["3 · Own functions<br/>closures fold into the outer function, generic instances fold into the origin<br/>init code merges into pkg.init, generated code is dropped"]
    P4["4 · Classify every call site<br/>(see the next diagram)"]
    P5["5 · Aggregate<br/>a flag holds only if it holds at every call site<br/>implicit edges are bounded"]
    P6["6 · Finish<br/>roles / init nodes / external deps merged per module<br/>resolve -entry into meta.entries"]
    OUT[("raw.json")]

    P1 --> P2 --> P3 --> P4 --> P5 --> P6 --> OUT
```

### What one call site becomes

```mermaid
flowchart LR
    SITE["one call site"]
    OWN["calls an own function<br/>direct: statically known<br/>dynamic: interface / function value, with candidate count<br/>callback: calls a closure defined elsewhere<br/>cha-fallback: VTA resolved nothing, class hierarchy used instead"]
    EXT["calls an external package<br/>boundary: classified by the call site<br/>(not by the resolved implementation)"]
    ESC["hands an own value to external code<br/>registers: a function value passed to an external function<br/>registers + reflect: given to text/template<br/>registers + implicit-iface: converted to an external interface / any"]
    LINK["go:linkname<br/>direct + linkname: a body-less declaration<br/>linked to the real function"]

    SITE --> OWN
    SITE --> EXT
    SITE --> ESC
    SITE --> LINK
```

The **flags** on an edge say how far to trust it:

| Flag | Meaning |
|---|---|
| `external-iface` | A dynamic call on an interface declared outside the module (such as `types.Type` or `error`); an own type merely happens to implement it. Wrong edges concentrated here in the precision audit |
| `table` | The callee is taken from a map / slice element; at run time only one of the candidates runs |
| `cha-fallback` | VTA resolved no candidate at all; every implementation from class-hierarchy analysis is used instead (common in libraries) |
| `implicit-iface` | After an own type is converted to an external interface, external code may call its methods (an over-approximation) |
| `reflect` | An own value handed to a template, which calls its exported methods by name |
| `linkname` | A body-less declaration linked by `//go:linkname` |

A function's **role** says why it has (or lacks) callers: `entry`, `init`, `registered`, `implicit`, `generated`, `iface`, `api`, `referenced`, `unused`; empty means some function calls it. See the comments in [graph.go](../internal/graph/graph.go).

## Data model: raw and model

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
    Function "*" --> "1" Package : belongs to
    Edge "*" --> "1" Function : from
    Edge "*" --> "1" Function : to (or an external dependency)
    Model --|> Graph : superset (adds fields)
    Model ..> Annotations : added to every node and edge
```

- **Function IDs** are package-qualified names such as `(*internal/order.Service).Create`, without line numbers, so an ID survives code moving around and two results can be diffed.
- **External dependencies**: standard-library packages stay separate; third-party packages merge into one node per module.
- **Edge `kind`**: `direct`, `dynamic`, `registers`, `callback`, `boundary`. **`mode`**: `call`, `go`, `defer`.

## Layer 2: what the model layer does

```mermaid
flowchart LR
    RAW[("raw.json")]
    ANN["Annotate<br/><br/>packages / functions: module, internal, tags<br/>external deps: std or third-party module, category, low-signal tag<br/>fan-in: number of distinct own callers (guessed edges do not count)<br/>reachability: from the entries and init, along non-guessed edges<br/>hidden by default: one reason per node / edge"]
    RULEFILE["rules.json<br/>(optional)"]
    MODEL[("model.json")]
    VIS["Visible(opts)<br/>for static exports: reason switches / rules /<br/>fan-in thresholds / reachable only"]

    RAW --> ANN
    RULEFILE -.-> MODEL
    ANN --> MODEL
    MODEL --> VIS
```

### Two sources of hiding

| Source | Decided by | Represented as |
|---|---|---|
| **Built-in policy** | the model layer | Written to `hidden` / `hidden_by`, with three reasons: `low-confidence` (low-confidence edges), `implicit` (implicit edges), `low-signal` (common dependencies such as `fmt` and `errors`) |
| **Rules** | the user | **Not** written to `hidden`; they travel in `model.rules` and are applied by whoever draws. In the browser they can be switched on and off, added and removed without re-running the analysis |

Rules match a path prefix at element boundaries: `golang.org/x/tools` matches its sub-packages but not `golang.org/x/toolsfoo`; a trailing `*` means a plain prefix; the fields of one rule must all match. The matching is implemented once in Go (`Rule.Matches*`) and once in the front end, and both were checked against the same set of cases.

## Layer 3: how the front end draws

```mermaid
flowchart TD
    M[("model.json<br/>embedded in viewer.html")]
    LS[("browser localStorage<br/>rules / thresholds / theme")]
    ST["state<br/>mode / expanded packages / selection / focus"]
    HIDE["decide what to draw<br/>built-in hiding reasons (one switch each)<br/>user rules: module / package / function / tag<br/>fan-in thresholds: external deps / own functions<br/>reachability from the entry"]
    FOCUS["focus: BFS from one node<br/>downstream N levels or all, upstream M levels"]
    BUILD["build: nodes + aggregated edges, packages are nested groups"]
    ELK["ELK layout: layered + orthogonal routing<br/>asynchronous, cached per state"]
    SVG["SVG rendering<br/>CSS variables decide dark / light"]
    PANEL["side panel and filter panel<br/>details / call sites / callers / search / rules"]

    M --> ST
    LS <--> ST
    ST --> HIDE
    HIDE --> FOCUS
    FOCUS --> BUILD
    BUILD --> ELK
    ELK --> SVG
    SVG -->|"select / expand / double-click"| ST
    ST --> PANEL
    PANEL -->|"add a rule / change a threshold"| ST
```

Notes:
- **The layout is cached**: it is recomputed only when the drawn graph changes (expand, focus, hiding conditions); selecting and searching never re-layout.
- **The layout is deterministic**: ELK runs with a fixed random seed, so the same input gives the same layout.
- **Dark / light**: every colour comes from a CSS variable and node colours are resolved at draw time, so switching theme redraws without re-layout.
- **Large-graph guard**: ELK blocks the page when there are many nodes, so the page says so before laying out and suggests focus mode.

## What happens in one `calltrail run`

```mermaid
sequenceDiagram
    actor U as User
    participant CLI as cmd/calltrail
    participant AN as internal/analyze
    participant MO as internal/model
    participant RE as internal/render
    participant FS as output directory

    U->>CLI: calltrail run -C project -entry E -rules R -out-dir D
    CLI->>MO: LoadRules (if -rules was given)
    CLI->>AN: Run(dir, patterns or entry, tags, algorithm)
    AN-->>CLI: Graph (with meta.entries)
    CLI->>MO: Annotate(Graph, rules)
    MO-->>CLI: Model
    CLI->>FS: write raw.json
    CLI->>FS: write model.json
    CLI->>RE: HTML(Model)
    RE-->>CLI: one html with ELK and the model embedded
    CLI->>FS: write viewer.html
    CLI-->>U: print the resolved entries and statistics
```

## Verification tools

The analyzer's results are checked in two ways that are **independent of the call-graph algorithm**; the tools are in [tools/recall](../tools/recall):

```mermaid
flowchart LR
    GRAPH[("raw.json")]

    subgraph STATIC["Static check: direct"]
        SA["read every statically resolvable call<br/>from the syntax tree (go/types)"]
        SB["check both ways<br/>is each source call in the graph?<br/>does each direct edge have a source call?"]
        SA --> SB
    end

    subgraph RUNTIME["Runtime check: instrument + compare.py"]
        RA["instrument the entry of every function of the target"]
        RB["run it for real (tests / commands)<br/>record the call stack of each call site"]
        RC["map to function IDs of the graph<br/>nearest own caller → callee"]
        RD["recall: is the edge in the graph?<br/>is a function that ran labelled unused?"]
        RA --> RB --> RC --> RD
    end

    GRAPH --> SB
    GRAPH --> RD
```

- The **static check** finds: missed direct calls, extra direct edges, whether generic instances fold correctly.
- The **runtime check** finds the blind spots of static analysis: interface dispatch, callbacks, reflection, initialisation.
- A summary with the numbers: [validation.en.md](validation.en.md). The detailed records (Chinese): [precision-audit](precision-audit-2026-11-05.md) (sampling extra edges), [recall-check](recall-check-2026-11-05.md) (finding missed edges with runtime call stacks), [generics-validation](generics-validation-2026-11-05.md) (a real generics-heavy project).

## Package layout

```mermaid
flowchart TD
    CMD["cmd/calltrail<br/>command line"]
    AN["internal/analyze<br/>layer 1: analyzer"]
    GR["internal/graph<br/>schema of raw.json"]
    MO["internal/model<br/>layer 2: annotations / rules / filtering"]
    RE["internal/render<br/>layer 3: front end (embeds elkjs)"]
    TOOLS["tools/recall<br/>verification tools"]

    CMD --> AN
    CMD --> MO
    CMD --> RE
    AN --> GR
    MO --> GR
    RE --> MO
    TOOLS --> GR
```

| Path | Content |
|---|---|
| `internal/analyze` | `analyze.go` main flow; `implicit.go` implicit edges / init / linkname; `ifaces.go` interfaces and generics; `entry.go` entry resolution; `classify.go` helpers |
| `internal/graph` | The schema and stable ordering |
| `internal/model` | `model.go` annotations and filtering; `rules.go` the rules file |
| `internal/render` | `viewer.html` (the front end, one file); `render.go` embedding and filling in; `elk.bundled.js` ([elkjs](https://github.com/kieler/elkjs) layout engine, EPL-2.0, see [ELK-LICENSE.md](../internal/render/ELK-LICENSE.md)) |
| `tools/recall` | `instrument` instrumentation; `compare.py` runtime check; `direct` static check |
