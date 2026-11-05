package model

import (
	"encoding/json"
	"testing"

	"github.com/hangyu629/calltrail/internal/graph"
)

func sample() *graph.Graph {
	return &graph.Graph{
		SchemaVersion: "raw-test",
		Meta:          graph.Meta{Module: "example.com/app"},
		Packages: []graph.Package{
			{ID: "(root)", Path: "example.com/app", Kind: "own"},
			{ID: "cmd/app", Path: "example.com/app/cmd/app", Kind: "own"},
			{ID: "internal/store", Path: "example.com/app/internal/store", Kind: "own"},
			{ID: "pkg/internal/deep", Path: "example.com/app/pkg/internal/deep", Kind: "own"},
		},
		Functions: []graph.Function{
			{ID: "cmd/app.main", Package: "cmd/app", Role: "entry", Exported: false},
			{ID: "internal/store.Get", Package: "internal/store", Exported: true},
			{ID: "internal/store.init", Package: "internal/store", Role: "init"}, // only touches fmt
			{ID: "cmd/app.init", Package: "cmd/app", Role: "init"},               // calls store: stays
			{ID: "pkg/internal/deep.F", Package: "pkg/internal/deep", Role: "api"},
		},
		Boundaries: []graph.Boundary{
			{ID: "ext:fmt", Path: "fmt", Kind: "std", Category: "other"},
			{ID: "ext:net/http", Path: "net/http", Kind: "std", Category: "http"},
			{ID: "ext:github.com/lib/pq", Path: "github.com/lib/pq", Kind: "third", Category: "db"},
			{ID: "ext:internal/abi", Path: "internal/abi", Kind: "std", Category: "other"},
		},
		Edges: []graph.Edge{
			{From: "cmd/app.main", To: "internal/store.Get", Kind: "direct", Mode: "call", Sites: []string{"a.go:1"}},
			{From: "cmd/app.main", To: "ext:fmt", Kind: "boundary", Mode: "call", Sites: []string{"a.go:2"}},
			{From: "cmd/app.main", To: "ext:net/http", Kind: "boundary", Mode: "call", Sites: []string{"a.go:3"}},
			{From: "internal/store.Get", To: "ext:github.com/lib/pq", Kind: "boundary", Mode: "call", Sites: []string{"s.go:1"}},
			{From: "internal/store.init", To: "ext:fmt", Kind: "boundary", Mode: "call", Sites: []string{"s.go:9"}},
			{From: "cmd/app.init", To: "internal/store.Get", Kind: "direct", Mode: "call", Sites: []string{"a.go:9"}},
			{From: "cmd/app.main", To: "pkg/internal/deep.F", Kind: "dynamic", Mode: "call", Flags: []string{"external-iface"}, Sites: []string{"a.go:4"}},
			{From: "cmd/app.main", To: "internal/store.Get", Kind: "registers", Mode: "call", Flags: []string{"implicit-iface"}, Sites: []string{"a.go:5"}},
			{From: "cmd/app.main", To: "internal/store.Get", Kind: "dynamic", Mode: "go", Flags: []string{"cha-fallback"}, Sites: []string{"a.go:6"}},
		},
	}
}

