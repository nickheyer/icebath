package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// Longest a quick question to a helper program may take, such as its version
const probeTimeout = 5 * time.Second

// What a helper program printed and how it ended
type Output struct {
	Stdout, Stderr string
	Code           int
}

// Runs a helper program with exactly the variables given, this process's for none, its output as text; a timeout fails with the message
// given, a program that would not start with the label
func Run(ctx context.Context, argv []string, env map[string]string, timeout time.Duration, label, timedOut string, stdin io.Reader) (Output, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if env != nil {
		cmd.Env = Pairs(env)
	}
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return Output{}, errors.New(timedOut)
	}
	out := Output{Stdout: strings.ToValidUTF8(stdout.String(), "�"), Stderr: strings.ToValidUTF8(stderr.String(), "�")}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		out.Code = exit.ExitCode()
		return out, nil
	}
	if err != nil {
		return Output{}, fmt.Errorf("icebath can't start %s: %v.", label, err)
	}
	return out, nil
}

// NAME=value pairs of a child's environment, sorted
func Pairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, name := range slices.Sorted(maps.Keys(env)) {
		out = append(out, name+"="+env[name])
	}
	return out
}

// Words of a helper's failure that say which kind it is, the first rule to match winning
type Rule struct {
	Kind  Failure
	Words []string
}

// Kind of a helper's failure by its text, internal when no rule matches
func Classify(text string, rules []Rule) Failure {
	lowered := strings.ToLower(text)
	for _, rule := range rules {
		if slices.ContainsFunc(rule.Words, func(word string) bool { return strings.Contains(lowered, word) }) {
			return rule.Kind
		}
	}
	return Internal
}

// First line of what the program says of its version, "version unknown" when it says nothing or fails
func Version(ctx context.Context, binary string, env map[string]string) string {
	out, err := Run(ctx, []string{binary, "--version"}, env, probeTimeout, binary, binary+" --version timed out", nil)
	if err != nil || out.Code != 0 {
		return "version unknown"
	}
	text := strings.TrimSpace(out.Stdout)
	if text == "" {
		text = strings.TrimSpace(out.Stderr)
	}
	if first, _, _ := strings.Cut(text, "\n"); strings.TrimSpace(first) != "" {
		return strings.TrimSpace(first)
	}
	return "version unknown"
}
