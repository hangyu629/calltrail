// Package model is the second layer: raw.json (package graph) plus annotations.
//
// The raw layer says what is in the code. This layer says what to make of it: which package is
// internal, which dependency is noise, which edge is a guess. It never changes or removes a raw
// fact; it only adds fields, so every consumer (viewer, exporter) reads one self-contained file.
//
// Hiding has two sources:
//
//   - the built-in policy, decided here and recorded per node / edge as Hidden + HiddenBy;
//   - rules (Rule): ignore a module, a package prefix or a tag. They are NOT baked into Hidden.
//     They travel in Model.Rules and are applied by whoever draws, so a user can switch them
//     on and off without re-running the analysis. Matching is defined once, here (Rule.Matches*),
//     and mirrored in the viewer.
package model

import (
	"strings"

	"github.com/hangyu629/calltrail/internal/graph"
)

const SchemaVersion = "model-0.1"

// Reasons for the built-in policy. A viewer offers one toggle per reason.
const (
	ReasonLowConfidence = "low-confidence"
	ReasonImplicit      = "implicit"
	ReasonLowSignal     = "low-signal"
)

// Policy documents the reasons, so viewers can label their toggles from the data.
var Policy = []PolicyReason{
	{
		ID: ReasonLowConfidence, Title: "低可信边", TitleEN: "Low-confidence edges",
		Description:   "外部接口上的动态调用、按 map/slice 取函数的调用、仅类层次推测的调用；精度抽查里误报集中在这几类",
		DescriptionEN: "Dynamic calls on external interfaces, calls through map/slice elements, and calls that are only class-hierarchy guesses; wrong edges concentrated in these categories in the precision audit",
	},
	{
		ID: ReasonImplicit, Title: "隐式边", TitleEN: "Implicit edges",
		Description:   "自有类型被转成外部接口（error、fmt.Stringer、any…）后，外部代码可能调用它的方法；过近似，用来让 role 更准",
		DescriptionEN: "After an own type is converted to an external interface (error, fmt.Stringer, any, ...), external code may call its methods; an over-approximation that makes roles more accurate",
	},
	{
		ID: ReasonLowSignal, Title: "低信号依赖", TitleEN: "Low-signal dependencies",
		Description:   "fmt、errors、strings、context… 到处都在调用，没有架构意义；只碰这些依赖的 init 也一并隐藏",
		DescriptionEN: "fmt, errors, strings, context, ... are called from everywhere and carry no architectural meaning; init code that touches only these is hidden as well",
	},
}

