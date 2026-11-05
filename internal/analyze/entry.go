package analyze

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hangyu629/calltrail/internal/graph"
)

// An entry says where the program starts. Spec forms (relative to Options.Dir):
//
//	cmd/server/main.go        a file: its package is analysed; the entries are `main` if the file
//	                          declares it, else every function declared in the file
//	./cmd/server              a package: `main` if it is a main package, else its exported functions
//	cmd/server:Run            a function (or Type.Method) of a package ...
//	cmd/server/main.go:Run    ... or of a file's package
//
// Go compiles packages, not files, so a file cannot be analysed on its own; it selects the package
// and, inside it, the functions to start from.
type entrySpec struct {
	pattern string // package pattern to load, relative to Dir
	dir     string // absolute directory of the package ("" when an import path was given)
	file    string // absolute path, when a file was given
	fn      string // function name, when given
}

func parseEntry(dir, spec string) (entrySpec, error) {
	path, fn := spec, ""
	if i := strings.LastIndex(spec, ":"); i > 0 {
		path, fn = spec[:i], spec[i+1:]
	}
	if path == "" {
		return entrySpec{}, fmt.Errorf("entry %q: empty path", spec)
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, path)
	}
	e := entrySpec{fn: fn}
	switch {
	case strings.HasSuffix(path, ".go"):
		if st, err := os.Stat(abs); err != nil || st.IsDir() {
			return e, fmt.Errorf("entry file %q does not exist", path)
		}
		e.file, e.dir = abs, filepath.Dir(abs)
	default:
		if st, err := os.Stat(abs); err == nil && st.IsDir() {
			e.dir = abs
		} else {
			e.pattern = path // not a directory: an import path, passed to go list as it is
		}
	}
	if e.dir != "" {
		rel, err := filepath.Rel(dir, e.dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			return e, fmt.Errorf("entry %q is outside the module directory %s", path, dir)
		}
		e.pattern = "."
		if rel != "." {
			e.pattern = "./" + filepath.ToSlash(rel)
		}
	}
	return e, nil
}

// resolveEntries finds the entry functions in the graph. pkgID is the entry's package.
func resolveEntries(g *graph.Graph, e entrySpec, pkgID, modDir string) ([]string, error) {
	var inPkg []graph.Function
	for _, f := range g.Functions {
		if f.Package == pkgID && f.Role != "init" {
			inPkg = append(inPkg, f)
		}
	}
	var ids []string
	switch {
	case e.fn != "":
		for _, f := range inPkg {
			if f.Label == e.fn || f.ID == e.fn || f.ID == pkgID+"."+e.fn {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("entry: no function %q in package %s", e.fn, pkgID)
		}
	case e.file != "":
		rel, err := filepath.Rel(modDir, e.file)
		if err != nil {
			return nil, err
		}
		prefix := filepath.ToSlash(rel) + ":"
		var inFile []graph.Function
		for _, f := range inPkg {
			if strings.HasPrefix(f.Pos, prefix) {
				inFile = append(inFile, f)
			}
		}
		for _, f := range inFile {
			if f.Role == "entry" { // main.main
				ids = []string{f.ID}
				break
			}
		}
		if ids == nil {
			for _, f := range inFile {
				ids = append(ids, f.ID)
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("entry: %s declares no functions", filepath.ToSlash(rel))
		}
	default:
		for _, f := range inPkg {
			if f.Role == "entry" {
				ids = []string{f.ID}
				break
			}
		}
		if ids == nil {
			for _, f := range inPkg {
				if f.Exported {
					ids = append(ids, f.ID)
				}
			}
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("entry: package %s has no main and no exported functions", pkgID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}
