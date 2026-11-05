// Package analyze turns a Go module into a graph.Graph.
//
// Pipeline: go/packages -> SSA -> call graph (VTA by default) -> keep only the
// module's own code, fold everything else into boundary nodes.
package analyze

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/static"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/hangyu629/calltrail/internal/graph"
)

// maxImplicitEdges bounds how many implicit-iface edges are drawn to one function.
const maxImplicitEdges = 3

type Options struct {
	Dir      string   // module directory
	Patterns []string // default ./...
	Tags     string
	Algo     string // vta (default) | cha | static
	// Entry says where the program starts (see entrySpec). It selects the package to analyse, so it
	// replaces Patterns; the entry functions are recorded in Meta.Entries.
	Entry string
}

type analyzer struct {
	prog    *ssa.Program
	modPath string
	modDir  string
	gen     map[string]bool // generated files (absolute)

	edges  map[ekey]*eacc
	bounds map[string]map[string]bool // boundary key (std pkg path or third-party module) -> import paths seen
	modOf  map[string]string          // third-party import path -> its module path

	refd       map[string]bool   // functions used as values (not just called)
	initPkgs   map[string]string // "<pkg>.init" node id -> import path
	links      []link
	genCalled  map[string]bool        // functions called by generated code (which is not drawn)
	ifaces     map[string][]ifaceDecl // interface methods by name
	typeInst   map[*types.TypeName][]*types.Named
	cha        *callgraph.Graph // class hierarchy graph: the fallback where VTA resolves nothing
	rtypes     []types.Type     // own concrete types converted to an interface somewhere
	reachCache map[types.Type][]*types.Named
}

type ekey struct{ from, to, kind, mode string }
type eacc struct {
	sites  map[string]map[string]bool // site -> flags that hold at that site
	cand   int
	ifaces map[string]bool
}

// siteInfo describes how precise a dynamic call site is.
type siteInfo struct {
	flags []string
	iface string
}

