package analyze

import (
	"go/types"
	"sort"

	"golang.org/x/tools/go/packages"
)

// A method that satisfies an interface but is never called by name is not dead code: it may be
// called through the interface by code we cannot see, or exist only so the type satisfies a
// (sealed) interface, like `nodeImpl()` marker methods. Seen in the recall check
// (docs/recall-check-2026-11-05.md): 4 of the 6 remaining "unused" functions.
//
// Generics: a generic type or interface has no method set of its own, so we test the
// instantiations that actually appear in the source (Stack[int], Container[int], ...).

type ifaceDecl struct {
	name string // display name: module-relative for own interfaces, without type arguments
	ext  bool   // declared outside the module
	typ  *types.Interface
	n    int // number of methods
}

// hasTypeParam reports whether t still mentions a type parameter (an instantiation inside
// generic code, e.g. Stack[T], says nothing about concrete types).
func hasTypeParam(t types.Type) bool {
	switch x := types.Unalias(t).(type) {
	case *types.TypeParam:
		return true
	case *types.Pointer:
		return hasTypeParam(x.Elem())
	case *types.Slice:
		return hasTypeParam(x.Elem())
	case *types.Array:
		return hasTypeParam(x.Elem())
	case *types.Chan:
		return hasTypeParam(x.Elem())
	case *types.Map:
		return hasTypeParam(x.Key()) || hasTypeParam(x.Elem())
	case *types.Named:
		for i := 0; i < x.TypeArgs().Len(); i++ {
			if hasTypeParam(x.TypeArgs().At(i)) {
				return true
			}
		}
	}
	return false
}

// indexInterfaces collects, by method name, every named interface of the program, and every
// concrete instantiation of generic types that appears in the source.
func (a *analyzer) indexInterfaces(pkgs []*packages.Package) {
	a.ifaces = map[string][]ifaceDecl{}
	a.typeInst = map[*types.TypeName][]*types.Named{}
	add := func(d ifaceDecl) {
		for i := 0; i < d.typ.NumMethods(); i++ {
			m := d.typ.Method(i).Name()
			a.ifaces[m] = append(a.ifaces[m], d)
		}
	}
	display := func(tn *types.TypeName) (string, bool) {
		path := tn.Pkg().Path()
		if a.inMod(path) {
			return a.short(path) + "." + tn.Name(), false
		}
		return path + "." + tn.Name(), true
	}

	// instantiations seen in the source: Stack[int], Container[string], ...
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if p.TypesInfo == nil {
			return
		}
		for _, inst := range p.TypesInfo.Instances {
			named, ok := types.Unalias(inst.Type).(*types.Named)
			if !ok || named.TypeArgs().Len() == 0 || hasTypeParam(named) {
				continue
			}
			key := types.TypeString(named, nil)
			if seen[key] {
				continue
			}
			seen[key] = true
			origin := named.Origin().Obj()
			a.typeInst[origin] = append(a.typeInst[origin], named)
			if it, ok := named.Underlying().(*types.Interface); ok && it.NumMethods() > 0 && origin.Pkg() != nil {
				name, ext := display(origin)
				if !ext || origin.Exported() {
					add(ifaceDecl{name: name, ext: ext, typ: it, n: it.NumMethods()})
				}
			}
		}
	})

	// non-generic named interfaces
	for _, p := range a.prog.AllPackages() {
		sc := p.Pkg.Scope()
		for _, nm := range sc.Names() {
			tn, ok := sc.Lookup(nm).(*types.TypeName)
			if !ok || tn.IsAlias() {
				continue
			}
			named, ok := tn.Type().(*types.Named)
			if !ok || named.TypeParams().Len() > 0 {
				continue
			}
			it, ok := named.Underlying().(*types.Interface)
			if !ok || it.NumMethods() == 0 {
				continue
			}
			name, ext := display(tn)
			if ext && !tn.Exported() {
				continue // another package's private interface: nobody outside can use it
			}
			add(ifaceDecl{name: name, ext: ext, typ: it, n: it.NumMethods()})
		}
	}
	if e, ok := types.Universe.Lookup("error").Type().Underlying().(*types.Interface); ok {
		add(ifaceDecl{name: "error", ext: true, typ: e, n: 1})
	}
}

// implementsOf lists (at most 4) interfaces that the receiver type of method fully implements
// and that contain the method: own interfaces first, then the smallest ones.
func (a *analyzer) implementsOf(recv types.Type, method string) []string {
	if recv == nil {
		return nil
	}
	base := recv
	if p, ok := base.(*types.Pointer); ok {
		base = p.Elem()
	}
	n, ok := types.Unalias(base).(*types.Named)
	if !ok {
		return nil
	}
	targets := []types.Type{base}
	if n.TypeParams().Len() > 0 { // generic type: use the instantiations seen in the source
		targets = nil
		for _, inst := range a.typeInst[n.Obj()] {
			targets = append(targets, inst)
		}
	}
	var found []ifaceDecl
	seen := map[string]bool{}
	for _, t := range targets {
		ptr := types.NewPointer(t)
		for _, d := range a.ifaces[method] {
			if seen[d.name] {
				continue
			}
			if types.Implements(t, d.typ) || types.Implements(ptr, d.typ) {
				seen[d.name] = true
				found = append(found, d)
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		x, y := found[i], found[j]
		if x.ext != y.ext {
			return !x.ext
		}
		if x.n != y.n {
			return x.n < y.n
		}
		return x.name < y.name
	})
	if len(found) > 4 {
		found = found[:4]
	}
	names := make([]string, len(found))
	for i, d := range found {
		names[i] = d.name
	}
	return names
}
