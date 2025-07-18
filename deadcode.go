package deadcode

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"os"
	"path/filepath"
	"regexp"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

var cwd, _ = os.Getwd()

// DeadCode instance linter.
type DeadCode struct {
	issues []Issue
}

// Issue from linter.
type Issue struct {
	Func     string
	Filename string
	Line     int
}

// Settings linter.
type Settings struct {
	Test   bool   `json:"test"`
	Filter string `json:"filter"`
}

func init() {
	register.Plugin("deadcode", NewDeadCode)
}

// NewDeadCode retuns new instance linter.
func NewDeadCode(settings any) (register.LinterPlugin, error) {
	s, err := register.DecodeSettings[Settings](settings)
	if err != nil {
		return nil, err
	}

	issues, err := runAnalysis(s)
	if err != nil {
		return nil, err
	}

	return &DeadCode{issues}, nil
}

func (d *DeadCode) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{
		{
			Name: "deadcode",
			Doc:  "finds unreachable funcs.",
			Run:  d.run,
		},
	}, nil
}

func (d *DeadCode) run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		filename := Rel(pass.Fset.Position(file.Pos()).Filename)
		for _, issue := range d.issues {
			if filename != issue.Filename {
				continue
			}

			ast.Inspect(file, func(n ast.Node) bool {
				funcDecl, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}

				funcDeclPos := pass.Fset.Position(funcDecl.Pos())
				if funcDeclPos.Line == issue.Line {
					pass.Report(analysis.Diagnostic{
						Pos:            funcDecl.Pos(),
						End:            0,
						Message:        fmt.Sprintf("func `%s` is unused", issue.Func),
						SuggestedFixes: nil,
					})
				}

				return true
			})
		}
	}
	return nil, nil
}

func (d *DeadCode) GetLoadMode() string {
	return register.LoadModeSyntax
}

func runAnalysis(settings Settings) ([]Issue, error) {
	testFlag := settings.Test
	filterFlag := settings.Filter

	// Load, parse, and type-check the complete program(s).
	cfg := &packages.Config{
		Mode:  packages.LoadAllSyntax | packages.NeedModule,
		Tests: testFlag,
	}

	initial, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, fmt.Errorf("Load: %v", err)
	}

	if len(initial) == 0 {
		return nil, errors.New("no find packages")
	}

	if packages.PrintErrors(initial) > 0 {
		return nil, errors.New("packages contain errors")
	}

	var filter *regexp.Regexp

	// If -filter is unset, use first module (if available).
	if filterFlag != "" {
		filter, err = regexp.Compile(filterFlag)
		if err != nil {
			return nil, fmt.Errorf("failed create filter: %v", err)
		}
	}

	// Create SSA-form program representation and find main packages.
	prog, pkgs := ssautil.AllPackages(initial, ssa.InstantiateGenerics)
	prog.Build()

	mains := ssautil.MainPackages(pkgs)
	if len(mains) == 0 {
		return nil, errors.New("no find main packages")
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
	for pkgpath := range maps.Keys(byPkgPath) {
		if filter != nil && !filter.MatchString(pkgpath) {
			continue
		}

		m := byPkgPath[pkgpath]

		for fn := range maps.Keys(m) {
			pos := prog.Fset.Position(fn.Pos())

			if generated[pos.Filename] {
				continue
			}

			issues = append(issues, Issue{
				Func:     fn.Name(),
				Filename: Rel(pos.Filename),
				Line:     pos.Line,
			})
		}
	}

	return issues, nil
}

// Rel returns a relative path.
func Rel(filename string) string {
	if rel, err := filepath.Rel(cwd, filename); err == nil {
		return rel
	}
	return filename
}

// ReceiverNamed returns the named type (if any) associated with the
// type of recv, which may be of the form N or *N, or aliases thereof.
func ReceiverNamed(recv *types.Var) (isPtr bool, named *types.Named) {
	t := recv.Type()
	if ptr, ok := types.Unalias(t).(*types.Pointer); ok {
		isPtr = true
		t = ptr.Elem()
	}
	named, _ = types.Unalias(t).(*types.Named)
	return
}
