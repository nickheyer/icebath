package profiles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Where a credential comes from
type SourceKind string

const (
	// Value names an environment variable
	SourceEnv SourceKind = "env"
	// Value is the credential itself
	SourceStatic SourceKind = "static"
	// Value is a path whose trimmed contents are the credential
	SourceFile SourceKind = "file"
	// Value is a shell command whose trimmed output is the credential
	SourceExec SourceKind = "exec"
	// No credential, kept over the source of a template
	SourceNone SourceKind = "none"
)

// Budget for an exec source
const ExecTimeout = 15 * time.Second

// One credential source, the zero value yielding no credential
type Source struct {
	Kind  SourceKind `json:"kind,omitempty"`
	Value string     `json:"value,omitempty"`
}

func Env(name string) Source     { return Source{Kind: SourceEnv, Value: name} }
func Static(key string) Source   { return Source{Kind: SourceStatic, Value: key} }
func File(path string) Source    { return Source{Kind: SourceFile, Value: path} }
func Exec(command string) Source { return Source{Kind: SourceExec, Value: command} }

func (s Source) validate(profile string) error {
	switch s.Kind {
	case "", SourceNone:
		return nil
	case SourceEnv, SourceStatic, SourceFile, SourceExec:
		if s.Value == "" {
			return fmt.Errorf("profile %s: %s key source needs a value", profile, s.Kind)
		}
		return nil
	}
	return fmt.Errorf("profile %s: unknown key source %q", profile, s.Kind)
}

// Names the source without revealing a static credential
func (s Source) Describe() string {
	switch s.Kind {
	case "", SourceNone:
		return "no key source"
	case SourceStatic:
		return "static key"
	}
	return string(s.Kind) + " " + s.Value
}

// Produces the credential, empty with no error for the zero source
func (s Source) Resolve(ctx context.Context) (string, error) {
	switch s.Kind {
	case "", SourceNone:
		return "", nil
	case SourceEnv:
		return os.Getenv(s.Value), nil
	case SourceStatic:
		return s.Value, nil
	case SourceFile:
		data, err := os.ReadFile(s.Value)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	case SourceExec:
		ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", s.Value)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("exec key source: %w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	return "", fmt.Errorf("unknown key source %q", s.Kind)
}

// Accepts the object form or a string joining kind and value with a colon
func (s *Source) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		kind, value, ok := strings.Cut(text, ":")
		if !ok {
			return errors.New("key source string must look like kind:value")
		}
		*s = Source{Kind: SourceKind(kind), Value: value}
		return nil
	}
	var obj struct {
		Kind  SourceKind `json:"kind"`
		Value string     `json:"value"`
	}
	if err := json.Unmarshal(data, &obj); err != nil {
		return err
	}
	*s = Source(obj)
	return nil
}
