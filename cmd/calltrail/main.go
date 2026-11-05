// calltrail: Go call-graph analyzer, in three layers.
//
//	raw.json     what is in the code (analyzer; x/tools)            calltrail analyze
//	model.json   raw + annotations: internal?, noise?, rules         calltrail annotate
//	viewer.html  one view of the model (self-contained page)         calltrail view
//
// `calltrail run` does all three and writes the three files into one directory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/hangyu629/calltrail/internal/analyze"
	"github.com/hangyu629/calltrail/internal/graph"
	"github.com/hangyu629/calltrail/internal/model"
	"github.com/hangyu629/calltrail/internal/render"
)

const usage = `calltrail: Go call graphs, in three layers (raw facts -> annotations -> a view)

usage:
  calltrail run      [flags] [patterns...]    all three layers; writes raw.json, model.json, viewer.html into -out-dir
  calltrail analyze  [flags] [patterns...]    layer 1 only: raw.json
  calltrail annotate [-rules r.json] [-o model.json] raw.json
                                              layer 2: model.json (raw + annotations)
  calltrail view     [-rules r.json] [-o viewer.html] model.json|raw.json
                                              layer 3: a self-contained html page
  calltrail demo     [flags] [patterns...]    analyze + annotate + view, only the html (-o)

flags (run / analyze / demo):
  -C dir        module directory to analyse (default .)
  -algo A       vta (default) | cha | static
  -tags t       build tags
  -entry E      where the program starts, instead of package patterns (see below)
  -rules f      ignore rules (run / demo)
  -out-dir d    output directory (run), default calltrail-out
  -o file       output file (analyze: raw.json, demo: calltrail.html)

-entry E, relative to -C:
  cmd/server/main.go       a file: its package is analysed; entries = main if the file declares it,
                           else every function the file declares
  ./cmd/server             a package: main if it is a main package, else its exported functions
  cmd/server:Run           a function (or Type.Method) of a package; also cmd/server/main.go:Run
The viewer then opens on the entry, shows its whole call tree, and can hide what it cannot reach.

rules.json: {"rules": [{"module": "golang.org/x/sys"}, {"package": "internal/gen/*"},
                       {"function": "internal/util.*"}, {"tag": "internal"}]}
  a rule hides what it matches; fields in one rule are ANDed; paths match at element boundaries
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "analyze":
		err = cmdAnalyze(args)
	case "annotate":
		err = cmdAnnotate(args)
	case "view":
		err = cmdView(args)
	case "run":
		err = cmdRun(args, false)
	case "demo":
		err = cmdRun(args, true)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "calltrail:", err)
		os.Exit(1)
	}
}

type analyzeFlags struct {
	dir, algo, tags, entry *string
}

func addAnalyzeFlags(fs *flag.FlagSet) analyzeFlags {
	return analyzeFlags{
		dir:   fs.String("C", ".", "module directory"),
		algo:  fs.String("algo", "vta", "vta | cha | static"),
		tags:  fs.String("tags", "", "build tags"),
		entry: fs.String("entry", "", "where the program starts: a file, a package or a function (see usage); replaces the package patterns"),
	}
}

func analyzeNow(f analyzeFlags, patterns []string) (*graph.Graph, error) {
	if st, err := os.Stat(*f.dir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("-C %q is not a directory", *f.dir)
	}
	if _, err := exec.LookPath("go"); err != nil {
		return nil, fmt.Errorf("the go tool is not on PATH; calltrail runs `go list` to load the code")
	}
	t0 := time.Now()
	g, err := analyze.Run(analyze.Options{Dir: *f.dir, Patterns: patterns, Tags: *f.tags, Algo: *f.algo, Entry: *f.entry})
	if err != nil {
		return nil, err
	}
	if len(g.Meta.Entries) > 0 {
		fmt.Fprintf(os.Stderr, "entry %q -> %s\n", g.Meta.Entry, strings.Join(g.Meta.Entries, ", "))
	}
	fmt.Fprintf(os.Stderr, "analyzed %s in %s: %d packages, %d own functions (of %d), %d edges, %d boundaries\n",
		g.Meta.Module, time.Since(t0).Round(time.Millisecond), len(g.Packages), len(g.Functions),
		g.Meta.TotalFunctions, len(g.Edges), len(g.Boundaries))
	return g, nil
}

func cmdAnalyze(args []string) error {
	fs := flag.NewFlagSet("analyze", flag.ExitOnError)
	af := addAnalyzeFlags(fs)
	out := fs.String("o", "raw.json", "output file")
	fs.Parse(args)
	g, err := analyzeNow(af, fs.Args())
	if err != nil {
		return err
	}
	return writeJSON(*out, g)
}

func cmdAnnotate(args []string) error {
	fs := flag.NewFlagSet("annotate", flag.ExitOnError)
	rulesPath := fs.String("rules", "", "rules file")
	out := fs.String("o", "model.json", "output file")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("annotate needs exactly one raw.json")
	}
	g, err := readRaw(fs.Arg(0))
	if err != nil {
		return err
	}
	rules, err := loadRules(*rulesPath)
	if err != nil {
		return err
	}
	return writeJSON(*out, model.Annotate(g, rules))
}

func cmdView(args []string) error {
	fs := flag.NewFlagSet("view", flag.ExitOnError)
	rulesPath := fs.String("rules", "", "rules file (only when the input is raw.json)")
	out := fs.String("o", "viewer.html", "output html")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("view needs exactly one model.json or raw.json")
	}
	b, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		return err
	}
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return err
	}
	var m model.Model
	if strings.HasPrefix(head.SchemaVersion, "model-") {
		if *rulesPath != "" {
			return fmt.Errorf("-rules only applies to raw.json; put the rules in the model with `calltrail annotate`")
		}
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
	} else {
		var g graph.Graph
		if err := json.Unmarshal(b, &g); err != nil {
			return err
		}
		rules, err := loadRules(*rulesPath)
		if err != nil {
			return err
		}
		m = *model.Annotate(&g, rules)
	}
	return writeHTML(&m, *out)
}

func cmdRun(args []string, htmlOnly bool) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	af := addAnalyzeFlags(fs)
	rulesPath := fs.String("rules", "", "rules file")
	outDir := fs.String("out-dir", "calltrail-out", "directory for raw.json, model.json, viewer.html")
	outHTML := fs.String("o", "calltrail.html", "output html (demo only)")
	fs.Parse(args)

	rules, err := loadRules(*rulesPath)
	if err != nil {
		return err
	}
	g, err := analyzeNow(af, fs.Args())
	if err != nil {
		return err
	}
	m := model.Annotate(g, rules)
	if htmlOnly {
		return writeHTML(m, *outHTML)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "raw.json"), g); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(*outDir, "model.json"), m); err != nil {
		return err
	}
	return writeHTML(m, filepath.Join(*outDir, "viewer.html"))
}

func loadRules(path string) ([]model.Rule, error) {
	if path == "" {
		return nil, nil
	}
	return model.LoadRules(path)
}

func readRaw(path string) (*graph.Graph, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var g graph.Graph
	if err := json.Unmarshal(b, &g); err != nil {
		return nil, err
	}
	if strings.HasPrefix(g.SchemaVersion, "model-") {
		return nil, fmt.Errorf("%s is already a model; annotate needs raw.json from `calltrail analyze`", path)
	}
	return &g, nil
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", path)
	return nil
}

func writeHTML(m *model.Model, path string) error {
	html, err := render.HTML(m)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, html, 0o644); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "wrote", path)
	return nil
}
