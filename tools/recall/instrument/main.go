// Command instrument rewrites a copy of a Go module so that every function records
// "who called me" the first time each call site is executed. Used to build a runtime
// ground truth of call edges, to measure the recall of the static call graph.
//
//	go run ./tools/recall/instrument -src <module dir> -dst <new dir>
//
// Run the resulting binary with CALLTRAIL_TRACE=<file>; each line is
//
//	binary \t callee \t caller1|caller2|...   (runtime function names, innermost first, each as name@file)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

const traceSrc = `package calltrailtrace

import (
	"os"
	"runtime"
	"strings"
	"sync"
)

var (
	mu   sync.Mutex
	seen = map[[2]uintptr]bool{}
	out  *os.File
	once sync.Once
)

// E records the caller chain of the function that calls it, once per call site.
func E() {
	once.Do(func() {
		if p := os.Getenv("CALLTRAIL_TRACE"); p != "" {
			out, _ = os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		}
	})
	if out == nil {
		return
	}
	var pcs [2]uintptr
	if runtime.Callers(2, pcs[:]) < 2 {
		return
	}
	k := [2]uintptr{pcs[0], pcs[1]}
	mu.Lock()
	if seen[k] {
		mu.Unlock()
		return
	}
	seen[k] = true
	mu.Unlock()
	var big [48]uintptr
	n := runtime.Callers(2, big[:])
	fr := runtime.CallersFrames(big[:n])
	var names []string
	for {
		f, more := fr.Next()
		file := f.File
		if i := strings.LastIndexByte(file, '/'); i >= 0 {
			file = file[i+1:]
		}
		names = append(names, f.Function+"@"+file) // the file tells test code from library code
		if !more {
			break
		}
	}
	bin := os.Args[0]
	if i := strings.LastIndexByte(bin, '/'); i >= 0 {
		bin = bin[i+1:]
	}
	line := bin + "\t" + names[0] + "\t" + strings.Join(names[1:], "|") + "\n"
	mu.Lock()
	out.WriteString(line)
	mu.Unlock()
}
`

func main() {
	src := flag.String("src", "", "module directory to copy and instrument")
	dst := flag.String("dst", "", "destination directory (must not exist)")
	flag.Parse()
	if *src == "" || *dst == "" {
		flag.Usage()
		os.Exit(2)
	}
	if _, err := os.Stat(*dst); err == nil {
		fatal("destination %s already exists", *dst)
	}
	if out, err := exec.Command("cp", "-R", *src, *dst).CombinedOutput(); err != nil {
		fatal("copy: %v\n%s", err, out)
	}
	_ = exec.Command("chmod", "-R", "u+w", *dst).Run()

	mod := modulePath(filepath.Join(*dst, "go.mod"))
	tracePkg := mod + "/internal/calltrailtrace"
	must(os.MkdirAll(filepath.Join(*dst, "internal", "calltrailtrace"), 0o755))
	must(os.WriteFile(filepath.Join(*dst, "internal", "calltrailtrace", "trace.go"), []byte(traceSrc), 0o644))

	var files, funcs int
	err := filepath.WalkDir(*dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != *dst && (name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			if filepath.Base(p) == "calltrailtrace" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		n, err := instrumentFile(p, tracePkg)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if n > 0 {
			files++
			funcs += n
		}
		return nil
	})
	must(err)
	fmt.Printf("instrumented %d functions in %d files of module %s\n", funcs, files, mod)
}

func instrumentFile(path, tracePkg string) (int, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return 0, nil // not parseable on its own (e.g. template); leave untouched
	}
	if ast.IsGenerated(f) {
		return 0, nil // the analyzer drops generated code too
	}
	for _, im := range f.Imports {
		if im.Path.Value == `"C"` {
			return 0, nil // cgo
		}
	}
	n := 0
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Body == nil || fd.Name.Name == "init" || fd.Name.Name == "_" {
			continue
		}
		if fd.Doc != nil {
			skip := false
			for _, c := range fd.Doc.List {
				if strings.HasPrefix(c.Text, "//go:nosplit") || strings.HasPrefix(c.Text, "//go:norace") || strings.HasPrefix(c.Text, "//go:nocheckptr") {
					skip = true
				}
			}
			if skip {
				continue
			}
		}
		call := &ast.ExprStmt{X: &ast.CallExpr{Fun: &ast.SelectorExpr{X: ast.NewIdent("calltrailtrace"), Sel: ast.NewIdent("E")}}}
		fd.Body.List = append([]ast.Stmt{call}, fd.Body.List...)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	astutil.AddNamedImport(fset, f, "calltrailtrace", tracePkg)
	var buf bytes.Buffer
	if err := (&printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 8}).Fprint(&buf, fset, f); err != nil {
		return 0, err
	}
	return n, os.WriteFile(path, buf.Bytes(), 0o644)
}

func modulePath(gomod string) string {
	b, err := os.ReadFile(gomod)
	must(err)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(l, "module "))
		}
	}
	fatal("no module line in %s", gomod)
	return ""
}

func must(err error) {
	if err != nil {
		fatal("%v", err)
	}
}
func fatal(f string, a ...any) { fmt.Fprintf(os.Stderr, "instrument: "+f+"\n", a...); os.Exit(1) }
