package analyze

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
)

// Edges the call graph algorithms cannot see. Found by comparing the static graph with a
// runtime trace (docs/recall-check-2026-11-05.md):
//
//	package initialisation   var initialisers and init() run before main, nobody "calls" them
//	implicit interface use   an own type converted to an external interface (error, fmt.Stringer,
//	                         types.Importer, ...) is later called by code we do not analyse
//	reflection (templates)   text/template calls exported methods by name
//	go:linkname              a body-less declaration is bound to a function in another package

// isInitFn reports whether fn is package initialisation code.
func isInitFn(fn *ssa.Function) bool {
	if fn == nil || fn.Pkg == nil {
		return false
	}
	if fn.Synthetic == "package initializer" {
		return true
	}
	n := fn.Name()
	// a *method* named init is an ordinary method
	return fn.Synthetic == "" && fn.Signature.Recv() == nil && (n == "init" || strings.HasPrefix(n, "init#"))
}

// nodeID returns the graph node a function is folded into: itself, or the single
// "<pkg>.init" node that stands for all package initialisation code of its package.
func (a *analyzer) nodeID(fn *ssa.Function) (string, bool) {
	if fn == nil || fn.Pkg == nil || !a.inMod(fn.Pkg.Pkg.Path()) {
		return "", false
	}
	if isInitFn(fn) {
		if fn.Synthetic == "" && a.gen[a.prog.Fset.Position(fn.Pos()).Filename] {
			return "", false
		}
		return a.short(fn.Pkg.Pkg.Path()) + ".init", true
	}
	if !a.own(fn) {
		return "", false
	}
	return a.id(fn), true
}

// ownNamed returns the own named type behind T or *T.
func (a *analyzer) ownNamed(t types.Type) *types.Named {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := types.Unalias(t).(*types.Named)
	if !ok || n.Obj().Pkg() == nil || !a.inMod(n.Obj().Pkg().Path()) {
		return nil
	}
	return n
}

// Methods that fmt, encoding/json etc. call on any value through well-known interfaces.
// name -> number of results.
var wellKnown = map[string]int{
	"String": 1, "Error": 1, "GoString": 1, "Format": 0,
	"MarshalJSON": 2, "MarshalText": 2, "MarshalBinary": 2,
	"UnmarshalJSON": 1, "UnmarshalText": 1, "UnmarshalBinary": 1, // json.Unmarshal(data, &v)
}

// Methods errors.Is / errors.As / errors.Unwrap call on an error value, beyond Error().
var errorExtras = map[string]int{"Is": 1, "As": 1, "Unwrap": 1}

// escape handles `own value -> external interface` conversions: the methods the interface
// requires (or, for `any`, the well-known ones) may now be called by code we do not see.
func (a *analyzer) escape(callerID string, self *ssa.Function, mi *ssa.MakeInterface) {
	a.escapeType(callerID, self, mi.Pos(), mi.X.Type(), mi.Type())
}

// escapeIface is the same for an own *interface* value converted to another interface:
// any type that implements the source interface may be the dynamic value.
func (a *analyzer) escapeIface(callerID string, self *ssa.Function, ci *ssa.ChangeInterface) {
	n, ok := types.Unalias(ci.X.Type()).(*types.Named)
	if !ok || n.Obj().Pkg() == nil || !a.inMod(n.Obj().Pkg().Path()) {
		return // only own interfaces: for others we know nothing about the implementers
	}
	src, _ := n.Underlying().(*types.Interface)
	if src == nil || src.NumMethods() == 0 {
		return
	}
	for _, rt := range a.runtimeTypes() {
		if types.Implements(rt, src) {
			a.escapeType(callerID, self, ci.Pos(), rt, ci.Type())
		}
	}
}

// runtimeTypes are the own concrete types that are converted to some interface anywhere.
func (a *analyzer) runtimeTypes() []types.Type {
	if a.rtypes == nil {
		a.rtypes = []types.Type{}
		for _, t := range a.prog.RuntimeTypes() {
			if _, isIface := t.Underlying().(*types.Interface); !isIface && a.ownNamed(t) != nil {
				a.rtypes = append(a.rtypes, t)
			}
		}
	}
	return a.rtypes
}

