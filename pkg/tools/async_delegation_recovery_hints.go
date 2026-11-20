package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	TranscriptTailLines = 20
	TranscriptTailChars = 2000
	recoveryGitTimeout  = 5 * time.Second
)

// Last lines of a live transcript bounded in characters, empty when unreadable or blank
func TranscriptTail(path string, lines, chars int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return ""
	}
	if _, err := f.Seek(max(0, size-int64(chars)*4), io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	all := pystr.Splitlines(pystr.DecodeUTF8Replace(data), false)
	tail := pystr.Strip(strings.Join(all[max(0, len(all)-lines):], "\n"))
	if rs := []rune(tail); len(rs) > chars {
		tail = "…" + string(rs[len(rs)-chars:])
	}
	return tail
}

// Tail of every readable transcript keyed by task index
func TranscriptTails(paths map[string]string) map[string]string {
	out := map[string]string{}
	for index, path := range paths {
		if tail := TranscriptTail(path, TranscriptTailLines, TranscriptTailChars); tail != "" {
			out[index] = tail
		}
	}
	return out
}

// Branch, short head and dirty count of the owner checkout through hardened git, empty when not a checkout
func GitStateHint(cwd string) string {
	if cwd == "" {
		return ""
	}
	if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		return ""
	}
	env := noninteractiveRepoGitEnv(cwd, nil)
	if env == nil {
		return ""
	}
	run := func(args ...string) (string, bool) {
		ctx, cancel := context.WithTimeout(context.Background(), recoveryGitTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, hardenGitArgv(args)...)...)
		cmd.Env = mapToEnviron(env)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", false
		}
		return pystr.DecodeUTF8Replace(out.Bytes()), true
	}
	head, ok := run("log", "-1", "--format=%h %s")
	if !ok {
		return ""
	}
	branch := "?"
	if b, ok := run("rev-parse", "--abbrev-ref", "HEAD"); ok && b != "" {
		branch = b
	}
	branch = pystr.Strip(branch)
	dirty := "?"
	if status, ok := run("status", "--porcelain"); ok {
		dirty = strconv.Itoa(len(pystr.Splitlines(status, false)))
	}
	return fmt.Sprintf("%s @ %s (%s uncommitted file(s)) in %s", branch, pystr.Strip(head), dirty, cwd)
}
