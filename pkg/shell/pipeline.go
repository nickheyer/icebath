package shell

import (
	"context"
	"strconv"
	"sync"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Hidden command every pipeline stage ends with, naming the pipeline, the stage and its exit code
const stageCmd = "__icebath_stage"

// Path an output process substitution writes to, opened on the stream of the line in progress
const stdoutPath = "/dev/icebath/stdout"

type stagesKey struct{}

// Exit codes of every pipeline a line runs, filled in as their stages finish
type stages struct {
	mu     sync.Mutex
	codes  [][]int
	names  [][]string
	latest int
}

// Registers a pipeline and returns its id
func (st *stages) add() int {
	st.codes = append(st.codes, nil)
	st.names = append(st.names, nil)
	return len(st.codes) - 1
}

// Sizes the pipeline for n stages, none reported yet
func (st *stages) size(id, n int) {
	codes := make([]int, n)
	for i := range codes {
		codes[i] = -1
	}
	st.codes[id] = codes
	st.names[id] = make([]string, n)
}

// Stores the exit code of one stage and remembers the pipeline as the latest to finish one
func (st *stages) record(id, idx, code int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if id < 0 || id >= len(st.codes) || idx < 0 || idx >= len(st.codes[id]) {
		return
	}
	st.codes[id][idx] = code
	st.latest = id
}

// Codes and command names of every stage of the latest pipeline, nothing before one ran or while a stage never reported
func (st *stages) last() ([]int, []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.latest < 0 {
		return nil, nil
	}
	codes := st.codes[st.latest]
	for _, c := range codes {
		if c < 0 {
			return nil, nil
		}
	}
	return append([]int(nil), codes...), append([]string(nil), st.names[st.latest]...)
}

// First word of a stage that is a plain call with a literal name, empty otherwise
func commandName(s *syntax.Stmt) string {
	call, ok := s.Cmd.(*syntax.CallExpr)
	if !ok || len(call.Args) == 0 {
		return ""
	}
	return call.Args[0].Lit()
}

// Reports the stage a hidden call names and ends with the stage's own exit code
func recordStage(ctx context.Context, args []string) error {
	if len(args) != 4 {
		return interp.ExitStatus(2)
	}
	id, _ := strconv.Atoi(args[1])
	idx, _ := strconv.Atoi(args[2])
	code, _ := strconv.Atoi(args[3])
	if st, ok := ctx.Value(stagesKey{}).(*stages); ok {
		st.record(id, idx, code)
	}
	if code == 0 {
		return nil
	}
	return interp.ExitStatus(uint8(code))
}

// Ends every pipeline stage with the hidden call reporting its code and points output process substitutions at the line's stream
func rewrite(prog *syntax.File) *stages {
	st := &stages{latest: -1}
	seen := map[*syntax.BinaryCmd]bool{}
	syntax.Walk(prog, func(n syntax.Node) bool {
		if ps, ok := n.(*syntax.ProcSubst); ok && ps.Op == syntax.CmdOut {
			ps.Stmts = []*syntax.Stmt{toStdout(ps)}
			return true
		}
		bc, ok := n.(*syntax.BinaryCmd)
		if !ok || !isPipe(bc.Op) || seen[bc] {
			return true
		}
		var slots []**syntax.Stmt
		chain(bc, &slots, seen)
		id := st.add()
		st.size(id, len(slots))
		for idx, slot := range slots {
			st.names[id][idx] = commandName(*slot)
			*slot = wrap(*slot, id, idx)
		}
		return true
	})
	return st
}

// Collects the stage slots of a pipeline in order, nested pipes on either side being links of it
func chain(bc *syntax.BinaryCmd, slots *[]**syntax.Stmt, seen map[*syntax.BinaryCmd]bool) {
	seen[bc] = true
	for _, slot := range []**syntax.Stmt{&bc.X, &bc.Y} {
		if link, ok := (*slot).Cmd.(*syntax.BinaryCmd); ok && isPipe(link.Op) && len((*slot).Redirs) == 0 && !(*slot).Negated {
			chain(link, slots, seen)
			continue
		}
		*slots = append(*slots, slot)
	}
}

func isPipe(op syntax.BinCmdOperator) bool { return op == syntax.Pipe || op == syntax.PipeAll }

// Statement running the substitution's body as a block with its output redirected to the shell's stdout path
func toStdout(ps *syntax.ProcSubst) *syntax.Stmt {
	pos := ps.Pos()
	redir := &syntax.Redirect{OpPos: pos, Op: syntax.RdrOut, Word: word(lit(pos, stdoutPath))}
	return &syntax.Stmt{Position: pos, Cmd: &syntax.Block{Lbrace: pos, Rbrace: pos, Stmts: ps.Stmts}, Redirs: []*syntax.Redirect{redir}}
}

// Statement running s in a subshell and then the hidden call with zero or with the subshell's exit code
func wrap(s *syntax.Stmt, id, idx int) *syntax.Stmt {
	pos := s.Pos()
	sub := &syntax.Stmt{Position: pos, Cmd: &syntax.Subshell{Lparen: pos, Rparen: pos, Stmts: []*syntax.Stmt{s}}}
	ok := &syntax.Stmt{Position: pos, Cmd: &syntax.BinaryCmd{OpPos: pos, Op: syntax.AndStmt, X: sub, Y: report(pos, id, idx, lit(pos, "0"))}}
	status := &syntax.ParamExp{Dollar: pos, Short: true, Param: &syntax.Lit{ValuePos: pos, ValueEnd: pos, Value: "?"}}
	return &syntax.Stmt{Position: pos, Cmd: &syntax.BinaryCmd{OpPos: pos, Op: syntax.OrStmt, X: ok, Y: report(pos, id, idx, status)}}
}

// Statement calling the hidden command with the pipeline, the stage and the code word
func report(pos syntax.Pos, id, idx int, code syntax.WordPart) *syntax.Stmt {
	args := []*syntax.Word{word(lit(pos, stageCmd)), word(lit(pos, strconv.Itoa(id))), word(lit(pos, strconv.Itoa(idx))), word(code)}
	return &syntax.Stmt{Position: pos, Cmd: &syntax.CallExpr{Args: args}}
}

func lit(pos syntax.Pos, value string) *syntax.Lit {
	return &syntax.Lit{ValuePos: pos, ValueEnd: pos, Value: value}
}

func word(part syntax.WordPart) *syntax.Word {
	return &syntax.Word{Parts: []syntax.WordPart{part}}
}
