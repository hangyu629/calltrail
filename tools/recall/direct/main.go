// Command direct is a static oracle for *direct* call edges, independent of SSA and of any
// call graph algorithm: it reads every statically resolvable call out of the syntax trees
// (go/types), and checks it against graph.json in both directions.
//
//	recall:    every call expression in own code has a matching edge in the graph
//	precision: every `direct` edge between own functions has a matching call expression
//
// Generic functions and methods of generic types get their own line in the report,
// because instances fold into one origin node and are easy to get wrong.
//
//	go run ./tools/recall/direct -dir <module dir> -graph graph.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"

	"github.com/hangyu629/calltrail/internal/graph"
)

type pair struct{ from, to string }

func main() {
	dir := flag.String("dir", ".", "module directory")
	gpath := flag.String("graph", "graph.json", "graph.json produced by calltrail")
	show := flag.Int("show", 12, "how many examples to print per category")
	flag.Parse()

	b, err := os.ReadFile(*gpath)
	must(err)
	var g graph.Graph
	must(json.Unmarshal(b, &g))
	mod := g.Meta.Module

	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
		Dir: *dir,
	}
	pkgs, err := packages.Load(cfg, "./...")
	must(err)

	inMod := func(p string) bool { return p == mod || strings.HasPrefix(p, mod+"/") }
	short := func(p string) string {
		if p == mod {
			return "(root)"
		}
		return strings.TrimPrefix(p, mod+"/")
	}
	// ID of a function or method, in the analyzer's format.
	funcID := func(f *types.Func) (string, bool) {
		f = f.Origin()
		if f.Pkg() == nil || !inMod(f.Pkg().Path()) {
			return "", false
		}
		sig := f.Type().(*types.Signature)
		if r := sig.Recv(); r != nil {
			t, ptr := r.Type(), ""
			if p, ok := t.(*types.Pointer); ok {
				t, ptr = p.Elem(), "*"
			}
			n, ok := types.Unalias(t).(*types.Named)
			if !ok {
				return "", false // method of an interface: dynamic
			}
			return "(" + ptr + short(f.Pkg().Path()) + "." + n.Obj().Name() + ")." + f.Name(), true
		}
		return short(f.Pkg().Path()) + "." + f.Name(), true
	}
	isGeneric := func(f *types.Func) bool {
		f = f.Origin()
		sig := f.Type().(*types.Signature)
		if sig.TypeParams().Len() > 0 {
			return true
		}
		if r := sig.Recv(); r != nil {
			t := r.Type()
			if p, ok := t.(*types.Pointer); ok {
				t = p.Elem()
			}
			if n, ok := types.Unalias(t).(*types.Named); ok {
				return n.TypeParams().Len() > 0
			}
		}
		return false
	}

	src := map[pair]bool{}        // calls found in the source
	srcGeneric := map[pair]bool{} // ... whose caller or callee is generic
	// calls on a value whose type is a type parameter (func f[S I](s S) { s.M() }): in the source
	// the callee is the constraint's abstract method; after instantiation it is a direct call
	typeParamCalls := map[pair]bool{} // caller id -> method name (stored in .to)
	for _, p := range pkgs {
		if !inMod(p.PkgPath) {
			continue
		}
		for _, f := range p.Syntax {
			if ast.IsGenerated(f) {
				continue
			}
			for _, d := range f.Decls {
				var callerID string
				var callerGeneric bool
				switch d := d.(type) {
				case *ast.FuncDecl:
					obj, _ := p.TypesInfo.Defs[d.Name].(*types.Func)
					if obj == nil || d.Body == nil {
						continue
					}
					if d.Recv == nil && (d.Name.Name == "init") {
						callerID = short(p.PkgPath) + ".init"
					} else if id, ok := funcID(obj); ok {
						callerID, callerGeneric = id, isGeneric(obj)
					} else {
						continue
					}
				case *ast.GenDecl: // package-level var initialisers run in the package init
					callerID = short(p.PkgPath) + ".init"
				default:
					continue
				}
				ast.Inspect(d, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					fun := ast.Unparen(call.Fun)
					switch x := fun.(type) { // explicit instantiation: F[int](...)
					case *ast.IndexExpr:
						fun = ast.Unparen(x.X)
					case *ast.IndexListExpr:
						fun = ast.Unparen(x.X)
					}
					var id *ast.Ident
					switch x := fun.(type) {
					case *ast.Ident:
						id = x
					case *ast.SelectorExpr:
						id = x.Sel
						if _, isTP := types.Unalias(p.TypesInfo.TypeOf(x.X)).(*types.TypeParam); isTP {
							typeParamCalls[pair{callerID, x.Sel.Name}] = true
						}
					default:
						return true
					}
					callee, _ := p.TypesInfo.Uses[id].(*types.Func)
					if callee == nil {
						return true // conversion, builtin, function value
					}
					to, ok := funcID(callee)
					if !ok {
						return true
					}
					pr := pair{callerID, to}
					src[pr] = true
					if callerGeneric || isGeneric(callee) {
						srcGeneric[pr] = true
					}
					return true
				})
			}
		}
	}

	have := map[pair]bool{} // any edge kind
	haveDirect := map[pair]bool{}
	fns := map[string]bool{}
	for _, f := range g.Functions {
		fns[f.ID] = true
	}
	for _, e := range g.Edges {
		have[pair{e.From, e.To}] = true
		linkname := false
		for _, f := range e.Flags {
			linkname = linkname || f == "linkname" // synthetic by design: no call expression exists
		}
		if e.Kind == "direct" && !linkname {
			haveDirect[pair{e.From, e.To}] = true
		}
	}

	report := func(title string, set map[pair]bool, in map[pair]bool, ex *int) (found, total int, missing []pair) {
		for pr := range set {
			if !fns[pr.from] || !fns[pr.to] { // the graph may cover only part of the module
				continue
			}
			total++
			if in[pr] {
				found++
			} else {
				missing = append(missing, pr)
			}
		}
		sort.Slice(missing, func(i, j int) bool {
			if missing[i].from != missing[j].from {
				return missing[i].from < missing[j].from
			}
			return missing[i].to < missing[j].to
		})
		fmt.Printf("%-52s %5d / %-5d  (%.1f%%)\n", title, found, total, 100*float64(found)/float64(max(total, 1)))
		return
	}
	var n int
	_, _, miss := report("recall: source calls found in graph (all)", src, have, &n)
	_, _, missG := report("recall: ... involving a generic function/type", srcGeneric, have, &n)
	printPairs("missing (all)", miss, *show)
	if len(missG) > 0 {
		printPairs("missing (generic)", missG, *show)
	}

	// precision: direct edges nobody wrote
	methodName := func(id string) string {
		if i := strings.LastIndex(id, ")."); i >= 0 {
			return id[i+2:]
		}
		return id[strings.LastIndex(id, ".")+1:]
	}
	var phantom []pair
	var total, viaTypeParam int
	for e := range haveDirect {
		if strings.HasSuffix(e.from, ".init") || !fns[e.from] || !fns[e.to] {
			continue
		}
		total++
		switch {
		case src[e]:
		case typeParamCalls[pair{e.from, methodName(e.to)}]:
			viaTypeParam++ // explained: call through a type parameter, resolved at instantiation
		default:
			phantom = append(phantom, e)
		}
	}
	sort.Slice(phantom, func(i, j int) bool { return phantom[i].from+phantom[i].to < phantom[j].from+phantom[j].to })
	backed := total - len(phantom) - viaTypeParam
	fmt.Printf("%-52s %5d / %-5d  (%.1f%%)\n", "precision: direct edges backed by a call expression", backed, total, 100*float64(backed)/float64(max(total, 1)))
	fmt.Printf("%-52s %5d\n", "  + explained: call through a type parameter", viaTypeParam)
	fmt.Printf("%-52s %5d\n", "  = unexplained", len(phantom))
	printPairs("direct edges with no matching call expression and no type-parameter call", phantom, *show)
}

func printPairs(title string, ps []pair, show int) {
	if len(ps) == 0 {
		return
	}
	fmt.Printf("\n--- %s (%d)\n", title, len(ps))
	for i, p := range ps {
		if i >= show {
			fmt.Printf("   ... %d more\n", len(ps)-show)
			break
		}
		fmt.Printf("   %s -> %s\n", p.from, p.to)
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "direct:", err)
		os.Exit(1)
	}
}
