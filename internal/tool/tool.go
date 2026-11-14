// Tool contract, registry, toolsets and their resolution shared by every tool package
package tool

import (
	"context"
	"encoding/json"
	"math"
	"time"
)

type Tool struct {
	Name           string
	Toolset        string
	Description    string
	Parameters     json.RawMessage
	Handler        Handler
	Check          *Check
	RequiresEnv    []string
	Emoji          string
	MaxResultChars int
	DynamicSchema  func(ctx context.Context, available Names) (*Override, bool)
	ExecTimeout    time.Duration
	Concurrency    Concurrency
	ScopePaths     func(args Args) []string
	Effects        Effects
	Untrusted      bool
	Override       bool
	Owner          string
	Prepare        func(ctx context.Context, c *Call, args Args) error
}

type Handler func(ctx context.Context, c *Call, args Args) (Result, error)

type Args map[string]any

func (a Args) Decode(v any) error {
	raw, err := json.Marshal(map[string]any(a))
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (a Args) String(key string) (string, bool) {
	v, ok := a[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func (a Args) Clone() Args {
	if a == nil {
		return nil
	}
	out := make(Args, len(a))
	for k, v := range a {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = cloneValue(item)
		}
		return out
	case Args:
		return t.Clone()
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	}
	return v
}

type Override struct {
	Description *string
	Parameters  json.RawMessage
}

type Names map[string]bool

type Concurrency uint8

const (
	Sequential Concurrency = iota
	ParallelSafe
	PathReader
	PathWriter
	Never
)

type Effects uint8

const (
	MayHaveEffect Effects = 0
	NoEffect      Effects = 1 << (iota - 1)
	Idempotent
	Mutating
)

type Check struct {
	Name     string
	Probe    func(ctx context.Context) (bool, error)
	Uncached bool
}

const Unlimited = math.MaxInt

const DefaultEmoji = "⚡"