func (a *analyzer) escapeType(callerID string, self *ssa.Function, at token.Pos, concrete, target types.Type) {
	if a.ownNamed(concrete) == nil {
		return
	}
	target = types.Unalias(target)
	iface, _ := target.Underlying().(*types.Interface)
	if iface == nil {
		return
	}
	if n, ok := target.(*types.Named); ok {
		if n.Obj().Pkg() != nil && a.inMod(n.Obj().Pkg().Path()) {
			return // own interface: the call graph already resolves calls through it
		}
	} else if iface.NumMethods() > 0 {
		return // anonymous non-empty interface
	}
	pos := a.pos(at)
	if pos == "" {
		pos = a.pos(self.Pos())
	}
	if pos == "" {
		return
	}
	flags := siteInfo{flags: []string{"implicit-iface"}}
	ms := a.prog.MethodSets.MethodSet(concrete)
	if iface.NumMethods() > 0 {
		for i := 0; i < iface.NumMethods(); i++ {
			a.methodEdge(callerID, ms, iface.Method(i), pos, flags)
		}
		if n, ok := target.(*types.Named); ok && n.Obj().Pkg() == nil && n.Obj().Name() == "error" {
			a.methodsNamed(callerID, ms, errorExtras, pos, flags)
		}
		return
	}
	// `any`: fmt and encoding/json call well-known methods on the value, and on the values
	// inside it (struct fields, slice elements, ...), found by reflection.
	a.wellKnownEdges(callerID, ms, pos, flags)
	for _, nested := range a.reachOwn(concrete) {
		a.wellKnownEdges(callerID, a.prog.MethodSets.MethodSet(types.NewPointer(nested)), pos, flags)
	}
}

func (a *analyzer) wellKnownEdges(callerID string, ms *types.MethodSet, pos string, info siteInfo) {
	a.methodsNamed(callerID, ms, wellKnown, pos, info)
}

// methodsNamed adds an edge to each method of ms whose name is in names (name -> result count).
func (a *analyzer) methodsNamed(callerID string, ms *types.MethodSet, names map[string]int, pos string, info siteInfo) {
	for i := 0; i < ms.Len(); i++ {
		m, ok := ms.At(i).Obj().(*types.Func)
		if !ok {
			continue
		}
		if n, ok := names[m.Name()]; ok && m.Type().(*types.Signature).Results().Len() == n {
			a.methodEdge(callerID, ms, m, pos, info)
		}
	}
}

// reachOwn lists the own named types reachable through the exported fields, elements and
// pointers of t (reflection-based encoders walk exactly these), excluding t itself.
func (a *analyzer) reachOwn(t types.Type) []*types.Named {
	if a.reachCache == nil {
		a.reachCache = map[types.Type][]*types.Named{}
	}
	if r, ok := a.reachCache[t]; ok {
		return r
	}
	var out []*types.Named
	seen := map[types.Type]bool{}
	var walk func(t types.Type, top bool)
	walk = func(t types.Type, top bool) {
		t = types.Unalias(t)
		if seen[t] {
			return
		}
		seen[t] = true
		switch x := t.(type) {
		case *types.Pointer:
			walk(x.Elem(), top)
		case *types.Slice:
			walk(x.Elem(), false)
		case *types.Array:
			walk(x.Elem(), false)
		case *types.Chan:
			walk(x.Elem(), false)
		case *types.Map:
			walk(x.Key(), false)
			walk(x.Elem(), false)
		case *types.Named:
			if x.Obj().Pkg() == nil || !a.inMod(x.Obj().Pkg().Path()) {
				return // another package's type: we do not know what is inside
			}
			if it, ok := x.Underlying().(*types.Interface); ok {
				// an interface-typed field holds, at run time, any implementer
				for _, rt := range a.runtimeTypes() {
					if it.NumMethods() > 0 && types.Implements(rt, it) {
						walk(rt, false)
					}
				}
				return
			}
			if !top {
				out = append(out, x)
			}
			walk(x.Underlying(), false)
		case *types.Struct:
			for i := 0; i < x.NumFields(); i++ {
				if f := x.Field(i); f.Exported() {
					walk(f.Type(), false)
				}
			}
		}
	}
	walk(t, true)
	a.reachCache[t] = out
	return out
}