func TestAnnotate(t *testing.T) {
	m := Annotate(sample(), nil)

	// packages: only a path element called "internal" counts
	wantInternal := map[string]bool{"(root)": false, "cmd/app": false, "internal/store": true, "pkg/internal/deep": true}
	for _, p := range m.Packages {
		if p.Internal != wantInternal[p.ID] {
			t.Errorf("package %s: internal=%v, want %v", p.ID, p.Internal, wantInternal[p.ID])
		}
		if p.Module != "example.com/app" {
			t.Errorf("package %s: module %q", p.ID, p.Module)
		}
	}

	// boundaries: noise is hidden, real dependencies are not; std vs third-party module
	wantB := map[string]struct {
		hidden bool
		module string
	}{
		"ext:fmt": {true, "std"}, "ext:internal/abi": {true, "std"},
		"ext:net/http": {false, "std"}, "ext:github.com/lib/pq": {false, "github.com/lib/pq"},
	}
	for _, b := range m.Boundaries {
		w := wantB[b.ID]
		if b.Hidden != w.hidden || b.Module != w.module {
			t.Errorf("boundary %s: hidden=%v module=%q, want hidden=%v module=%q", b.ID, b.Hidden, b.Module, w.hidden, w.module)
		}
		if b.Hidden && b.HiddenBy != ReasonLowSignal {
			t.Errorf("boundary %s: hidden_by %q", b.ID, b.HiddenBy)
		}
	}

	// edges: reason by cause, direct edges untouched, ids unique
	byKey := map[string]Edge{}
	ids := map[string]bool{}
	for _, e := range m.Edges {
		if ids[e.ID] {
			t.Errorf("duplicate edge id %q", e.ID)
		}
		ids[e.ID] = true
		k := e.From + ">" + e.To + "|" + e.Kind + "|" + e.Mode
		byKey[k] = e
	}
	for k, want := range map[string]string{
		"cmd/app.main>internal/store.Get|direct|call":    "",
		"cmd/app.main>ext:fmt|boundary|call":             ReasonLowSignal,
		"cmd/app.main>ext:net/http|boundary|call":        "",
		"cmd/app.main>pkg/internal/deep.F|dynamic|call":  ReasonLowConfidence,
		"cmd/app.main>internal/store.Get|registers|call": ReasonImplicit,
		"cmd/app.main>internal/store.Get|dynamic|go":     ReasonLowConfidence,
	} {
		e, ok := byKey[k]
		if !ok {
			t.Errorf("edge %s missing", k)
			continue
		}
		if e.HiddenBy != want || e.Hidden != (want != "") {
			t.Errorf("edge %s: hidden=%v by %q, want by %q", k, e.Hidden, e.HiddenBy, want)
		}
	}

	// init nodes: hidden only when everything they touch is hidden
	for _, f := range m.Functions {
		switch f.ID {
		case "internal/store.init":
			if !f.Hidden || f.HiddenBy != ReasonLowSignal {
				t.Errorf("%s should be hidden (it only touches fmt)", f.ID)
			}
		case "cmd/app.init", "cmd/app.main":
			if f.Hidden {
				t.Errorf("%s should stay visible", f.ID)
			}
		}
	}
	// raw facts are untouched: same counts, same roles
	if len(m.Edges) != len(sample().Edges) || len(m.Functions) != 5 {
		t.Errorf("annotation must not add or remove facts: %d edges, %d functions", len(m.Edges), len(m.Functions))
	}
}

func TestRules(t *testing.T) {
	m := Annotate(sample(), nil)
	pkg := func(id string) Package {
		for _, p := range m.Packages {
			if p.ID == id {
				return p
			}
		}
		t.Fatalf("no package %s", id)
		return Package{}
	}
	bnd := func(id string) Boundary {
		for _, b := range m.Boundaries {
			if b.ID == id {
				return b
			}
		}
		t.Fatalf("no boundary %s", id)
		return Boundary{}
	}
	for _, c := range []struct {
		name string
		r    Rule
		pkg  string
		want bool
	}{
		{"prefix at an element boundary", Rule{Package: "internal"}, "internal/store", true},
		{"not a partial element", Rule{Package: "intern"}, "internal/store", false},
		{"exact", Rule{Package: "cmd/app"}, "cmd/app", true},
		{"glob", Rule{Package: "internal/*"}, "internal/store", true},
		{"module of own packages", Rule{Module: "example.com/app"}, "cmd/app", true},
		{"tag", Rule{Tag: "internal"}, "internal/store", true},
		{"tag absent", Rule{Tag: "internal"}, "cmd/app", false},
		{"fields are ANDed", Rule{Package: "cmd", Tag: "internal"}, "cmd/app", false},
		{"empty rule matches nothing", Rule{}, "cmd/app", false},
	} {
		if got := c.r.MatchesPackage(pkg(c.pkg)); got != c.want {
			t.Errorf("%s: rule %+v on package %s = %v, want %v", c.name, c.r, c.pkg, got, c.want)
		}
	}
	for _, c := range []struct {
		name string
		r    Rule
		id   string
		want bool
	}{
		{"third-party module", Rule{Module: "github.com/lib/pq"}, "ext:github.com/lib/pq", true},
		{"module is not a partial name", Rule{Module: "github.com/lib"}, "ext:github.com/lib/pq", true}, // prefix at an element boundary
		{"module partial element", Rule{Module: "github.com/li"}, "ext:github.com/lib/pq", false},
		{"std as a module", Rule{Module: "std"}, "ext:net/http", true},
		{"std does not match third-party", Rule{Module: "std"}, "ext:github.com/lib/pq", false},
		{"import path", Rule{Package: "net/http"}, "ext:net/http", true},
		{"category tag", Rule{Tag: "cat:db"}, "ext:github.com/lib/pq", true},
	} {
		if got := c.r.MatchesBoundary(bnd(c.id)); got != c.want {
			t.Errorf("%s: rule %+v on %s = %v, want %v", c.name, c.r, c.id, got, c.want)
		}
	}
}

