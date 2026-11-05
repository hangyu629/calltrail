package analyze

import (
	"go/types"
	"strings"

	"golang.org/x/tools/go/ssa"
)

// funcValue resolves an SSA value that is a function/method value
// (f, s.Method, http.HandlerFunc(f), closures) to the function it denotes.
func (a *analyzer) funcValue(v ssa.Value) *ssa.Function {
	for depth := 0; depth < 4; depth++ {
		switch x := v.(type) {
		case *ssa.Function:
			return a.unwrap(x)
		case *ssa.MakeClosure:
			if fn, ok := x.Fn.(*ssa.Function); ok {
				return a.unwrap(fn)
			}
			return nil
		case *ssa.ChangeType:
			v = x.X
		case *ssa.Convert:
			v = x.X
		case *ssa.MakeInterface:
			v = x.X
		default:
			return nil
		}
	}
	return nil
}

// unwrap looks through synthetic wrappers (bound method values etc.) to the real callee.
func (a *analyzer) unwrap(fn *ssa.Function) *ssa.Function {
	for d := 0; d < 3 && fn.Synthetic != ""; d++ {
		var next *ssa.Function
	scan:
		for _, b := range fn.Blocks {
			for _, in := range b.Instrs {
				if c, ok := in.(ssa.CallInstruction); ok {
					if s := c.Common().StaticCallee(); s != nil {
						next = s
						break scan
					}
				}
			}
		}
		if next == nil {
			return fn
		}
		fn = next
	}
	return fn
}

// category is a first-cut heuristic; the real rule table should be configurable.
func category(p string) string {
	has := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(p, s) {
				return true
			}
		}
		return false
	}
	switch {
	case has("database/sql", "gorm", "sqlx", "redis", "mongo", "pgx", "bun", "ent/", "bolt", "badger", "leveldb"):
		return "db"
	case has("kafka", "amqp", "nats", "rabbit", "pubsub", "sarama", "sqs", "nsq"):
		return "mq"
	case has("net/http", "grpc", "net/rpc", "resty", "fasthttp", "gin-gonic", "labstack/echo", "go-chi", "gorilla/mux", "net/url", "websocket", "net"):
		if p == "net/url" {
			return "other"
		}
		return "http"
	case p == "os" || has("io/fs", "path/filepath", "io/ioutil", "bufio", "os/exec", "afero", "fsnotify", "embed"):
		return "fs"
	}
	return "other"
}

// isClosure reports whether fn is an anonymous function nested in another function.
func (a *analyzer) isClosure(fn *ssa.Function) bool {
	if o := fn.Origin(); o != nil {
		fn = o
	}
	return fn.Parent() != nil
}

// siteInfo classifies a dynamic call site. Findings from the 25-edge precision audit
// (docs/precision-audit-2026-11-05.md):
//   - an interface declared outside the module with an own implementer is the riskiest
//     pattern: the own type merely happens to implement it, the value rarely flows here;
//   - a callee fetched from a map/slice element dispatches on data, so every candidate is
//     "possible" but only one runs.
func (a *analyzer) siteInfo(c *ssa.CallCommon) siteInfo {
	if c.IsInvoke() {
		name, ext := "error", true // the universe `error` has no package
		if sig, ok := c.Method.Type().(*types.Signature); ok && sig.Recv() != nil {
			if n, ok := types.Unalias(sig.Recv().Type()).(*types.Named); ok && n.Obj().Pkg() != nil {
				name = n.Obj().Pkg().Path() + "." + n.Obj().Name()
				ext = !a.inMod(n.Obj().Pkg().Path())
			} else if c.Method.Pkg() != nil { // anonymous interface literal
				name = c.Method.Pkg().Path() + ".(interface)"
				ext = !a.inMod(c.Method.Pkg().Path())
			}
		}
		info := siteInfo{iface: name}
		if ext {
			info.flags = []string{"external-iface"}
		}
		return info
	}
	v := c.Value
	for depth := 0; depth < 6; depth++ {
		switch x := v.(type) {
		case *ssa.Lookup, *ssa.IndexAddr, *ssa.Index:
			return siteInfo{flags: []string{"table"}}
		case *ssa.Extract:
			v = x.Tuple
		case *ssa.UnOp:
			v = x.X
		case *ssa.ChangeType:
			v = x.X
		case *ssa.TypeAssert:
			v = x.X
		default:
			return siteInfo{}
		}
	}
	return siteInfo{}
}
