# The three layers: raw → model → view

[English](architecture-layers.en.md) | [中文](architecture-layers.md)

```
Go source ──calltrail analyze──▶ raw.json ──calltrail annotate──▶ model.json ──calltrail view──▶ viewer.html
             (x/tools)             facts          + annotations + rules          one view      (later: archify.json …)
```

`calltrail run` produces all three outputs into one directory:

```bash
calltrail run -out-dir calltrail-out ./...                         # raw.json, model.json, viewer.html
calltrail run -rules calltrail.rules.json -out-dir calltrail-out   # with ignore rules
```

| Layer | File | Content | Principle |
|---|---|---|---|
| 1 Raw | `raw.json` ([internal/graph](../internal/graph/graph.go)) | The result of the x/tools analysis: packages, functions, external dependencies, edges (with `kind`, `flags`, call sites), roles | **Facts only, no decisions.** Drops no external dependency (`fmt` and `errors` are in), holds no presentation policy |
| 2 Model | `model.json` ([internal/model](../internal/model/model.go)) | A superset of the raw layer: annotations on every node and edge | **Only adds fields, never changes or removes a fact**; self-contained, the view layer reads only this |
| 3 View | `viewer.html` ([internal/render](../internal/render/render.go)) | Turns the model into the form a front end needs | Each view is a read-only transformation of the model; an Archify export would be a sibling |

## What the model layer adds