// reflectExport: a value handed to a template may have any exported method called by name.
func (a *analyzer) reflectExport(callerID string, arg ssa.Value, pos string) {
	mi, ok := arg.(*ssa.MakeInterface)
	if !ok || a.ownNamed(mi.X.Type()) == nil {
		return
	}
	ms := a.prog.MethodSets.MethodSet(mi.X.Type())
	for i := 0; i < ms.Len(); i++ {
		if m, ok := ms.At(i).Obj().(*types.Func); ok && m.Exported() {
			a.methodEdge(callerID, ms, m, pos, siteInfo{flags: []string{"reflect"}})
		}
	}
}

func (a *analyzer) methodEdge(callerID string, ms *types.MethodSet, m *types.Func, pos string, info siteInfo) {
	sel := ms.Lookup(m.Pkg(), m.Name())
	if sel == nil {
		return
	}
	fn := a.prog.MethodValue(sel) // nil for abstract methods
	if fn == nil {
		return
	}
	c := a.canon(a.unwrap(fn))
	to, ok := a.nodeID(c)
	if !ok || isInitFn(c) || to == callerID {
		return
	}
	a.add(callerID, to, "registers", "call", pos, 0, info)
}

// noteRefs records functions used as values (stored, passed, compared) rather than called.
func (a *analyzer) noteRefs(in ssa.Instruction) {
	var callee *ssa.Value
	if ci, ok := in.(ssa.CallInstruction); ok {
		callee = &ci.Common().Value
	}
	var buf [8]*ssa.Value
	for _, op := range in.Operands(buf[:0]) {
		if op == nil || op == callee || *op == nil {
			continue
		}
		fn, ok := (*op).(*ssa.Function)
		if !ok || fn.Parent() != nil { // a closure literal is not a reference to its parent
			continue
		}
		c := a.canon(a.unwrap(fn))
		if id, ok := a.nodeID(c); ok && !isInitFn(c) {
			a.refd[id] = true
		}
	}
}

type link struct{ from, to, site string }

// scanLinknames finds `//go:linkname local pkg.name` on body-less declarations that point
// into another package of the module (the "pull" form used e.g. by x/tools/go/ssa/ssautil).
func (a *analyzer) scanLinknames(pkgs []*packages.Package) {
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if !a.inMod(p.PkgPath) {
			return
		}
		for _, f := range p.Syntax {
			bodyless := map[string]bool{}
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Body == nil && fd.Recv == nil {
					bodyless[fd.Name.Name] = true
				}
			}
			for _, cg := range f.Comments {
				for _, c := range cg.List {
					fs := strings.Fields(c.Text)
					if len(fs) != 3 || fs[0] != "//go:linkname" || !bodyless[fs[1]] {
						continue
					}
					i := strings.LastIndex(fs[2], ".")
					if i <= 0 || strings.ContainsAny(fs[2][:i], "()*") {
						continue
					}
					rp, name := fs[2][:i], fs[2][i+1:]
					if !a.inMod(rp) {
						continue
					}
					a.links = append(a.links, link{
						from: a.short(p.PkgPath) + "." + fs[1],
						to:   a.short(rp) + "." + name,
						site: a.posIn(p.Fset, c.Pos()),
					})
				}
			}
		}
	})
}

func (a *analyzer) posIn(fset *token.FileSet, p token.Pos) string {
	if !p.IsValid() {
		return ""
	}
	pp := fset.Position(p)
	rel, err := filepath.Rel(a.modDir, pp.Filename)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = pp.Filename
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(rel), pp.Line)
}

// noteGeneratedCalls remembers what generated code calls. Generated functions are not graph
// nodes, but a function whose only callers are generated is not dead (recall check, x/tools
// internal/typeparams: hand-written helpers called by code copied in by a generator).
func (a *analyzer) noteGeneratedCalls(caller *ssa.Function, n *callgraph.Node) {
	if caller.Pkg == nil || caller.Synthetic != "" || !a.inMod(caller.Pkg.Pkg.Path()) || isInitFn(caller) {
		return
	}
	if !a.gen[a.prog.Fset.Position(caller.Pos()).Filename] {
		return
	}
	for _, e := range n.Out {
		c := a.canon(e.Callee.Func)
		if id, ok := a.nodeID(c); ok && !isInitFn(c) {
			a.genCalled[id] = true
		}
	}
}
