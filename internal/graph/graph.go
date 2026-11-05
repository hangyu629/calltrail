// Package graph defines the graph.json schema: the only contract between the
// analyzer and any renderer. It describes what is in the code, not how to draw it.
package graph

import "sort"

const SchemaVersion = "0.1-spike"

type Graph struct {
	SchemaVersion string     `json:"schema_version"`
	Meta          Meta       `json:"meta"`
	Packages      []Package  `json:"packages"`
	Functions     []Function `json:"functions"`
	Boundaries    []Boundary `json:"boundaries"`
	Edges         []Edge     `json:"edges"`
}

type Meta struct {
	Module         string   `json:"module"`
	Algo           string   `json:"algo"`
	GoVersion      string   `json:"go_version"`
	Patterns       []string `json:"patterns"`
	Tags           string   `json:"tags,omitempty"`
	Entry          string   `json:"entry,omitempty"`   // what the user gave to -entry
	Entries        []string `json:"entries,omitempty"` // the entry functions it resolved to
	TotalFunctions int      `json:"total_functions"`   // everything SSA saw, incl. std/third-party
	OwnFunctions   int      `json:"own_functions"`
}

type Package struct {
	ID   string `json:"id"` // module-relative path, "(root)" for the module root
	Path string `json:"path"`
	Kind string `json:"kind"` // own
}

type Function struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Package  string `json:"package"`
	Pos      string `json:"pos"` // file:line, module-relative
	Exported bool   `json:"exported"`
	// Role says why a function has (or lacks) callers; "" means "called by something".
	//   entry       main.main
	//   init        package initialisation (var initialisers + init funcs) of a package
	//   registered  its value is handed to external code (handler, goroutine body, ...)
	//   implicit    own type converted to an external interface / template data, or reached only
	//               by cha-fallback guesses: something we cannot see may call it
	//   generated   called only from generated code, which is not drawn (so it has no visible caller)
	//   iface       a method that implements an interface but is never called by name: it may be
	//               called through the interface by code we cannot see, or exist only to satisfy
	//               a (sealed) interface such as a `nodeImpl()` marker method
	//   api         exported, no caller in the module
	//   referenced  unexported, never called but used as a value somewhere
	//   unused      none of the above; the best dead-code candidates (still not proof:
	//               reflection by name, cgo exports and push-style linkname are not modelled)
	Role string `json:"role,omitempty"`
	// Implements lists (at most 4) interfaces this method belongs to: own ones first, then the
	// smallest. Only set for methods whose receiver type implements the whole interface.
	Implements []string `json:"implements,omitempty"`
	// ImplicitCallers counts the functions that may hand this function to external code (convert
	// its receiver to error / fmt.Stringer / any, ...). The graph draws few or none of these edges
	// (see analyze.maxImplicitEdges); this is the real number.
	ImplicitCallers int `json:"implicit_callers,omitempty"`
}

type Boundary struct {
	ID       string   `json:"id"`       // "ext:<std package | third-party module>"
	Path     string   `json:"path"`     // same without the prefix
	Category string   `json:"category"` // db | http | mq | fs | other
	Kind     string   `json:"kind"`     // std | third
	Packages []string `json:"packages"` // import paths folded into this node
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind: direct | dynamic | registers | boundary | callback
	//   callback: From invokes a closure that is *defined inside* To (closures are
	//   folded into their enclosing function, so this is not a call to To itself).
	Kind       string `json:"kind"`
	Mode       string `json:"mode"` // call | go | defer
	Candidates int    `json:"candidates,omitempty"`
	// Flags mark edges whose precision is known to be weak. A flag is set only if it
	// holds at every call site of the edge:
	//   external-iface: dynamic call on an interface declared outside the module
	//   table:          callee value comes from a map/slice element (data-dependent dispatch)
	//   cha-fallback:   a dynamic call that VTA resolved to nothing in the module (a library whose
	//                   concrete types come from outside); edges are every implementer (CHA)
	//   implicit-iface: (kind registers) an own value is converted to an external interface, so
	//                   external code may call its methods
	//   reflect:        (kind registers) own value given to a template: exported methods callable by name
	//   linkname:       body-less declaration bound to a function in another package by //go:linkname
	Flags  []string `json:"flags,omitempty"`
	Ifaces []string `json:"ifaces,omitempty"` // interfaces invoked at the sites
	Sites  []string `json:"sites"`
}

// Sort makes the output deterministic.
func (g *Graph) Sort() {
	sort.Slice(g.Packages, func(i, j int) bool { return g.Packages[i].ID < g.Packages[j].ID })
	sort.Slice(g.Functions, func(i, j int) bool { return g.Functions[i].ID < g.Functions[j].ID })
	sort.Slice(g.Boundaries, func(i, j int) bool { return g.Boundaries[i].ID < g.Boundaries[j].ID })
	sort.Slice(g.Edges, func(i, j int) bool {
		a, b := g.Edges[i], g.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Mode < b.Mode
	})
}