func TestVisible(t *testing.T) {
	m := Annotate(sample(), nil)
	count := func(v *Model) (f, b, e int) { return len(v.Functions), len(v.Boundaries), len(v.Edges) }

	// default: the built-in policy hides noise and guesses
	v := m.Visible(ViewOptions{})
	f, b, e := count(v)
	if f != 4 || b != 2 || e != 4 {
		t.Errorf("default view: %d functions %d boundaries %d edges, want 4 2 4", f, b, e)
	}
	// show one reason
	v = m.Visible(ViewOptions{Show: map[string]bool{ReasonImplicit: true}})
	if _, _, e = count(v); e != 5 {
		t.Errorf("showing implicit edges: %d edges, want 5", e)
	}
	// a rule hides a package, its functions and every edge touching them
	v = m.Visible(ViewOptions{Rules: []Rule{{Package: "internal/store"}}})
	for _, fn := range v.Functions {
		if fn.Package == "internal/store" {
			t.Errorf("function %s of an ignored package is still visible", fn.ID)
		}
	}
	for _, ed := range v.Edges {
		if ed.From == "internal/store.Get" || ed.To == "internal/store.Get" {
			t.Errorf("edge %s touches an ignored package", ed.ID)
		}
	}
	// a rule hides a dependency by module
	v = m.Visible(ViewOptions{Rules: []Rule{{Module: "github.com/lib/pq"}}})
	for _, bd := range v.Boundaries {
		if bd.Module == "github.com/lib/pq" {
			t.Error("ignored module still visible")
		}
	}
	// the input is never modified
	if len(m.Edges) != len(sample().Edges) {
		t.Error("Visible modified the model")
	}
}

func TestDeterministicAndRoundTrip(t *testing.T) {
	a, _ := json.Marshal(Annotate(sample(), nil))
	b, _ := json.Marshal(Annotate(sample(), nil))
	if string(a) != string(b) {
		t.Error("annotation is not deterministic")
	}
	var back Model
	if err := json.Unmarshal(a, &back); err != nil {
		t.Fatal(err)
	}
	c, _ := json.Marshal(&back)
	if string(a) != string(c) {
		t.Error("model does not survive a JSON round trip")
	}
}

func TestParseRules(t *testing.T) {
	for _, in := range []string{
		`{"rules":[{"module":"golang.org/x/sys"},{"package":"internal/gen/*","note":"generated"}]}`,
		`[{"module":"golang.org/x/sys"},{"package":"internal/gen/*","note":"generated"}]`,
	} {
		rs, err := ParseRules([]byte(in))
		if err != nil || len(rs) != 2 || rs[0].ID != "r1" || rs[1].ID != "r2" {
			t.Errorf("ParseRules(%s) = %+v, %v", in, rs, err)
		}
	}
	if _, err := ParseRules([]byte(`[{"note":"nothing to match"}]`)); err == nil {
		t.Error("a rule with no module/package/tag must be rejected")
	}
	if _, err := ParseRules([]byte(`{`)); err == nil {
		t.Error("invalid JSON must be rejected")
	}
}

