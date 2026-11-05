package analyze

import (
	"strings"
	"testing"

	"github.com/hangyu629/calltrail/internal/graph"
)

func load(t *testing.T, dir string) *graph.Graph {
	t.Helper()
	g, err := Run(Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func edge(g *graph.Graph, from, to, kind string) *graph.Edge {
	for i := range g.Edges {
		e := &g.Edges[i]
		if e.From == from && e.To == to && e.Kind == kind {
			return e
		}
	}
	return nil
}

func fn(g *graph.Graph, id string) *graph.Function {
	for i := range g.Functions {
		if g.Functions[i].ID == id {
			return &g.Functions[i]
		}
	}
	return nil
}

// Generics: instances fold into their generic origin, and dispatch through a generic
// interface still resolves to methods of generic types.
func TestGenerics(t *testing.T) {
	g := load(t, "testdata/generics")

	for _, c := range []struct{ from, to, kind string }{
		{"(root).main", "(root).use", "direct"},
		{"(root).main", "(root).Sum", "direct"}, // Sum[int] and Sum[float64] share one node
		{"(root).main", "(root).Map", "direct"},
		{"(root).use", "(*(root).Stack).Push", "dynamic"}, // c.Push via Container[int]
		{"(root).use", "(*(root).Stack).Len", "dynamic"},
		{"(root).Map", "(root).main", "callback"}, // Map calls the closure defined in main
		{"(root).main", "((root).Set).Len", "dynamic"},
		// never-instantiated generic type: edges come from the generic origin's body
		{"((root).Orphan).World", "(root).orphanHelper", "direct"},
	} {
		if edge(g, c.from, c.to, c.kind) == nil {
			t.Errorf("missing %s edge %s -> %s", c.kind, c.from, c.to)
		}
	}
	for _, id := range []string{"(root).Sum", "(*(root).Stack).Push"} {
		if n := countFn(g, id); n != 1 {
			t.Errorf("%s appears %d times, want 1 (instances must fold)", id, n)
		}
	}

	roles := map[string]string{
		"(*(root).Stack).marker": "unused", // not called, implements nothing
		"((root).Set).Add":       "api",    // exported, no callers, implements nothing
		"((root).A).seal":        "iface",  // implements sealed, via a generic receiver
		"((root).Orphan).Hello":  "api",    // generic type nobody instantiates: still listed
	}
	for id, want := range roles {
		f := fn(g, id)
		if f == nil {
			t.Errorf("function %s not in graph", id)
			continue
		}
		if f.Role != want {
			t.Errorf("%s: role %q, want %q (implements %v)", id, f.Role, want, f.Implements)
		}
	}

	want := map[string]string{
		"((root).A).seal":        "sealed",    // non-generic interface, generic receiver
		"(*(root).Stack).Push":   "Container", // generic interface
		"((root).Set).Len":       "Lener",
		"(*(root).Stack).marker": "",
	}
	for id, sub := range want {
		f := fn(g, id)
		if f == nil {
			continue
		}
		got := strings.Join(f.Implements, ",")
		if sub == "" && got != "" || !strings.Contains(got, sub) {
			t.Errorf("%s: implements %q, want it to contain %q", id, got, sub)
		}
	}
}

func countFn(g *graph.Graph, id string) int {
	n := 0
	for _, f := range g.Functions {
		if f.ID == id {
			n++
		}
	}
	return n
}

// Generated code is dropped even when its package is only a dependency of the packages
// named by the patterns (it used to be detected only for the pattern packages).
func TestGeneratedFilesInDependencies(t *testing.T) {
	g, err := Run(Options{Dir: "testdata/depgen", Patterns: []string{"."}})
	if err != nil {
		t.Fatal(err)
	}
	if fn(g, "sub.Real") == nil {
		t.Error("sub.Real missing")
	}
	for _, id := range []string{"sub.Generated", "sub.helper"} {
		if fn(g, id) != nil {
			t.Errorf("%s comes from a generated file and must not be in the graph", id)
		}
	}
	// ... but what generated code calls is not dead
	if f := fn(g, "sub.onlyFromGenerated"); f == nil || f.Role != "generated" {
		t.Errorf("sub.onlyFromGenerated: %+v, want role generated", f)
	}
}

// Methods that external code calls through `any`: fmt calls String, encoding/json calls
// MarshalJSON on the value and, by reflection, on the types inside it.
func TestImplicitThroughAny(t *testing.T) {
	g := load(t, "testdata/implicit")
	for _, c := range []struct{ from, to string }{
		{"(root).encode", "((root).Color).MarshalJSON"},   // field of Wrapper, found by reflection
		{"(root).show", "((root).Sq).String"},             // own interface value converted to any
		{"(root).encodeDoc", "((root).Text).MarshalJSON"}, // implementer held by an interface-typed field
		{"(root).fail", "((root).MyErr).Is"},              // errors.Is calls Is on an error
	} {
		e := edge(g, c.from, c.to, "registers")
		if e == nil {
			t.Errorf("missing implicit edge %s -> %s", c.from, c.to)
			continue
		}
		if len(e.Flags) != 1 || e.Flags[0] != "implicit-iface" {
			t.Errorf("%s -> %s: flags %v, want [implicit-iface]", c.from, c.to, e.Flags)
		}
	}
	if f := fn(g, "((root).Color).MarshalJSON"); f == nil || f.Role != "implicit" {
		t.Errorf("Color.MarshalJSON: %+v, want role implicit", f)
	}
}

// Implicit edges are thinned out: none to a function that is called by name anyway, at most
// maxImplicitEdges to the rest; Function.ImplicitCallers keeps the real count.
func TestImplicitEdgesAreBounded(t *testing.T) {
	g := load(t, "testdata/implicit")
	count := func(to string) (n int) {
		for _, e := range g.Edges {
			if e.To == to && e.Kind == "registers" && len(e.Flags) == 1 && e.Flags[0] == "implicit-iface" {
				n++
			}
		}
		return
	}
	// five functions hand a Wrapper to json, so Color.MarshalJSON has five implicit callers
	if f := fn(g, "((root).Color).MarshalJSON"); f == nil || f.ImplicitCallers != 5 {
		t.Errorf("Color.MarshalJSON implicit callers: %+v, want 5", f)
	}
	if n := count("((root).Color).MarshalJSON"); n != maxImplicitEdges {
		t.Errorf("Color.MarshalJSON: %d implicit edges drawn, want %d", n, maxImplicitEdges)
	}
	// Sq2.String is called by name: the implicit caller is counted but its edge is not drawn
	if f := fn(g, "((root).Sq2).String"); f == nil || f.ImplicitCallers != 1 || f.Role != "" {
		t.Errorf("Sq2.String: %+v, want 1 implicit caller and no role", f)
	}
	if n := count("((root).Sq2).String"); n != 0 {
		t.Errorf("Sq2.String: %d implicit edges drawn, want 0 (it has a real caller)", n)
	}
}

// -entry: a file, a package or a function selects where the program starts.
func TestEntry(t *testing.T) {
	run := func(entry string) (*graph.Graph, error) {
		return Run(Options{Dir: "testdata/entry", Entry: entry})
	}
	for _, c := range []struct {
		entry string
		want  []string
		note  string
	}{
		{"main.go", []string{"(root).main"}, "a file that declares main: just main"},
		{"extra.go", []string{"(root).Extra", "(root).Other"}, "a file without main: the functions it declares"},
		{"main.go:run", []string{"(root).run"}, "file:function"},
		{"./", []string{"(root).main"}, "a main package: main"},
		{"lib", []string{"lib.Dead", "lib.Public", "lib.Used"}, "a library package: its exported functions"},
		{"lib:Used", []string{"lib.Used"}, "package:function"},
	} {
		g, err := run(c.entry)
		if err != nil {
			t.Errorf("entry %q (%s): %v", c.entry, c.note, err)
			continue
		}
		if strings.Join(g.Meta.Entries, ",") != strings.Join(c.want, ",") {
			t.Errorf("entry %q (%s): entries %v, want %v", c.entry, c.note, g.Meta.Entries, c.want)
		}
		if g.Meta.Entry != c.entry {
			t.Errorf("entry %q: meta.entry = %q", c.entry, g.Meta.Entry)
		}
	}
	// the entry only selects the package: lib is still loaded as a dependency of main
	g, _ := run("main.go")
	if fn(g, "lib.Used") == nil {
		t.Error("lib.Used (a dependency of the entry package) must be in the graph")
	}

	for _, bad := range []string{"nope.go", "main.go:missing", "lib/lib.go:Nope", "../outside"} {
		if _, err := run(bad); err == nil {
			t.Errorf("entry %q should be an error", bad)
		}
	}
	if _, err := Run(Options{Dir: "testdata/entry", Entry: "main.go", Patterns: []string{"./..."}}); err == nil {
		t.Error("-entry together with package patterns should be an error")
	}
}
