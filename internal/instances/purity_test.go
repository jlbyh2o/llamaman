package instances

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Renderer purity as an executable property (DESIGN sections 5.7 and 15).
//
// Section 5.7 states it as a list of things RenderArgv does not do: it does not
// import internal/fit or internal/hw, does not read live VRAM, does not touch
// the clock, DOES NOT OPEN A FILE, and produces identical output for identical
// rows on any host. Section 15 asks for that as a test rather than a comment,
// because three separate guarantees rest on it — `instance-exec` can call it
// from a process with no D-Bus, no HTTP and no GPU probe; `config_hash` cannot
// move because free VRAM moved; and the golden files beside this test mean
// something.
//
// Stubbing the standard library to panic is not something a Go test can do, so
// the property is checked where it is actually decidable: the IMPORTS of the
// files that make up the two renderers. A renderer cannot read the clock without
// importing `time`, cannot open a file without `os` or `io`, and cannot consult
// the GPU without internal/hw — so a package-level import check is not a proxy
// for the rule, it is the rule.

// rendererFiles are the files RenderArgv, RenderBenchArgv, UnknownFlags and
// ConfigHash are built from. service.go is deliberately NOT among them: the
// service stamps timestamps and talks to a store, and it is the renderers that
// must stay pure.
var rendererFiles = []string{"render.go", "hash.go", "extraflags.go", "validate.go"}

// forbidden is what a pure renderer may not reach for. The two internal
// packages are named by section 5.7 directly; the rest are the mechanisms by
// which impurity would arrive.
var forbidden = map[string]string{
	"os":           "a renderer must not open a file, read an environment variable or stat a path",
	"io":           "a renderer must not read anything",
	"io/fs":        "a renderer must not read anything",
	"time":         "a renderer must not touch the clock; identical rows must render identically forever",
	"net":          "a renderer must not touch the network",
	"net/http":     "a renderer must not touch the network",
	"os/exec":      "a renderer produces argv; it never runs anything",
	"math/rand":    "a renderer must be deterministic",
	"database/sql": "a renderer takes rows, it does not fetch them",
	"github.com/jlbyh2o/llamaman/internal/fit":   "section 5.7: RenderArgv does not import internal/fit",
	"github.com/jlbyh2o/llamaman/internal/hw":    "section 5.7: RenderArgv does not import internal/hw — `-ngl auto` is resolved by llama.cpp, not here (D51)",
	"github.com/jlbyh2o/llamaman/internal/store": "a renderer takes rows, it does not query for them",
}

func TestRenderersArePure(t *testing.T) {
	fset := token.NewFileSet()

	for _, name := range rendererFiles {
		t.Run(name, func(t *testing.T) {
			f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parsing %s: %v", name, err)
			}
			for _, spec := range f.Imports {
				path, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatalf("unquoting an import path in %s: %v", name, err)
				}
				if why, bad := forbidden[path]; bad {
					t.Errorf("%s imports %q — %s", name, path, why)
				}
				if strings.HasPrefix(path, "github.com/jlbyh2o/llamaman/internal/") &&
					path != "github.com/jlbyh2o/llamaman/internal/model" {
					t.Errorf("%s imports %q; a renderer's only internal dependency is "+
						"internal/model (the FlagSet and the rows)", name, path)
				}
			}
		})
	}
}

// TestRenderersReachNoPackageState is the other half of "identical output for
// identical rows on any host": a package-level VARIABLE is state a renderer
// could read, and the only three that exist are effectively constants — two
// forbidden-flag lists and one compiled regexp, none of which anything writes.
func TestRenderersReachNoPackageState(t *testing.T) {
	fset := token.NewFileSet()

	allowed := map[string]bool{
		"forbiddenServerFlags": true,
		"forbiddenBenchFlags":  true,
		"nameRE":               true,
	}

	var found []string
	for _, name := range rendererFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for _, ident := range value.Names {
					if !allowed[ident.Name] {
						found = append(found, name+": "+ident.Name)
					}
				}
			}
		}
	}
	sort.Strings(found)
	if len(found) > 0 {
		t.Errorf("the renderers carry mutable package state, which is exactly what makes "+
			"output depend on something other than the rows: %v", found)
	}
}