// Fan-in counts distinct own callers; guessed edges (low-confidence, implicit) do not count,
// low-signal ones do (they are real calls).
func TestFanIn(t *testing.T) {
	m := Annotate(sample(), nil)
	want := map[string]int{
		"internal/store.Get":    2, // main, cmd/app.init; the implicit and cha-fallback edges from main add nothing
		"pkg/internal/deep.F":   0, // reached only through a low-confidence edge
		"cmd/app.main":          0,
		"ext:fmt":               2, // main and store.init, although both edges are hidden by the policy
		"ext:net/http":          1,
		"ext:github.com/lib/pq": 1,
	}
	for _, f := range m.Functions {
		if w, ok := want[f.ID]; ok && f.FanIn != w {
			t.Errorf("function %s: fan_in %d, want %d", f.ID, f.FanIn, w)
		}
	}
	for _, b := range m.Boundaries {
		if w, ok := want[b.ID]; ok && b.FanIn != w {
			t.Errorf("boundary %s: fan_in %d, want %d", b.ID, b.FanIn, w)
		}
	}
}

func TestVisibleFanInThreshold(t *testing.T) {
	m := Annotate(sample(), nil)
	ids := func(v *Model) (fn, b map[string]bool) {
		fn, b = map[string]bool{}, map[string]bool{}
		for _, f := range v.Functions {
			fn[f.ID] = true
		}
		for _, x := range v.Boundaries {
			b[x.ID] = true
		}
		return
	}
	// dependencies called by more than one function are "common"
	_, b := ids(m.Visible(ViewOptions{Show: map[string]bool{ReasonLowSignal: true}, MaxFanInBoundary: 1}))
	if b["ext:fmt"] {
		t.Error("fmt (fan-in 2) should be hidden by the threshold even though low-signal is shown")
	}
	if !b["ext:net/http"] || !b["ext:github.com/lib/pq"] {
		t.Error("dependencies with fan-in 1 must stay")
	}
	// own functions called by more than one function
	f, _ := ids(m.Visible(ViewOptions{MaxFanInFunction: 1}))
	if f["internal/store.Get"] || !f["cmd/app.main"] {
		t.Errorf("function threshold: store.Get (2 callers) should go, main stay; got %v", f)
	}
	// threshold 0 = off
	f, _ = ids(m.Visible(ViewOptions{}))
	if !f["internal/store.Get"] {
		t.Error("no threshold must not hide anything")
	}
}

func TestFunctionRules(t *testing.T) {
	m := Annotate(sample(), nil)
	get := func(id string) Function {
		for _, f := range m.Functions {
			if f.ID == id {
				return f
			}
		}
		t.Fatalf("no function %s", id)
		return Function{}
	}
	for _, c := range []struct {
		name string
		r    Rule
		id   string
		want bool
	}{
		{"exact id", Rule{Function: "internal/store.Get"}, "internal/store.Get", true},
		{"other id", Rule{Function: "internal/store.Get"}, "cmd/app.main", false},
		{"prefix glob", Rule{Function: "internal/store.*"}, "internal/store.Get", true},
		{"no partial without glob", Rule{Function: "internal/store"}, "internal/store.Get", false},
		{"ANDed with package", Rule{Function: "internal/store.*", Package: "cmd"}, "internal/store.Get", false},
		{"ANDed with tag", Rule{Function: "internal/store.Get", Tag: "exported"}, "internal/store.Get", true},
		{"a package rule is not a function rule", Rule{Package: "internal/store"}, "internal/store.Get", false},
	} {
		if got := c.r.MatchesFunction(get(c.id)); got != c.want {
			t.Errorf("%s: %+v on %s = %v, want %v", c.name, c.r, c.id, got, c.want)
		}
	}
	// a function rule never hides a package or a dependency
	if (Rule{Function: "x*"}).MatchesPackage(m.Packages[0]) || (Rule{Function: "x*"}).MatchesBoundary(m.Boundaries[0]) {
		t.Error("function rules must not match packages or boundaries")
	}
	// and Visible applies it to the function (and its edges), nothing else
	v := m.Visible(ViewOptions{Rules: []Rule{{Function: "internal/store.Get"}}})
	for _, f := range v.Functions {
		if f.ID == "internal/store.Get" {
			t.Error("ignored function still visible")
		}
	}
	for _, e := range v.Edges {
		if e.From == "internal/store.Get" || e.To == "internal/store.Get" {
			t.Errorf("edge %s touches an ignored function", e.ID)
		}
	}
	if len(v.Packages) != len(m.Packages) {
		t.Error("ignoring a function must not hide its package")
	}
	if _, err := ParseRules([]byte(`[{"function":"a.B"}]`)); err != nil {
		t.Errorf("a function-only rule is valid: %v", err)
	}
}

