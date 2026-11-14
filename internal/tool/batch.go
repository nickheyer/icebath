package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/model/wire"
)

type Segment struct {
	Parallel bool
	Calls    []int
}

type NativeBlocks struct {
	Anthropic []json.RawMessage
	Bedrock   []json.RawMessage
}

type admission struct {
	paths  []string
	writer bool
}

type reservation struct {
	path   string
	writer bool
}

func Plan(ctx context.Context, calls []wire.ToolCall, r *Registry, at Scope, cwd string, deadline time.Duration) ([]Segment, error) {
	var segments []Segment
	var current []int
	var reserved []reservation
	var deferTools map[string]bool
	extendSequential := func(idx ...int) {
		if n := len(segments); n > 0 && !segments[n-1].Parallel {
			segments[n-1].Calls = append(segments[n-1].Calls, idx...)
			return
		}
		segments = append(segments, Segment{Calls: slices.Clone(idx)})
	}
	closeParallel := func() {
		if len(current) >= 2 {
			segments = append(segments, Segment{Parallel: true, Calls: current})
		} else if len(current) == 1 {
			extendSequential(current...)
		}
		current, reserved = nil, nil
	}
	for i, call := range calls {
		if call.Name == callToolName && deferTools == nil {
			cfg, err := r.searchConfig(ctx)
			if err != nil {
				return nil, err
			}
			deferTools = cfg.deferTools
		}
		adm, ok := r.admit(call, at, cwd, deadline, deferTools)
		if !ok {
			closeParallel()
			extendSequential(i)
			continue
		}
		conflict := false
		for _, p := range adm.paths {
			for _, ex := range reserved {
				if (adm.writer || ex.writer) && pathsOverlap(p, ex.path) {
					conflict = true
				}
			}
		}
		if conflict {
			closeParallel()
		}
		for _, p := range adm.paths {
			reserved = append(reserved, reservation{p, adm.writer})
		}
		current = append(current, i)
	}
	closeParallel()
	return segments, nil
}

func (r *Registry) concurrencyOf(name string, at Scope) (Concurrency, *Entry) {
	e, ok := r.Lookup(name, at)
	if !ok {
		return Sequential, nil
	}
	return e.Tool.Concurrency, e
}

func (r *Registry) admit(call wire.ToolCall, at Scope, cwd string, deadline time.Duration, deferTools map[string]bool) (admission, bool) {
	name := call.Name
	if c, _ := r.concurrencyOf(name, at); c == Never {
		return admission{}, false
	}
	node, err := parseNode([]byte(call.Arguments))
	if err != nil {
		slog.Debug(fmt.Sprintf("Could not parse args for %s — treating as sequential barrier; raw=%s", name, runePrefix(call.Arguments, 200)))
		return admission{}, false
	}
	if !node.isObj() {
		slog.Debug(fmt.Sprintf("Non-dict args for %s — treating as sequential barrier", name))
		return admission{}, false
	}
	args := Args(node.value().(map[string]any))
	if name == callToolName {
		if u, msg := r.resolveUnderlying(node, at, nil, deferTools); msg == "" && u.name != "" {
			name = u.name
			if u.name != ConnectorBatch {
				m, _ := u.args.value().(map[string]any)
				args = Args(m)
			}
		}
	}
	c, e := r.concurrencyOf(name, at)
	if c == Never || deadline > 0 && e != nil && e.Tool.ExecTimeout > deadline {
		return admission{}, false
	}
	if c == PathReader || c == PathWriter {
		var raw []string
		if e.Tool.ScopePaths != nil {
			raw = e.Tool.ScopePaths(args)
		}
		var paths []string
		for _, p := range raw {
			if trimWide(p) == "" {
				continue
			}
			if cp := canonicalPath(p, cwd); !slices.Contains(paths, cp) {
				paths = append(paths, cp)
			}
		}
		if len(paths) == 0 {
			return admission{}, false
		}
		return admission{paths: paths, writer: c == PathWriter}, true
	}
	if c == ParallelSafe || name == searchToolName || name == describeToolName || name == ConnectorBatch {
		return admission{}, true
	}
	return admission{}, false
}

