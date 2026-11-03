package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/agent"
)

// Characters an agent name may hold at most
const agentNameMax = 256

// Subagent definition, a system prompt with the tools, model and limits it runs under
type Agent struct {
	// Full name the Agent tool spawns it by, plugin:name for a plugin agent
	Name string
	// Name without any plugin prefix
	Short       string
	Description string
	// Tools offered, every tool when empty
	Tools []string
	// Tools withheld, applied before the offered list
	DisallowedTools []string
	// Model alias, id or inherit
	Model          string
	PermissionMode string
	MaxTurns       int
	// Context tokens at which compaction runs, inherited when zero
	AutoCompactWindow int
	// Skills whose content is preloaded, the first thirty two
	Skills []string
	// Servers connected while the agent runs, by name or inline
	MCPServers []ServerRef
	// Hooks active while the agent runs, Stop read as SubagentStop
	Hooks agent.HookConfig
	// Memory scope, user, project or local
	Memory string
	// Stays in the background even when the parent asks to wait
	Background bool
	// Skips the project instruction files
	OmitInstructions bool
	Effort           string
	// Worktree when it runs in a temporary git worktree
	Isolation string
	Color     string
	// First user turn auto submitted when the agent runs as the main session
	InitialPrompt string
	// System prompt, the markdown body
	Prompt string
	File   string
	Plugin *Plugin
	Origin string
	// Errors found reading the agent
	Problems []string
	// Fields the file set that the format does not know
	Unknown []string
}

// MCP server an agent names, a reference to a configured one or an inline definition
type ServerRef struct {
	Name   string
	Inline *Server
}

// Fields an agent file may set
var agentFields = []string{"name", "description", "tools", "disallowedTools", "model", "permissionMode", "maxTurns", "autoCompactWindow", "skills", "mcpServers", "hooks", "memory", "background", "omitClaudeMd", "effort", "isolation", "color", "initialPrompt", "experimental", "license", "metadata"}

// Values the permission mode of an agent may take
var permissionModes = []string{"", "default", "manual", "acceptEdits", "auto", "dontAsk", "bypassPermissions", "plan"}

// Values the color of an agent may take
var agentColors = []string{"", "red", "blue", "green", "yellow", "purple", "orange", "pink", "cyan"}