func TestReachability(t *testing.T) {
	g := sample()
	g.Meta.Entries = []string{"cmd/app.main"}
	m := Annotate(g, nil)

	reached := map[string]bool{}
	for _, f := range m.Functions {
		reached[f.ID] = f.Reachable
	}
	for id, want := range map[string]bool{
		"cmd/app.main":        true,
		"internal/store.Get":  true, // main calls it
		"cmd/app.init":        true, // package initialisation runs before main
		"internal/store.init": true,
		"pkg/internal/deep.F": false, // main reaches it only through a low-confidence edge: a guess does not count
	} {
		if reached[id] != want {
			t.Errorf("function %s: reachable=%v, want %v", id, reached[id], want)
		}
	}
	for _, b := range m.Boundaries {
		want := b.ID != "ext:internal/abi" // nothing calls internal/abi
		if b.Reachable != want {
			t.Errorf("boundary %s: reachable=%v, want %v", b.ID, b.Reachable, want)
		}
	}
	for _, p := range m.Packages {
		// a package is reachable when one of its functions is: (root) has none, deep only an unreachable one
		want := p.ID == "cmd/app" || p.ID == "internal/store"
		if p.Reachable != want {
			t.Errorf("package %s: reachable=%v, want %v", p.ID, p.Reachable, want)
		}
	}

	// Visible: only the reachable part, packages and dependencies included
	v := m.Visible(ViewOptions{OnlyReachable: true, Show: map[string]bool{ReasonLowSignal: true, ReasonLowConfidence: true}})
	for _, f := range v.Functions {
		if f.ID == "pkg/internal/deep.F" {
			t.Error("unreachable function still visible")
		}
	}
	for _, p := range v.Packages {
		if p.ID == "pkg/internal/deep" {
			t.Error("package with nothing reachable still visible")
		}
	}
	for _, b := range v.Boundaries {
		if b.ID == "ext:internal/abi" {
			t.Error("unreferenced dependency still visible")
		}
	}
	// without entries nothing is reachable, and OnlyReachable must not hide everything
	m2 := Annotate(sample(), nil)
	if v2 := m2.Visible(ViewOptions{OnlyReachable: true}); len(v2.Functions) != len(m2.Visible(ViewOptions{}).Functions) {
		t.Error("OnlyReachable without entries must be a no-op")
	}
}

// Every hiding reason must be explained in both languages: the page picks title / title_en
// (and description / description_en) by the interface language and has no other source for them.
func TestPolicyHasBothLanguages(t *testing.T) {
	if len(Policy) == 0 {
		t.Fatal("no policy")
	}
	for _, p := range Policy {
		if p.ID == "" || p.Title == "" || p.Description == "" || p.TitleEN == "" || p.DescriptionEN == "" {
			t.Errorf("policy %q is missing text in one language: %+v", p.ID, p)
		}
	}
}