func Run(o Options) (*graph.Graph, error) {
	if o.Algo == "" {
		o.Algo = "vta"
	}
	var entry *entrySpec
	if o.Entry != "" {
		if len(o.Patterns) > 0 {
			return nil, fmt.Errorf("give either package patterns or -entry, not both (-entry selects the package)")
		}
		abs, err := filepath.Abs(o.Dir)
		if err != nil {
			return nil, err
		}
		e, err := parseEntry(abs, o.Entry)
		if err != nil {
			return nil, err
		}
		entry = &e
		o.Patterns = []string{e.pattern}
	}
	if len(o.Patterns) == 0 {
		o.Patterns = []string{"./..."}
	}
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedSyntax |
			packages.NeedTypesInfo | packages.NeedTypesSizes | packages.NeedModule,
		Dir: o.Dir,
	}
	if o.Tags != "" {
		cfg.BuildFlags = []string{"-tags=" + o.Tags}
	}
	pkgs, err := packages.Load(cfg, o.Patterns...)
	if err != nil {
		return nil, err
	}
	var errs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		if len(errs) > 5 {
			errs = append(errs[:5], fmt.Sprintf("... and %d more", len(errs)-5))
		}
		return nil, fmt.Errorf("package load errors:\n  %s", strings.Join(errs, "\n  "))
	}

	a := &analyzer{gen: map[string]bool{}, edges: map[ekey]*eacc{}, bounds: map[string]map[string]bool{}, modOf: map[string]string{}, refd: map[string]bool{}, genCalled: map[string]bool{}, initPkgs: map[string]string{}}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Main {
			a.modPath, a.modDir = p.Module.Path, p.Module.Dir
			break
		}
	}
	if a.modPath == "" {
		return nil, fmt.Errorf("no main module found under %q", o.Dir)
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.Module != nil && !p.Module.Main {
			a.modOf[p.PkgPath] = p.Module.Path
		}
	})
	// generated files, in every package of the module (dependencies of the pattern packages too)
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.Module == nil || !p.Module.Main {
			return
		}
		for _, f := range p.Syntax {
			if ast.IsGenerated(f) {
				a.gen[p.Fset.Position(f.FileStart).Filename] = true
			}
		}
	})

	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	a.prog = prog

	var cg *callgraph.Graph
	allFuncs := ssautil.AllFunctions(prog)
	switch o.Algo {
	case "static":
		cg = static.CallGraph(prog)
	case "cha":
		cg = cha.CallGraph(prog)
	case "vta":
		a.cha = cha.CallGraph(prog)
		cg = vta.CallGraph(allFuncs, a.cha)
	default:
		return nil, fmt.Errorf("unknown algo %q", o.Algo)
	}
	cg.DeleteSyntheticNodes() // bridge over wrappers/thunks so edges reach real functions

	// own functions (closures folded into their outer function)
	ownFns := map[*ssa.Function]bool{}
	ids := map[string]bool{}
	for fn := range allFuncs {
		if c := a.canon(fn); a.own(c) {
			ownFns[c] = true
		}
	}
	// Methods of generic types only exist as instances, and only for instantiations the SSA
	// builder reached. List the declared (generic) methods too, so none silently disappears.
	// Those nobody instantiated have no call graph node; their static calls are read directly.
	var orphans []*ssa.Function
	for _, p := range prog.AllPackages() {
		if !a.inMod(p.Pkg.Path()) {
			continue
		}
		sc := p.Pkg.Scope()
		for _, nm := range sc.Names() {
			tn, ok := sc.Lookup(nm).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			if named, ok := tn.Type().(*types.Named); ok && named.TypeParams().Len() > 0 {
				for i := 0; i < named.NumMethods(); i++ {
					if f := prog.FuncValue(named.Method(i)); f != nil {
						if c := a.canon(f); a.own(c) {
							ownFns[c] = true
							if cg.Nodes[c] == nil && len(c.Blocks) > 0 {
								orphans = append(orphans, c)
							}
						}
					}
				}
			}
		}
	}
	for fn := range ownFns {
		ids[a.id(fn)] = true
	}
	a.scanLinknames(pkgs)
	a.indexInterfaces(pkgs)

	nodes := make([]*callgraph.Node, 0, len(cg.Nodes))
	for _, n := range cg.Nodes {
		if n.Func != nil {
			nodes = append(nodes, n)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	for _, n := range nodes {
		a.visit(n)
	}
	sort.Slice(orphans, func(i, j int) bool { return a.id(orphans[i]) < a.id(orphans[j]) })
	for _, f := range orphans {
		a.visit(&callgraph.Node{Func: f})
	}
	for _, l := range a.links {
		if ids[l.from] && ids[l.to] {
			a.add(l.from, l.to, "direct", "call", l.site, 0, siteInfo{flags: []string{"linkname"}})
		}
	}

	g := &graph.Graph{
		SchemaVersion: graph.SchemaVersion,
		Meta: graph.Meta{
			Module: a.modPath, Algo: o.Algo, GoVersion: runtime.Version(),
			Patterns: o.Patterns, Tags: o.Tags,
			TotalFunctions: len(allFuncs), OwnFunctions: len(ownFns),
		},
	}

	pkgSeen := map[string]bool{}
	incoming := map[string]map[string]bool{}        // fn id -> kinds of explicit incoming edges
	implicitIn := map[string]bool{}                 // fn id -> reached only by implicit/reflective edges
	var implicitEdges []graph.Edge                  // implicit-iface edges, thinned out below
	implicitCallers := map[string]map[string]bool{} // fn id -> functions that may hand it to external code
	hasFlag := func(fl []string, f string) bool {
		for _, x := range fl {
			if x == f {
				return true
			}
		}
		return false
	}
	for k, v := range a.edges {
		e := graph.Edge{From: k.from, To: k.to, Kind: k.kind, Mode: k.mode, Candidates: v.cand}
		flagCount := map[string]int{}
		for s, fl := range v.sites {
			e.Sites = append(e.Sites, s)
			for f := range fl {
				flagCount[f]++
			}
		}
		for f, n := range flagCount {
			if n == len(v.sites) { // only if it holds at every site
				e.Flags = append(e.Flags, f)
			}
		}
		sort.Strings(e.Flags)
		for i := range v.ifaces {
			e.Ifaces = append(e.Ifaces, i)
		}
		sort.Strings(e.Ifaces)
		if len(e.Ifaces) > 5 {
			e.Ifaces = e.Ifaces[:5]
		}
		sort.Strings(e.Sites)
		if len(e.Sites) > 12 {
			e.Sites = e.Sites[:12]
		}
		if k.kind == "registers" && hasFlag(e.Flags, "implicit-iface") {
			// decided below, once every function's explicit callers are known
			implicitEdges = append(implicitEdges, e)
			if implicitCallers[k.to] == nil {
				implicitCallers[k.to] = map[string]bool{}
			}
			implicitCallers[k.to][k.from] = true
			implicitIn[k.to] = true
			continue
		}
		g.Edges = append(g.Edges, e)
		if (k.kind == "registers" && hasFlag(e.Flags, "reflect")) || hasFlag(e.Flags, "cha-fallback") {
			implicitIn[k.to] = true
			continue
		}
		if incoming[k.to] == nil {
			incoming[k.to] = map[string]bool{}
		}
		incoming[k.to][k.kind] = true
	}

	// Implicit edges are over-approximate and numerous (x/tools: 22% of all edges, 77% of them to
	// functions that already have callers). They matter for a function's role, not for the picture, so:
	// draw none to a function that is called by name anyway, and at most maxImplicitEdges to the rest.
	// Function.ImplicitCallers keeps the true count.
	sort.Slice(implicitEdges, func(i, j int) bool {
		if implicitEdges[i].To != implicitEdges[j].To {
			return implicitEdges[i].To < implicitEdges[j].To
		}
		return implicitEdges[i].From < implicitEdges[j].From
	})
	kept := map[string]int{}
	for _, e := range implicitEdges {
		if len(incoming[e.To]) > 0 || kept[e.To] >= maxImplicitEdges {
			continue
		}
		kept[e.To]++
		g.Edges = append(g.Edges, e)
	}

	addPkg := func(pk, path string) {
		if !pkgSeen[pk] {
			pkgSeen[pk] = true
			g.Packages = append(g.Packages, graph.Package{ID: pk, Path: path, Kind: "own"})
		}
	}
	for fn := range ownFns {
		id := a.id(fn)
		pk := a.short(fn.Pkg.Pkg.Path())
		f := graph.Function{
			ID: id, Label: a.label(fn), Package: pk, Pos: a.pos(fn.Pos()),
			Exported: ast.IsExported(fn.Name()),
		}
		if r := fn.Signature.Recv(); r != nil {
			f.Implements = a.implementsOf(r.Type(), fn.Name())
		}
		f.ImplicitCallers = len(implicitCallers[id])
		switch {
		case fn.Pkg.Pkg.Name() == "main" && fn.Name() == "main":
			f.Role = "entry"
		case incoming[id]["registers"]:
			f.Role = "registered"
		case len(incoming[id]) > 0:
			// called by someone: no role
		case implicitIn[id]:
			f.Role = "implicit"
		case a.genCalled[id]:
			f.Role = "generated"
		case len(f.Implements) > 0:
			f.Role = "iface"
		case f.Exported:
			f.Role = "api"
		case a.refd[id]:
			f.Role = "referenced"
		default:
			f.Role = "unused"
		}
		g.Functions = append(g.Functions, f)
		addPkg(pk, fn.Pkg.Pkg.Path())
	}
	// package initialisation nodes (only where they do something we model)
	initSeen := map[string]bool{}
	for k := range a.edges {
		if _, ok := a.initPkgs[k.from]; ok {
			initSeen[k.from] = true
		}
	}
	for id := range initSeen {
		pp := a.initPkgs[id]
		pk := a.short(pp)
		g.Functions = append(g.Functions, graph.Function{ID: id, Label: "init", Package: pk, Role: "init"})
		addPkg(pk, pp)
	}
	for key, ps := range a.bounds {
		kind := "third"
		if !strings.Contains(strings.SplitN(key, "/", 2)[0], ".") {
			kind = "std"
		}
		b := graph.Boundary{ID: "ext:" + key, Path: key, Category: category(key), Kind: kind}
		for p := range ps {
			b.Packages = append(b.Packages, p)
			if b.Category == "other" {
				b.Category = category(p)
			}
		}
		sort.Strings(b.Packages)
		g.Boundaries = append(g.Boundaries, b)
	}
	g.Sort()
	if entry != nil {
		pkgID, err := a.entryPackage(entry)
		if err != nil {
			return nil, err
		}
		ids, err := resolveEntries(g, *entry, pkgID, a.modDir)
		if err != nil {
			return nil, err
		}
		g.Meta.Entry, g.Meta.Entries = o.Entry, ids
	}
	return g, nil
}

// entryPackage is the graph id of the entry's package.
func (a *analyzer) entryPackage(e *entrySpec) (string, error) {
	if e.dir == "" { // an import path
		if !a.inMod(e.pattern) {
			return "", fmt.Errorf("entry %q is not in module %s", e.pattern, a.modPath)
		}
		return a.short(e.pattern), nil
	}
	rel, err := filepath.Rel(a.modDir, e.dir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("entry %s is outside module %s", e.dir, a.modDir)
	}
	if rel == "." {
		return "(root)", nil
	}
	return filepath.ToSlash(rel), nil
}

// ---- per-function traversal ------------------------------------------------

func (a *analyzer) visit(n *callgraph.Node) {
	caller := a.canon(n.Func)
	callerID, ok := a.nodeID(caller)
	if !ok {
		a.noteGeneratedCalls(caller, n)
		return
	}
	if len(n.Func.Blocks) == 0 {
		return
	}
	if isInitFn(caller) {
		a.initPkgs[callerID] = caller.Pkg.Pkg.Path()
	}
	bySite := map[ssa.CallInstruction][]*callgraph.Edge{}
	for _, e := range n.Out {
		if e.Site != nil {
			bySite[e.Site] = append(bySite[e.Site], e)
		}
	}
	chaBySite := map[ssa.CallInstruction][]*callgraph.Edge{}
	if a.cha != nil {
		if cn := a.cha.Nodes[n.Func]; cn != nil {
			for _, e := range cn.Out {
				if e.Site != nil {
					chaBySite[e.Site] = append(chaBySite[e.Site], e)
				}
			}
		}
	}
	for _, b := range n.Func.Blocks {
		for _, in := range b.Instrs {
			a.noteRefs(in)
			switch x := in.(type) {
			case ssa.CallInstruction:
				a.visitSite(callerID, caller, x, bySite[x], chaBySite[x])
			case *ssa.MakeInterface:
				a.escape(callerID, n.Func, x)
			case *ssa.ChangeInterface:
				a.escapeIface(callerID, n.Func, x)
			}
		}
	}
}

func (a *analyzer) visitSite(callerID string, caller *ssa.Function, ci ssa.CallInstruction, out, chaOut []*callgraph.Edge) {
	common := ci.Common()
	if common.IsInvoke() == false && common.Value != nil {
		if _, isBuiltin := common.Value.(*ssa.Builtin); isBuiltin {
			return
		}
	}
	mode := "call"
	switch ci.(type) {
	case *ssa.Go:
		mode = "go"
	case *ssa.Defer:
		mode = "defer"
	}
	pos := a.pos(ci.Pos())
	if pos == "" {
		return
	}
	st := common.StaticCallee()
	dynamic := st == nil

	// 1. own -> own
	var info siteInfo
	if dynamic {
		info = a.siteInfo(common)
	}
	own := map[string]bool{} // callee id -> reached only through a closure defined in it
	callees := make([]*ssa.Function, 0, len(out)+1)
	for _, e := range out {
		callees = append(callees, e.Callee.Func)
	}
	if len(out) == 0 && st != nil {
		callees = append(callees, st) // caller has no call graph node (never-instantiated generic code)
	}
	// fromCallees computes the own callees of a set of resolved callees
	fromCallees := func(cs []*ssa.Function) map[string]bool {
		m := map[string]bool{}
		for _, cf := range cs {
			c := a.canon(cf)
			cid, ok := a.nodeID(c)
			if !ok || isInitFn(c) || (cid == callerID && cf != caller) {
				continue
			}
			m[cid] = a.isClosure(cf)
		}
		return m
	}
	// VTA resolved a dynamic call to nothing at all: typically a library, where the concrete values
	// come from outside the module (tests, users), so no type ever flows in. Fall back to the
	// class hierarchy (every implementer) and say so; the go-sdk run found VTA alone misses these.
	// Not when VTA did resolve it, even to external types only: then the own implementers that CHA
	// would add are the "own type happens to implement an external interface" false positives.
	if dynamic && a.cha != nil && len(out) == 0 && len(chaOut) > 0 {
		var cs []*ssa.Function
		for _, e := range chaOut {
			cs = append(cs, e.Callee.Func)
		}
		if m := fromCallees(cs); len(m) > 0 {
			callees = cs
			info.flags = append(append([]string(nil), info.flags...), "cha-fallback")
		}
	}
	for _, cf := range callees {
		c := a.canon(cf)
		cid, ok := a.nodeID(c)
		if !ok || isInitFn(c) {
			continue // init code is never called, only run (import-order calls between initialisers are noise)
		}
		if cid == callerID && cf != caller {
			continue // call into own closure: folded into this function
		}
		cb := a.isClosure(cf)
		if prev, seen := own[cid]; seen {
			own[cid] = prev && cb
		} else {
			own[cid] = cb
		}
	}
	for to, cb := range own {
		kind, cand := "direct", 0
		if dynamic {
			kind, cand = "dynamic", len(own)
		}
		if cb {
			kind = "callback"
		}
		a.add(callerID, to, kind, mode, pos, cand, info)
	}

	// 2. own -> external boundary, classified by call site, not by resolved callee
	ext := ""
	if st != nil {
		if p := a.pkgPath(a.canon(st)); p != "" && !a.inMod(p) {
			ext = p
		}
	} else if common.IsInvoke() {
		if m := common.Method; m != nil && m.Pkg() != nil && !a.inMod(m.Pkg().Path()) {
			ext = m.Pkg().Path()
		}
	}
	if ext == "" {
		return
	}
	// Every external package is a fact and is kept (fmt, errors, ... included). Which ones are noise
	// is a presentation decision: the model layer (internal/model) marks them hidden by default.
	key := ext // std packages stay individual; third-party packages merge into their module
	if m, ok := a.modOf[ext]; ok {
		key = m
	}
	if a.bounds[key] == nil {
		a.bounds[key] = map[string]bool{}
	}
	a.bounds[key][ext] = true
	a.add(callerID, "ext:"+key, "boundary", mode, pos, 0, siteInfo{})

	// 3. registration: own func value passed to an external function
	for _, arg := range common.Args {
		if fv := a.funcValue(arg); fv != nil {
			c := a.canon(fv)
			if cid, ok := a.nodeID(c); ok && !isInitFn(c) && cid != callerID {
				a.add(callerID, cid, "registers", mode, pos, 0, siteInfo{})
			}
		}
		if ext == "text/template" || ext == "html/template" {
			a.reflectExport(callerID, arg, pos)
		}
	}
}

func (a *analyzer) add(from, to, kind, mode, site string, cand int, info siteInfo) {
	k := ekey{from, to, kind, mode}
	e := a.edges[k]
	if e == nil {
		e = &eacc{sites: map[string]map[string]bool{}, ifaces: map[string]bool{}}
		a.edges[k] = e
	}
	fl := e.sites[site]
	if fl == nil {
		fl = map[string]bool{}
		e.sites[site] = fl
	}
	for _, f := range info.flags {
		fl[f] = true
	}
	if info.iface != "" {
		e.ifaces[info.iface] = true
	}
	if cand > e.cand {
		e.cand = cand
	}
}

// ---- helpers ---------------------------------------------------------------

// canon maps closures to their outermost function and generic instances to their origin.
func (a *analyzer) canon(fn *ssa.Function) *ssa.Function {
	for {
		if o := fn.Origin(); o != nil {
			fn = o
			continue
		}
		if p := fn.Parent(); p != nil {
			fn = p
			continue
		}
		return fn
	}
}

func (a *analyzer) own(fn *ssa.Function) bool {
	if fn == nil || fn.Synthetic != "" || fn.Pkg == nil || !a.inMod(fn.Pkg.Pkg.Path()) {
		return false
	}
	if isInitFn(fn) {
		return false
	}
	return !a.gen[a.prog.Fset.Position(fn.Pos()).Filename]
}

func (a *analyzer) inMod(path string) bool {
	return path == a.modPath || strings.HasPrefix(path, a.modPath+"/")
}

func (a *analyzer) pkgPath(fn *ssa.Function) string {
	if fn.Pkg != nil {
		return fn.Pkg.Pkg.Path()
	}
	if o := fn.Object(); o != nil && o.Pkg() != nil {
		return o.Pkg().Path()
	}
	return ""
}

func (a *analyzer) short(path string) string {
	if path == a.modPath {
		return "(root)"
	}
	return strings.TrimPrefix(path, a.modPath+"/")
}

// id is a stable, line-number-free function ID.
func (a *analyzer) id(fn *ssa.Function) string {
	pk := a.short(fn.Pkg.Pkg.Path())
	if r := fn.Signature.Recv(); r != nil {
		t, ptr := r.Type(), ""
		if p, ok := t.(*types.Pointer); ok {
			t, ptr = p.Elem(), "*"
		}
		if n, ok := t.(*types.Named); ok {
			return "(" + ptr + pk + "." + n.Obj().Name() + ")." + fn.Name()
		}
	}
	return pk + "." + fn.Name()
}

func (a *analyzer) label(fn *ssa.Function) string {
	if r := fn.Signature.Recv(); r != nil {
		t := r.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		if n, ok := t.(*types.Named); ok {
			return n.Obj().Name() + "." + fn.Name()
		}
	}
	return fn.Name()
}

func (a *analyzer) pos(p token.Pos) string { return a.posIn(a.prog.Fset, p) }