| Object | Added fields |
|---|---|
| Package | `module`, `internal` (some path element is `internal`), `tags` |
| Function | `module`, `internal`, `tags` (`role:…`, `exported`), `fan_in`, `hidden` / `hidden_by` |
| External dependency | `module` (`std` for the standard library, the module path for third-party), `tags` (`std`/`third`, `cat:db`…, `low-signal`), `fan_in`, `hidden` / `hidden_by` |
| Edge | `id` (stable, unique), `hidden` / `hidden_by` |
| Package / function / dependency | `reachable`, when an entry was given (see below) |
| Top level | `policy` (explains the hiding reasons; views build their switches from it), `rules` (the user's rules) |

### Built-in hiding policy (reasons)

Every node or edge hidden by default carries a reason, and a view offers one switch per reason:

| `hidden_by` | What it hides |
|---|---|
| `low-confidence` | Dynamic calls on external interfaces, table dispatch, edges that are only class-hierarchy guesses (the categories where wrong edges concentrated in the precision audit) |
| `implicit` | Edges meaning "may be called by external code" after an own type is converted to an external interface (an over-approximation that makes roles more accurate) |
| `low-signal` | Standard-library dependencies called from everywhere, such as `fmt`, `errors`, `strings`, `context`; and `init` nodes that touch only those |

## Ignore rules

A rule says only *what to ignore*; its fields are ANDed:

```json
{"rules": [
  {"module": "golang.org/x/sys"},
  {"package": "internal/gen/*"},
  {"tag": "internal"},
  {"function": "(*go/ssa.register).Type"},
  {"function": "internal/util.*"},
  {"package": "cmd", "tag": "internal", "note": "all fields given in one rule must match"}
]}
```

- **Matching**: a path prefix at element boundaries. `golang.org/x/tools` matches its sub-packages but not `golang.org/x/toolsfoo`; a trailing `*` means a plain prefix. For own packages `module` is the main module; the module of the standard library is `std`.
- **`function` rules** hide only functions (an exact id, or a prefix with a trailing `*`); they never hide the package or any dependency. `module` / `package` / `tag` rules reach functions through the package they belong to.
- **Effect**: a matched package is hidden together with its functions and every edge that touches them.
- **Rules are not written into `hidden`**; they travel with the model (`model.rules`) and are applied by whoever draws. In the browser they can be switched on and off, added and removed without re-running the analysis.
- **The matching is defined once**: `Rule.Matches*` in Go, mirrored one-to-one by the JavaScript in the page; both were checked against the same 16 cases and agree on all. On the Go side `Model.Visible()` applies the same rules for static exports (an Archify adapter would use it).

### Setting them in the page

The "Filter" panel at the top right has:
- a master switch for **external dependencies**;
- one switch per hiding reason, with counts;
- the list of ignore rules (tick to enable / disable; user rules can be deleted, rules from the rules file can only be disabled);
- adding a rule (module / package / function / tag) and two presets (ignore all `internal` packages, ignore the standard library);
- **Export rules** gives `calltrail.rules.json` to pass to the next `calltrail run -rules`; **Import rules** does the reverse.

The detail panel also has "ignore this package / dependency / function" buttons. A user's settings are kept per module in the browser (localStorage).

## Starting from an entry (`-entry`)

`meta.entry` in the raw layer records what the user typed; `meta.entries` holds the resolved entry-function ids. Resolution: a file → the package it belongs to, `main` if the file declares it, otherwise every function the file declares; a package → `main` for a main package, otherwise its exported functions; `path:name` → exactly that function. The raw layer is **not pruned**: it remains the complete set of facts for the entry's package and its dependencies.

"Reachable" is the model-layer annotation `reachable`: what can be reached from the entries along edges that are **not guesses** (low-confidence and implicit edges do not propagate, otherwise class-hierarchy guesses would pull unrelated code in). Package initialisation (the `init` nodes) runs before `main`, so it is a starting point too. A package is reachable when one of its functions is; an external dependency is reachable when a reachable function calls it. On the Go side use `ViewOptions.OnlyReachable`; without entries it is a no-op.

## Too many common things? Three ways to deal with it

Things called from everywhere, such as `fmt` and `errors`, can turn the graph into a blur. There are three levels of remedy, from effortless to fine-grained:

1. **The "Hide common dependencies" switch in the top bar** (on by default). It hides the standard-library dependencies of the built-in list (26 packages such as `fmt`, `errors`, `strings`, `context`, `log`, `flag`, plus `math/*` and `internal/*`; hover the switch to see the ones present in this result), that is, the `low-signal` reason. The graph draws dependencies at **package** level (not single functions like `fmt.Println`), so what is hidden is the whole package.
2. **Hide automatically by number of callers** (in "Filter"):
   - external dependencies: hide those called by more than N different functions;
   - own functions: hide those called by more than N different functions;
   - 0 means off. The count is `fan_in` in the model: how many different own functions call or reference it. **Guessed edges (low-confidence, implicit) do not count**; edges to low-signal dependencies do (they are real calls).
   - This is data-driven. The built-in list is fixed, while in x/tools `os` (158 callers), `path/filepath` (106) and `go/types` (408) are just as "everywhere" and are not on it.
3. **The "most called" list**: "Filter" lists the 8 most-called external dependencies and the 8 most-called own functions that are still shown; ✕ creates an ignore rule right away (a package or module for dependencies, a function id for functions).

The thresholds are off by default: what many functions call may be exactly what you care about (for example a database dependency called by every repository). Whether to hide it is your decision. On the Go side `ViewOptions{MaxFanInBoundary, MaxFanInFunction}` does the same for static exports.

## Commands

```
calltrail analyze  [-C dir] [-algo vta|cha|static] [-tags t] [-entry E] [-o raw.json] [patterns...]
calltrail annotate [-rules rules.json] [-o model.json] raw.json
calltrail view     [-rules rules.json] [-o viewer.html] model.json|raw.json
calltrail run      [-C dir] [-algo ...] [-tags t] [-entry E] [-rules rules.json] [-out-dir dir] [patterns...]
calltrail demo     ...  [-o out.html]        # only the html
```

`view` accepts `model.json` or `raw.json` (the latter is annotated first); `-rules` applies only to `raw.json` and is refused when the rules are already in a model, so that two sets of rules cannot fight each other.

## Design notes

- **The analyzer no longer drops low-signal dependencies.** The first version removed `fmt` and friends during analysis; that is a decision, and it does not belong in the raw layer. Cost: for x/tools the raw file grew from 2602 KB to about 3.0 MB, edges 7512 → 9060, boundaries 43 → 69; nearly all of the extra is `low-signal` and hidden by default. 20 more `init` nodes appeared that only touch things such as `flag`; the model layer hides them.
- **Presentation decisions live in the model layer.** "Which edges to fold by default" used to be hard-coded in the front end; it is now `hidden_by` in the model, and the front end builds its switches from `policy`.
- **Light mode**: see below.

## Light mode

It follows the system (`prefers-color-scheme`) by default and can be switched with the button at the top right; the choice is remembered in the browser, and until the user chooses, a change of the system theme is followed. In light mode a semantic colour keeps its meaning and is only darkened so it reads on white (Archify's theme-parity rule). Every colour, including arrowheads, the grid and group backgrounds, comes from a CSS variable.

## Not done yet

- **An Archify adapter** (the second view of the view layer): the interface is ready (read the model, filter with `Visible()`); the adapter itself is not written.
- **"Show only" rules** (an allow-list): there are only "ignore" rules.
- **gRPC / HTTP entry detection** and a "complete path of one endpoint" view: planned. Entry detection would arrive as a model-layer annotation (`entrypoint`).
- The rules a user sets in the page live only in that browser; to share them with a team, export them, commit the file and pass it with `-rules`.
- The page itself is in Chinese only.
