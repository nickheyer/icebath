package specprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Hidden verb under which a re-executed binary answers find_spec questions inside an interpreter's environment
const Entry = "pm-find-spec"

// One reply line, whether the module has a spec or the exception find_spec raised past ImportError and ValueError
type Answer struct {
	Found bool   `json:"found"`
	Error string `json:"error,omitempty"`
}

// An exception find_spec raised for a module that is neither ImportError nor ValueError
type FindSpecError struct {
	Module  string
	Message string
}

func (e *FindSpecError) Error() string {
	return fmt.Sprintf("find_spec(%q) raised %s", e.Module, e.Message)
}

// Asks a child of this binary hosting python with its environment whether each module has a spec, in order
func FindSpecs(ctx context.Context, python, dir string, env []string, modules []string) ([]bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var questions bytes.Buffer
	for _, module := range modules {
		line, err := json.Marshal(module)
		if err != nil {
			return nil, err
		}
		questions.Write(line)
		questions.WriteByte('\n')
	}
	cmd := exec.CommandContext(ctx, exe, Entry, "--python", python)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, &questions
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", Entry, python, err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(&stdout)
	found := make([]bool, 0, len(modules))
	for _, module := range modules {
		var reply Answer
		if err := decoder.Decode(&reply); err != nil {
			return nil, fmt.Errorf("%s %s: answer for %q: %w", Entry, python, module, err)
		}
		if reply.Error != "" {
			return nil, &FindSpecError{Module: module, Message: reply.Error}
		}
		found = append(found, reply.Found)
	}
	return found, nil
}
