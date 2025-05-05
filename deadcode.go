package deadcode

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

var cwd, _ = os.Getwd()

func init() {
	register.Plugin("deadfunc", New)
}

func New(settings any) (register.LinterPlugin, error) {
	issues, err := runAnalysis()
	if err != nil {
		return nil, err
	}

	return &DeadCode{issues}, nil
}

func (d *DeadCode) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{
		{
			Name: "deadcode",
			Doc:  "finds unused funcs",
			Run:  d.run,
		},
	}, nil
}

func (d *DeadCode) run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		pos := pass.Fset.Position(file.Pos())
		for _, issue := range d.issues {
			if GetFilenameRelative(pos.Filename) == issue.Filename {
				pass.Report(analysis.Diagnostic{
					Pos:            issue.Pos,
					End:            0,
					Category:       "deadcode",
					Message:        fmt.Sprintf("unused func `%s`", issue.Name),
					SuggestedFixes: nil,
				})
			}
		}
	}

	return nil, nil
}

func (d *DeadCode) GetLoadMode() string {
	return register.LoadModeSyntax
}

func runAnalysis() ([]Issue, error) {
	testFlag := false
	filterFlag := "<module>"

	// Load, parse, and type-check the complete program(s).
	cfg := &packages.Config{
		Mode:  packages.LoadAllSyntax | packages.NeedModule,
		Tests: testFlag,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		log.Fatalf("Load: %v", err)
	}

	if len(initial) == 0 {
		log.Fatalf("no packages")
	}

	if packages.PrintErrors(initial) > 0 {
		log.Fatalf("packages contain errors")
	}

	// If -filter is unset, use first module (if available).
	if filterFlag == "<module>" {
		if mod := initial[0].Module; mod != nil && mod.Path != "" {
			filterFlag = "^" + regexp.QuoteMeta(mod.Path) + "\\b"
		} else {
			filterFlag = "" // match any
		}
	}

	filter, err := regexp.Compile(filterFlag)
	if err != nil {
		log.Fatalf("-filter: %v", err)
	}

	// Create SSA-form program representation and find main packages.
	prog, pkgs := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	mains := ssautil.MainPackages(pkgs)
	if len(mains) == 0 {
		log.Fatalf("no main packages")
	}

	var roots []*ssa.Function
	for _, main := range mains {
		roots = append(roots, main.Func("init"), main.Func("main"))
	}

	// Gather all source-level functions as the user interface is expressed in terms of them.
	var sourceFuncs []*ssa.Function
	generated := make(map[string]bool)
	packages.Visit(initial, nil, func(p *packages.Package) {
		for _, file := range p.Syntax {
			for _, decl := range file.Decls {
				if decl, ok := decl.(*ast.FuncDecl); ok {
					obj := p.TypesInfo.Defs[decl.Name].(*types.Func)
					fn := prog.FuncValue(obj)
					sourceFuncs = append(sourceFuncs, fn)
				}
			}

			if ast.IsGenerated(file) {
				generated[p.Fset.File(file.Pos()).Name()] = true
			}
		}
	})

	// Compute the reachabilty from main.
	res := rta.Analyze(roots, false)

	reachablePosn := make(map[token.Position]bool)
	for fn := range res.Reachable {
		if fn.Pos().IsValid() || fn.Name() == "init" {
			reachablePosn[prog.Fset.Position(fn.Pos())] = true
		}
	}

	// Group unreachable functions by package path.
	byPkgPath := make(map[string]map[*ssa.Function]bool)
	for _, fn := range sourceFuncs {
		posn := prog.Fset.Position(fn.Pos())

		if !reachablePosn[posn] {
			reachablePosn[posn] = true // suppress dups with same pos

			pkgpath := fn.Pkg.Pkg.Path()
			m, ok := byPkgPath[pkgpath]
			if !ok {
				m = make(map[*ssa.Function]bool)
				byPkgPath[pkgpath] = m
			}
			m[fn] = true
		}
	}

	// Build array of jsonPackage objects.
	var issues []Issue
	for _, pkgpath := range slices.Sorted(maps.Keys(byPkgPath)) {
		if !filter.MatchString(pkgpath) {
			continue
		}

		m := byPkgPath[pkgpath]

		for _, fn := range slices.Collect(maps.Keys(m)) {
			pos := prog.Fset.Position(fn.Pos())

			if generated[pos.Filename] {
				continue
			}

			issues = append(issues, Issue{
				Name:     fn.Name(),
				Filename: GetFilenameRelative(pos.Filename),
				Line:     pos.Line,
				Pos:      fn.Pos(),
			})
		}
	}

	return issues, nil
}

func GetFilenameRelative(filename string) string {
	if rel, err := filepath.Rel(cwd, filename); err == nil {
		return rel
	}
	return filename
}

type DeadCode struct {
	issues []Issue
}

type Issue struct {
	Name     string
	Filename string
	Line     int
	Pos      token.Pos
}

func ReceiverNamed(recv *types.Var) (isPtr bool, named *types.Named) {
	t := recv.Type()
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		isPtr = true
		t = ptr.Elem()
	}
	named, _ = types.Unalias(t).(*types.Named)
	return
}