func expandUser(p string) string {
	if !strings.HasPrefix(p, "~") {
		return p
	}
	rest := p[1:]
	i := strings.IndexFunc(rest, func(r rune) bool { return r == '/' || r == filepath.Separator })
	name, tail := rest, ""
	if i >= 0 {
		name, tail = rest[:i], rest[i:]
	}
	if name == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		return home + tail
	}
	u, err := user.Lookup(name)
	if err != nil {
		return p
	}
	return u.HomeDir + tail
}

func canonicalPath(raw, cwd string) string {
	p := expandUser(raw)
	if !filepath.IsAbs(p) {
		base := cwd
		if base == "" {
			wd, err := os.Getwd()
			if err == nil {
				base = wd
			}
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	if real, err := realpath(p); err == nil {
		p = real
	}
	return normcase(p)
}

func pathParts(p string) []string {
	if p == "" {
		return nil
	}
	vol := filepath.VolumeName(p)
	rest := p[len(vol):]
	anchor := vol
	if rest != "" && (rest[0] == '/' || rest[0] == filepath.Separator) {
		anchor += string(filepath.Separator)
	}
	parts := pathComponents(rest)
	if anchor != "" {
		parts = append([]string{anchor}, parts...)
	}
	return parts
}

func pathsOverlap(a, b string) bool {
	left, right := pathParts(a), pathParts(b)
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	n := min(len(left), len(right))
	return slices.Equal(left[:n], right[:n])
}

func ExpandLocalBatches(calls []wire.ToolCall, native *NativeBlocks) []wire.ToolCall {
	var expanded []wire.ToolCall
	replacements := map[string][]int{}
	for _, call := range calls {
		if call.Name != callToolName {
			expanded = append(expanded, call)
			continue
		}
		node, err := parseNode([]byte(call.Arguments))
		if err != nil || !node.isObj() {
			expanded = append(expanded, call)
			continue
		}
		entries, msg := normalizeEntries(node)
		connectorOnly := true
		for _, e := range entries {
			if !strings.HasPrefix(e.name, ConnectorPrefix) {
				connectorOnly = false
			}
		}
		if msg != "" || len(entries) <= 1 || len(entries) > MaxConnectorCalls || connectorOnly {
			expanded = append(expanded, call)
			continue
		}
		parent := call.EffectiveID()
		var idx []int
		for i, e := range entries {
			arguments := newObj().with("calls", newArr(newObj().with("name", newStr(e.name)).with("arguments", e.args))).spaced()
			child := call
			if i == 0 {
				child.Arguments = arguments
			} else {
				child = wire.ToolCall{ID: wire.DeterministicCallID(callToolName, parent+arguments, i), Name: callToolName, Arguments: arguments}
			}
			idx = append(idx, len(expanded))
			expanded = append(expanded, child)
		}
		replacements[parent] = idx
	}
	wire.UniquifyToolCallIDs(expanded, nil)
	if native != nil && len(replacements) > 0 {
		native.Anthropic = replaceNativeBlocks(native.Anthropic, expanded, replacements, false)
		native.Bedrock = replaceNativeBlocks(native.Bedrock, expanded, replacements, true)
	}
	return expanded
}

func replaceNativeBlocks(blocks []json.RawMessage, calls []wire.ToolCall, replacements map[string][]int, bedrock bool) []json.RawMessage {
	if len(blocks) == 0 {
		return blocks
	}
	var out []json.RawMessage
	for _, block := range blocks {
		v, err := wire.ParseValue(block)
		if err != nil || !v.IsObject() {
			out = append(out, block)
			continue
		}
		var children []int
		if bedrock {
			if tool := v.Get("toolUse"); tool.IsObject() {
				children = replacements[tool.Get("toolUseId").Text()]
			}
		} else if v.Get("type").Text() == "tool_use" {
			children = replacements[v.Get("id").Text()]
		}
		if children == nil {
			out = append(out, block)
			continue
		}
		for _, i := range children {
			input, err := parseNode([]byte(calls[i].Arguments))
			if err != nil {
				input = newObj()
			}
			id := wire.Quote(calls[i].EffectiveID())
			d := wire.DictOf(v)
			if bedrock {
				tool := wire.DictOf(v.Get("toolUse")).Set("toolUseId", id).Set("input", input.compact())
				d.Set("toolUse", tool.JSON())
			} else {
				d.Set("id", id).Set("input", input.compact())
			}
			out = append(out, d.JSON())
		}
	}
	return out
}
