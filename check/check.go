// Copyright (c) 2017, Daniel Martí <mvdan@mvdan.cc>
// See LICENSE for licensing information

// Package check implements the unparam linter. Note that its API is not
// stable.
package check

import (
	"fmt"
	"go/token"
	"go/types"
	"os"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/loader"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"

	"github.com/kisielk/gotool"
	"github.com/mvdan/lint"
)

func UnusedParams(tests bool, args ...string) ([]string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	c := &Checker{wd: wd, tests: tests}
	return c.lines(args...)
}

type Checker struct {
	lprog *loader.Program
	prog  *ssa.Program

	wd string

	tests bool
}

var (
	_ lint.Checker = (*Checker)(nil)
	_ lint.WithSSA = (*Checker)(nil)
)

func (c *Checker) lines(args ...string) ([]string, error) {
	paths := gotool.ImportPaths(args)
	var conf loader.Config
	if _, err := conf.FromArgs(paths, c.tests); err != nil {
		return nil, err
	}
	lprog, err := conf.Load()
	if err != nil {
		return nil, err
	}
	prog := ssautil.CreateProgram(lprog, 0)
	prog.Build()
	c.Program(lprog)
	c.ProgramSSA(prog)
	issues, err := c.Check()
	if err != nil {
		return nil, err
	}
	lines := make([]string, len(issues))
	for i, issue := range issues {
		fpos := prog.Fset.Position(issue.Pos()).String()
		if strings.HasPrefix(fpos, c.wd) {
			fpos = fpos[len(c.wd)+1:]
		}
		lines[i] = fmt.Sprintf("%s: %s", fpos, issue.Message())
	}
	return lines, nil
}

type Issue struct {
	pos token.Pos
	msg string
}

func (i Issue) Pos() token.Pos  { return i.pos }
func (i Issue) Message() string { return i.msg }

func (c *Checker) Program(lprog *loader.Program) {
	c.lprog = lprog
}

func (c *Checker) ProgramSSA(prog *ssa.Program) {
	c.prog = prog
}

func (c *Checker) Check() ([]lint.Issue, error) {
	wantPkg := make(map[*types.Package]bool)
	for _, info := range c.lprog.InitialPackages() {
		wantPkg[info.Pkg] = true
	}
	cg := cha.CallGraph(c.prog)

	var potential []*ssa.Parameter
funcLoop:
	for fn := range ssautil.AllFunctions(c.prog) {
		if fn.Pkg == nil { // builtin?
			continue
		}
		if len(fn.Blocks) == 0 { // stub
			continue
		}
		if !wantPkg[fn.Pkg.Pkg] { // not part of given pkgs
			continue
		}
		if dummyImpl(fn.Blocks[0]) { // panic implementation
			continue
		}
		for _, edge := range cg.Nodes[fn].In {
			switch edge.Site.Common().Value.(type) {
			case *ssa.Function:
			default:
				// called via a parameter or field, type
				// is set in stone.
				continue funcLoop
			}
		}
		for i, par := range fn.Params {
			if i == 0 && fn.Signature.Recv() != nil { // receiver
				continue
			}
			switch par.Object().Name() {
			case "", "_": // unnamed
				continue
			}
			if len(*par.Referrers()) > 0 { // used
				continue
			}
			potential = append(potential, par)
		}

	}
	// TODO: replace by sort.Slice once we drop Go 1.7 support
	sort.Sort(byPos(potential))

	var curPkg *types.Package
	issues := make([]lint.Issue, 0, len(potential))
	for _, par := range potential {
		pkg := par.Parent().Pkg
		// since they are sorted by position, we will see all
		// the warnings for any package contiguously
		if tpkg := pkg.Pkg; tpkg != curPkg {
			curPkg = tpkg
		}
		issues = append(issues, Issue{
			pos: par.Pos(),
			msg: fmt.Sprintf("%s is unused", par.Name()),
		})
	}
	return issues, nil
}

type byPos []*ssa.Parameter

func (p byPos) Len() int           { return len(p) }
func (p byPos) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p byPos) Less(i, j int) bool { return p[i].Pos() < p[j].Pos() }

var rxHarmlessCall = regexp.MustCompile(`(?i)\b(log(ger)?|errors)\b|\bf?print`)

// dummyImpl reports whether a block is a dummy implementation. This is
// true if the block will almost immediately panic, throw or return
// constants only.
func dummyImpl(blk *ssa.BasicBlock) bool {
	var ops [8]*ssa.Value
	for _, instr := range blk.Instrs {
		for _, val := range instr.Operands(ops[:0]) {
			switch x := (*val).(type) {
			case nil, *ssa.Const, *ssa.ChangeType, *ssa.Alloc,
				*ssa.MakeInterface, *ssa.Function,
				*ssa.Global, *ssa.IndexAddr, *ssa.Slice:
			case *ssa.Call:
				if rxHarmlessCall.MatchString(x.Call.Value.String()) {
					continue
				}
			default:
				return false
			}
		}
		switch x := instr.(type) {
		case *ssa.Alloc, *ssa.Store, *ssa.UnOp, *ssa.BinOp,
			*ssa.MakeInterface, *ssa.MakeMap, *ssa.Extract,
			*ssa.IndexAddr, *ssa.FieldAddr, *ssa.Slice,
			*ssa.Lookup, *ssa.ChangeType, *ssa.TypeAssert,
			*ssa.Convert, *ssa.ChangeInterface:
			// non-trivial expressions in panic/log/print
			// calls
		case *ssa.Return, *ssa.Panic:
			return true
		case *ssa.Call:
			if rxHarmlessCall.MatchString(x.Call.Value.String()) {
				continue
			}
			return x.Call.Value.Name() == "throw" // runtime's panic
		default:
			return false
		}
	}
	return false
}