// Parses an agent file, the file name standing in for a missing name when lenient
func ParseAgent(text, path string, lenient bool) (*Agent, bool) {
	f := ParseFront(text)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	a := &Agent{File: path, Prompt: strings.TrimSpace(f.Body)}
	if !f.Present || f.Err != nil {
		if !lenient {
			return nil, false
		}
		a.Short, a.Description = base, "Agent from plugin"
		if f.Err != nil {
			a.Problems = append(a.Problems, "frontmatter does not parse: "+f.Err.Error())
		}
		return a, true
	}
	a.Short = strings.TrimSpace(f.String("name"))
	a.Description = strings.TrimSpace(f.String("description"))
	switch {
	case a.Short == "" && !lenient:
		return nil, false
	case a.Short == "":
		a.Short = base
	case len(a.Short) > agentNameMax:
		if !lenient {
			return nil, false
		}
		a.Problems = append(a.Problems, "name is over 256 characters, the file name is used")
		a.Short = base
	case strings.HasPrefix(a.Short, "-") || strings.Contains(a.Short, ":"):
		if !lenient {
			return nil, false
		}
		a.Problems = append(a.Problems, "name starts with a dash or holds a colon, the file name is used")
		a.Short = base
	}
	if a.Description == "" {
		if !lenient {
			return nil, false
		}
		a.Description = "Agent from plugin"
	}
	a.Tools = f.Rules("tools")
	a.DisallowedTools = f.Rules("disallowedTools")
	a.Model = strings.TrimSpace(f.String("model"))
	a.PermissionMode = strings.TrimSpace(f.String("permissionMode"))
	if a.PermissionMode == "manual" {
		a.PermissionMode = "default"
	}
	if err := checkChoice("permissionMode", a.PermissionMode, permissionModes); err != nil {
		a.Problems = append(a.Problems, err.Error())
		a.PermissionMode = ""
	}
	a.MaxTurns = f.Int("maxTurns")
	if w := f.Int("autoCompactWindow"); w >= 100000 && w <= 1000000 {
		a.AutoCompactWindow = w
	} else if f.Has("autoCompactWindow") {
		a.Problems = append(a.Problems, "invalid autoCompactWindow, a plain integer from 100000 to 1000000 is needed")
	}
	a.Skills = f.List("skills")
	if len(a.Skills) > 32 {
		a.Skills = a.Skills[:32]
	}
	a.Memory = strings.TrimSpace(f.String("memory"))
	if err := checkChoice("memory", a.Memory, []string{"", "user", "project", "local"}); err != nil {
		a.Problems = append(a.Problems, err.Error())
		a.Memory = ""
	}
	a.Background = f.Bool("background", false)
	a.OmitInstructions = f.Bool("omitClaudeMd", false)
	a.Effort = strings.TrimSpace(f.String("effort"))
	if err := checkChoice("effort", a.Effort, choices["effort"]); err != nil {
		a.Problems = append(a.Problems, err.Error())
		a.Effort = ""
	}
	a.Isolation = strings.TrimSpace(f.String("isolation"))
	if err := checkChoice("isolation", a.Isolation, []string{"", "worktree"}); err != nil {
		a.Problems = append(a.Problems, err.Error())
		a.Isolation = ""
	}
	a.Color = strings.TrimSpace(f.String("color"))
	if err := checkChoice("color", a.Color, agentColors); err != nil {
		a.Problems = append(a.Problems, err.Error())
		a.Color = ""
	}
	a.InitialPrompt = strings.TrimSpace(f.String("initialPrompt"))
	if raw := f.Map("hooks"); raw != nil {
		cfg, err := hooksFromMap(raw)
		if err != nil {
			a.Problems = append(a.Problems, "hooks: "+err.Error())
		} else {
			a.Hooks = cfg.ForSubagent().Labeled(agent.SourceAgent, a.Short, nil)
		}
	}
	if v, ok := f.Fields["mcpServers"]; ok {
		refs, err := serverRefs(v)
		if err != nil {
			a.Problems = append(a.Problems, "mcpServers: "+err.Error())
		}
		a.MCPServers = refs
	}
	a.Unknown = f.Unknown(agentFields...)
	return a, true
}

// Server references of an agent, strings naming configured servers and maps defining inline ones
func serverRefs(v any) ([]ServerRef, error) {
	var out []ServerRef
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			switch it := item.(type) {
			case string:
				out = append(out, ServerRef{Name: it})
			case map[string]any:
				for name, def := range it {
					srv, err := serverFromAny(name, def)
					if err != nil {
						return out, err
					}
					out = append(out, ServerRef{Name: name, Inline: srv})
				}
			default:
				return out, fmt.Errorf("entry %v is neither a name nor a server", item)
			}
		}
	case map[string]any:
		for name, def := range t {
			srv, err := serverFromAny(name, def)
			if err != nil {
				return out, err
			}
			out = append(out, ServerRef{Name: name, Inline: srv})
		}
	default:
		return nil, fmt.Errorf("expected a list or a map")
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Server read from a YAML value by way of its JSON
func serverFromAny(name string, def any) (*Server, error) {
	raw, err := json.Marshal(def)
	if err != nil {
		return nil, err
	}
	return parseServer(name, raw)
}

// Reads every agent file under a directory, walking subdirectories, with a lenient read for plugin agents
func readAgentDir(dir string, lenient bool, prefix bool) ([]*Agent, error) {
	var out []*Agent
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		a, ok := ParseAgent(string(data), path, lenient)
		if !ok {
			return nil
		}
		if prefix {
			if rel, err := filepath.Rel(dir, filepath.Dir(path)); err == nil && rel != "." {
				a.Short = strings.ReplaceAll(filepath.ToSlash(rel), "/", ":") + ":" + a.Short
			}
		}
		out = append(out, a)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Reads one agent file as a plugin manifest names it, the file name standing in for a missing name
func readAgentFile(path string) (*Agent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	a, _ := ParseAgent(string(data), path, true)
	return a, nil
}