// PolicyReason documents one hiding reason, in Chinese and English, so a view can build its
// switches from the data in either language.
type PolicyReason struct {
	ID            string `json:"id"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	TitleEN       string `json:"title_en"`
	DescriptionEN string `json:"description_en"`
}

type Model struct {
	SchemaVersion string         `json:"schema_version"`
	RawSchema     string         `json:"raw_schema_version"`
	Meta          graph.Meta     `json:"meta"`
	Policy        []PolicyReason `json:"policy"`
	Rules         []Rule         `json:"rules"`
	Packages      []Package      `json:"packages"`
	Functions     []Function     `json:"functions"`
	Boundaries    []Boundary     `json:"boundaries"`
	Edges         []Edge         `json:"edges"`
}

type Package struct {
	graph.Package
	Module   string   `json:"module"`   // module the package belongs to
	Internal bool     `json:"internal"` // some path element is "internal"
	Tags     []string `json:"tags,omitempty"`
	// Reachable: some function of it is reachable from the entries (see Function.Reachable).
	Reachable bool `json:"reachable,omitempty"`
}

type Function struct {
	graph.Function
	Module   string   `json:"module"`
	Internal bool     `json:"internal"`
	Tags     []string `json:"tags,omitempty"`
	FanIn    int      `json:"fan_in"` // distinct own functions that call or reference it (guessed edges not counted)
	// Reachable: reachable from Meta.Entries (and from package initialisation, which runs before
	// main) along edges that are not guesses. Only set when entries were given.
	Reachable bool   `json:"reachable,omitempty"`
	Hidden    bool   `json:"hidden,omitempty"`
	HiddenBy  string `json:"hidden_by,omitempty"`
}

type Boundary struct {
	graph.Boundary
	Module string   `json:"module"` // "std" for the standard library, else the third-party module
	Tags   []string `json:"tags,omitempty"`
	FanIn  int      `json:"fan_in"` // distinct own functions that call it (guessed edges not counted)
	// Reachable: called by a reachable function (see Function.Reachable).
	Reachable bool   `json:"reachable,omitempty"`
	Hidden    bool   `json:"hidden,omitempty"`
	HiddenBy  string `json:"hidden_by,omitempty"`
}

type Edge struct {
	graph.Edge
	ID       string `json:"id"` // stable, unique
	Hidden   bool   `json:"hidden,omitempty"`
	HiddenBy string `json:"hidden_by,omitempty"`
}

// Annotate adds the annotations. rules are carried along unchanged (see the package comment).
func Annotate(g *graph.Graph, rules []Rule) *Model {
	m := &Model{
		SchemaVersion: SchemaVersion,
		RawSchema:     g.SchemaVersion,
		Meta:          g.Meta,
		Policy:        Policy,
		Rules:         append([]Rule{}, rules...),
	}
	if m.Rules == nil {
		m.Rules = []Rule{}
	}

	internalPkg := map[string]bool{}
	for _, p := range g.Packages {
		in := hasInternal(p.ID)
		internalPkg[p.ID] = in
		mp := Package{Package: p, Module: g.Meta.Module, Internal: in}
		if in {
			mp.Tags = append(mp.Tags, "internal")
		}
		if p.ID == "(root)" {
			mp.Tags = append(mp.Tags, "root")
		}
		m.Packages = append(m.Packages, mp)
	}

	boundaryHidden := map[string]bool{}
	for _, b := range g.Boundaries {
		mb := Boundary{Boundary: b, Module: b.Path}
		mb.Tags = []string{b.Kind, "cat:" + b.Category}
		if b.Kind == "std" {
			mb.Module = "std"
		}
		if LowSignal(b.Path) {
			mb.Tags = append(mb.Tags, "low-signal")
			mb.Hidden, mb.HiddenBy = true, ReasonLowSignal
			boundaryHidden[b.ID] = true
		}
		m.Boundaries = append(m.Boundaries, mb)
	}

	// why an edge is hidden by the built-in policy ("" = drawn)
	reason := func(e graph.Edge) string {
		switch {
		case boundaryHidden[e.To]:
			return ReasonLowSignal
		case has(e.Flags, "implicit-iface"):
			return ReasonImplicit
		case has(e.Flags, "external-iface") || has(e.Flags, "table") || has(e.Flags, "cha-fallback"):
			return ReasonLowConfidence
		}
		return ""
	}

	// fan-in: how many different own functions call / reference a node. Guessed edges (low-confidence,
	// implicit) do not count; low-signal ones do, they are real calls.
	callers := map[string]map[string]bool{}
	for _, e := range g.Edges {
		if r := reason(e); r == ReasonLowConfidence || r == ReasonImplicit || e.From == e.To {
			continue
		}
		if callers[e.To] == nil {
			callers[e.To] = map[string]bool{}
		}
		callers[e.To][e.From] = true
	}
	for i := range m.Boundaries {
		m.Boundaries[i].FanIn = len(callers[m.Boundaries[i].ID])
	}

	// edges
	visibleOut := map[string]int{} // function id -> number of edges that stay visible
	for _, e := range g.Edges {
		me := Edge{Edge: e, ID: e.From + " -> " + e.To + "|" + e.Kind + "|" + e.Mode}
		if r := reason(e); r != "" {
			me.Hidden, me.HiddenBy = true, r
		}
		if !me.Hidden {
			visibleOut[e.From]++
			visibleOut[e.To]++
		}
		m.Edges = append(m.Edges, me)
	}

	// reachability from the entries; package initialisation runs before main, so init nodes are roots too
	reached := map[string]bool{}
	if len(g.Meta.Entries) > 0 {
		next := map[string][]string{}
		for _, e := range g.Edges {
			if r := reason(e); r == ReasonLowConfidence || r == ReasonImplicit {
				continue // a guess does not make anything reachable
			}
			next[e.From] = append(next[e.From], e.To)
		}
		queue := append([]string{}, g.Meta.Entries...)
		for _, f := range g.Functions {
			if f.Role == "init" {
				queue = append(queue, f.ID)
			}
		}
		for len(queue) > 0 {
			u := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			if reached[u] {
				continue
			}
			reached[u] = true
			queue = append(queue, next[u]...)
		}
		for i := range m.Boundaries {
			m.Boundaries[i].Reachable = reached[m.Boundaries[i].ID]
		}
	}
	pkgReached := map[string]bool{}
	for _, f := range g.Functions {
		if reached[f.ID] {
			pkgReached[f.Package] = true
		}
	}
	for i := range m.Packages {
		m.Packages[i].Reachable = pkgReached[m.Packages[i].ID]
	}

	for _, f := range g.Functions {
		mf := Function{Function: f, Reachable: reached[f.ID], Module: g.Meta.Module, Internal: internalPkg[f.Package], FanIn: len(callers[f.ID])}
		if f.Role != "" {
			mf.Tags = append(mf.Tags, "role:"+f.Role)
		}
		if f.Exported {
			mf.Tags = append(mf.Tags, "exported")
		}
		// a package-init node that only touches hidden dependencies has nothing left to show
		if f.Role == "init" && visibleOut[f.ID] == 0 {
			mf.Hidden, mf.HiddenBy = true, ReasonLowSignal
		}
		m.Functions = append(m.Functions, mf)
	}
	return m
}

func has(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hasInternal(path string) bool {
	for _, el := range strings.Split(path, "/") {
		if el == "internal" {
			return true
		}
	}
	return false
}

// lowSignal std packages are called from everywhere and carry no architectural meaning.
var lowSignalExact = map[string]bool{
	"fmt": true, "errors": true, "strings": true, "strconv": true, "sort": true,
	"context": true, "sync": true, "sync/atomic": true, "time": true, "bytes": true,
	"reflect": true, "slices": true, "maps": true, "log": true, "log/slog": true,
	"unicode": true, "unicode/utf8": true, "math": true, "cmp": true, "iter": true,
	"unsafe": true, "runtime": true, "regexp": true, "flag": true, "text/tabwriter": true,
	"io": true,
}

// LowSignal reports whether an import path is a "noise" dependency under the built-in policy.
func LowSignal(p string) bool {
	return lowSignalExact[p] || strings.HasPrefix(p, "math/") || strings.HasPrefix(p, "internal/")
}

// ---- rules -----------------------------------------------------------------

// A Rule hides the nodes it matches (and every edge touching them). Fields set together must all
// match. Matching is by path prefix at an element boundary: "golang.org/x/tools" matches
// "golang.org/x/tools/go/ssa" but not "golang.org/x/toolsfoo"; a trailing "*" is a plain prefix.
type Rule struct {
	ID       string `json:"id,omitempty"`
	Module   string `json:"module,omitempty"`   // module path ("std" for the standard library)
	Package  string `json:"package,omitempty"`  // package id (own) or import path (external)
	Function string `json:"function,omitempty"` // function id; a rule with this set applies to functions only
	Tag      string `json:"tag,omitempty"`
	Note     string `json:"note,omitempty"`
}

func (r Rule) Valid() bool {
	return r.Module != "" || r.Package != "" || r.Function != "" || r.Tag != ""
}

func pathMatch(pattern, s string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(s, p)
	}
	return s == pattern || strings.HasPrefix(s, pattern+"/")
}

func (r Rule) matches(path, module string, tags []string) bool {
	if !r.Valid() || r.Function != "" { // function rules never match packages or dependencies
		return false
	}
	if r.Module != "" && !pathMatch(r.Module, module) {
		return false
	}
	if r.Package != "" && !pathMatch(r.Package, path) {
		return false
	}
	return r.Tag == "" || has(tags, r.Tag)
}

func (r Rule) MatchesPackage(p Package) bool { return r.matches(p.ID, p.Module, p.Tags) }

// MatchesFunction applies only to rules that name a function (a rule on a package or module hides
// its functions through the package already). The id matches exactly, or as a prefix with a trailing *.
func (r Rule) MatchesFunction(f Function) bool {
	if r.Function == "" {
		return false
	}
	if p, ok := strings.CutSuffix(r.Function, "*"); ok {
		if !strings.HasPrefix(f.ID, p) {
			return false
		}
	} else if f.ID != r.Function {
		return false
	}
	if r.Package != "" && !pathMatch(r.Package, f.Package) {
		return false
	}
	if r.Module != "" && !pathMatch(r.Module, f.Module) {
		return false
	}
	return r.Tag == "" || has(f.Tags, r.Tag)
}
func (r Rule) MatchesBoundary(b Boundary) bool { return r.matches(b.Path, b.Module, b.Tags) }

// ---- applying (for static exports; the viewer does the same in the browser) -------------------

// ViewOptions say what to draw: which built-in reasons to show anyway, and which rules to apply.
type ViewOptions struct {
	Show  map[string]bool // reason -> show the nodes / edges hidden for that reason
	Rules []Rule
	// Hide what many functions call ("common" things), by fan-in. 0 = off.
	MaxFanInBoundary int // external dependencies called by more than this many own functions
	MaxFanInFunction int // own functions called by more than this many own functions
	// OnlyReachable keeps what the entries (Meta.Entries) can reach; no effect without entries.
	OnlyReachable bool
}

// Visible returns a copy of the model with hidden things removed. Edges that lose an endpoint
// are removed too. The input is not modified.
func (m *Model) Visible(o ViewOptions) *Model {
	out := *m
	out.Packages, out.Functions, out.Boundaries, out.Edges = nil, nil, nil, nil

	onlyReach := o.OnlyReachable && len(m.Meta.Entries) > 0
	hiddenPkg := map[string]bool{}
	for _, p := range m.Packages {
		if anyPkgRule(o.Rules, p) || (onlyReach && !p.Reachable) {
			hiddenPkg[p.ID] = true
			continue
		}
		out.Packages = append(out.Packages, p)
	}
	alive := map[string]bool{}
	for _, f := range m.Functions {
		if hiddenPkg[f.Package] || (f.Hidden && !o.Show[f.HiddenBy]) || anyFuncRule(o.Rules, f) ||
			(o.MaxFanInFunction > 0 && f.FanIn > o.MaxFanInFunction) || (onlyReach && !f.Reachable) {
			continue
		}
		alive[f.ID] = true
		out.Functions = append(out.Functions, f)
	}
	for _, b := range m.Boundaries {
		if (b.Hidden && !o.Show[b.HiddenBy]) || anyBoundaryRule(o.Rules, b) ||
			(o.MaxFanInBoundary > 0 && b.FanIn > o.MaxFanInBoundary) || (onlyReach && !b.Reachable) {
			continue
		}
		alive[b.ID] = true
		out.Boundaries = append(out.Boundaries, b)
	}
	for _, e := range m.Edges {
		if (e.Hidden && !o.Show[e.HiddenBy]) || !alive[e.From] || !alive[e.To] {
			continue
		}
		out.Edges = append(out.Edges, e)
	}
	return &out
}

func anyPkgRule(rs []Rule, p Package) bool {
	for _, r := range rs {
		if r.MatchesPackage(p) {
			return true
		}
	}
	return false
}

func anyFuncRule(rs []Rule, f Function) bool {
	for _, r := range rs {
		if r.MatchesFunction(f) {
			return true
		}
	}
	return false
}

func anyBoundaryRule(rs []Rule, b Boundary) bool {
	for _, r := range rs {
		if r.MatchesBoundary(b) {
			return true
		}
	}
	return false
}

// Counts summarises what is hidden by the built-in policy, by reason (for status lines).
func (m *Model) Counts() map[string]int {
	c := map[string]int{}
	for _, e := range m.Edges {
		if e.Hidden {
			c[e.HiddenBy]++
		}
	}
	return c
}
