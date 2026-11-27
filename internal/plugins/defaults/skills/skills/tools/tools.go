package tools

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/terminal"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
	"github.com/nickheyer/icebath/internal/tools"
	"github.com/nickheyer/icebath/pkg/strs"
)

const toolset = "skills"

// Text of a repeated view a session already holds
const unchanged = "Skill content unchanged since it was loaded earlier in this conversation — refer to the earlier skill_view result; it is still current and complete. (Re-issued after context compression, this returns the full content again.)"

// Views a session remembers, oldest dropped first
const remembered = 200

type registrant struct {
	skills   *skills.Registry
	terminal *terminal.Registry

	mu sync.Mutex
	// Digests of the views each session was served, by skill and file
	views map[string]*served
}

type served struct {
	order   []string
	digests map[string]string
}

func Register(registry *tools.Registry, skillset *skills.Registry, backends *terminal.Registry) {
	registry.Register(&registrant{skills: skillset, terminal: backends, views: map[string]*served{}})
}

func (r *registrant) Tools() []tools.Def {
	return []tools.Def{
		{
			Name:    "skills_list",
			Toolset: toolset,
			Title:   "List skills",
			About:   "List the skills the agent can load",
			Group:   "Skills",
			Icon:    tools.IconFind,
			Subject: "category",
			Schema: tools.Schema("List available skills (name + description). Use skill_view(name) to load full content.", []string{}, map[string]any{
				"category": tools.Property("string", "Optional category filter to narrow results"),
			}),
			Handler:  tools.Bind(r.list),
			Effect:   tools.ReadOnly,
			Parallel: true,
		},
		{
			Name:    "skill_view",
			Toolset: toolset,
			Title:   "Read skill",
			About:   "Load the instructions of a skill and its files",
			Group:   "Skills",
			Icon:    tools.IconRead,
			Subject: "name",
			Schema: tools.Schema("Load a skill's SKILL.md, or one of its linked references, templates or scripts. The first call returns SKILL.md plus a 'linked_files' dict; call again with file_path to read one.", []string{"name"}, map[string]any{
				"name":      tools.Property("string", "The skill name (use skills_list to see available skills). For plugin-provided skills, use the qualified form 'plugin:skill'."),
				"file_path": tools.Property("string", "OPTIONAL: Path to a linked file within the skill (e.g., 'references/api.md', 'templates/config.yaml', 'scripts/validate.py'). Omit to get the main SKILL.md content."),
			}),
			Handler:  r.view,
			Effect:   tools.ReadOnly,
			Parallel: true,
		},
		{
			Name:    "skill_manage",
			Toolset: toolset,
			Title:   "Change skill",
			About:   "Create, change or delete a skill",
			Group:   "Skills",
			Icon:    tools.IconEdit,
			Risks:   []string{skills.RiskWrite},
			Schema:  manageSchema(),
			Handler: r.manage,
			Effect:  tools.Mutates,
		},
	}
}

// Scope of a call: the session's profile, folder and surface, its user asked for missing variables and approval
func (r *registrant) scope(call *tools.Call) *skills.Scope {
	env := call.Env
	s := r.skills.Scope(env.Config, env.Cwd)
	s.Platform, s.Session, s.Approve = cmp.Or(env.Platform, s.Platform), call.SessionID, env.Approve
	if backend, ok := r.terminal.Backend(env.Config.GetString(terminalconfig.Backend)); ok && backend.Remote {
		s.Remote = backend.Name
	}
	if host := r.terminal.Host(); host != nil {
		s.Env = func() map[string]string { return host.ChildEnv(env.Config, nil) }
	}
	if env.Clarify != nil {
		s.Ask = func(ctx context.Context, skill string, v skills.Variable) (string, error) {
			question := fmt.Sprintf("The %s skill needs %s. %s", skill, v.Name, v.Prompt)
			if v.For != "" {
				question += " (needed for " + v.For + ")"
			}
			if v.Help != "" {
				question += "\n" + v.Help
			}
			reply, err := env.Clarify(ctx, []tools.Question{{ID: v.Name, Text: question}})
			if err != nil {
				return "", err
			}
			answer := reply.Answers[v.Name]
			if reply.Outcome != tools.Submitted || len(answer) == 0 {
				return "", errors.New(cmp.Or(reply.Notice, "The user skipped it."))
			}
			return strings.TrimSpace(answer[0]), nil
		}
	}
	return s
}

// One listed skill
type listed struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Category    *string `json:"category"`
}

func (r *registrant) list(ctx context.Context, call *tools.Call, args struct {
	Category string `json:"category"`
}) (string, error) {
	s := r.scope(call)
	if err := s.Seed(ctx); err != nil {
		return "", err
	}
	var all []listed
	for _, e := range s.Loadable(false) {
		var category *string
		if e.Category != "" {
			category = &e.Category
		}
		all = append(all, listed{e.Load, e.Description, category})
	}
	disabled := s.Disabled()
	plugin := "plugin"
	for _, p := range s.Plugins() {
		if !disabled[p.Qualified()] && skills.Supported(p.Frontmatter) {
			all = append(all, listed{p.Qualified(), p.Description, &plugin})
		}
	}
	if len(all) == 0 {
		return tools.JSON(skills.Result{"success": true, "skills": []listed{}, "categories": []string{}, "message": "No skills found in skills/ directory."}), nil
	}
	if args.Category != "" {
		all = slices.DeleteFunc(all, func(l listed) bool { return l.Category == nil || *l.Category != args.Category })
	}
	slices.SortStableFunc(all, func(a, b listed) int {
		return cmp.Or(cmp.Compare(strs.Deref(a.Category), strs.Deref(b.Category)), cmp.Compare(a.Name, b.Name))
	})
	categories := []string{}
	for _, l := range all {
		if c := strs.Deref(l.Category); c != "" && !slices.Contains(categories, c) {
			categories = append(categories, c)
		}
	}
	slices.Sort(categories)
	if all == nil {
		all = []listed{}
	}
	return tools.JSON(skills.Result{"success": true, "skills": all, "categories": categories, "count": len(all), "hint": "Use skill_view(name) to see full content, tags, and linked files"}), nil
}

func (r *registrant) view(ctx context.Context, call *tools.Call) string {
	name, ok := call.Args["name"].(string)
	if !ok {
		return tools.JSON(skills.Result{"success": false, "error": "Skill name must be a string.", "hint": "Use a skill name or relative path within the skills directory."})
	}
	file, _ := call.Args["file_path"].(string)
	s := r.scope(call)
	if err := s.Seed(ctx); err != nil {
		return tools.ErrorResult(err)
	}
	result := s.View(ctx, name, file, true)
	if !result.OK() {
		return tools.JSON(result)
	}
	text := tools.JSON(result)
	if stub := r.dedup(call.SessionID, name, file, result, text); stub != "" {
		return stub
	}
	resolved := cmp.Or(result.Text("name"), name)
	s.Viewed(ctx, resolved)
	s.Used(ctx, resolved)
	return text
}

// The unchanged stub when the session was served this very view before, else records it
func (r *registrant) dedup(session, name, file string, result skills.Result, text string) string {
	needed, _ := result["setup_needed"].(bool)
	if session == "" || needed || result.Text("readiness_status") == skills.SetupNeeded {
		return ""
	}
	resolved := cmp.Or(result.Text("name"), name)
	key := resolved + "\x00" + file
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	r.mu.Lock()
	defer r.mu.Unlock()
	views := r.views[session]
	if views == nil {
		views = &served{digests: map[string]string{}}
		r.views[session] = views
	}
	if prior, ok := views.digests[key]; ok {
		if prior == digest {
			return tools.JSON(skills.Result{"success": true, "status": "unchanged", "name": resolved, "file": cmp.Or(file, "SKILL.md"), "dedup": true, "content_returned": false, "message": unchanged})
		}
		views.order = slices.DeleteFunc(views.order, func(k string) bool { return k == key })
	}
	views.digests[key] = digest
	views.order = append(views.order, key)
	for len(views.order) > remembered {
		delete(views.digests, views.order[0])
		views.order = views.order[1:]
	}
	return ""
}

func (r *registrant) manage(ctx context.Context, call *tools.Call) string {
	ops, flat, err := parseOps(cmp.Or(call.RawArgs, "{}"))
	if err != nil {
		return tools.ErrorResult(err)
	}
	scope := r.scope(call)
	var result skills.Result
	if flat {
		result, err = scope.Manage(ctx, ops[0])
	} else {
		result, err = scope.Batch(ctx, ops)
	}
	if err != nil {
		return tools.ErrorResult(fmt.Errorf("Tool execution failed: %w", err))
	}
	return tools.JSON(result)
}

// Operations of a skill_manage call: its operations list, each op naming the call's skill when it names none; or the call itself as
// one operation, flat
func parseOps(raw string) ([]skills.Op, bool, error) {
	var call struct {
		Operations json.RawMessage `json:"operations"`
		Name       any             `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &call); err != nil {
		return nil, false, fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	if len(call.Operations) == 0 || string(call.Operations) == "null" {
		op, err := parseOp(json.RawMessage(raw), -1)
		return []skills.Op{op}, true, err
	}
	var items []json.RawMessage
	if json.Unmarshal(call.Operations, &items) != nil {
		return nil, false, nil
	}
	ops := make([]skills.Op, len(items))
	for i, item := range items {
		op, err := parseOp(item, i)
		if err != nil {
			return nil, false, err
		}
		if op.Name == "" {
			op.Name, _ = call.Name.(string)
		}
		ops[i] = op
	}
	return ops, false, nil
}

// One operation, one that is not an object having no action, a field of the wrong kind refused by name
func parseOp(raw json.RawMessage, i int) (skills.Op, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return skills.Op{}, nil
	}
	var all any
	json.Unmarshal(fields["replace_all"], &all)
	delete(fields, "replace_all")
	cleaned, err := json.Marshal(fields)
	if err != nil {
		return skills.Op{}, err
	}
	var op skills.Op
	if err := json.Unmarshal(cleaned, &op); err != nil {
		if wrong, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			field := wrong.Field
			if i >= 0 {
				field = fmt.Sprintf("operations[%d].%s", i, field)
			}
			return op, fmt.Errorf("Tool execution failed: TypeError: %s must be text, got %s", field, wrong.Value)
		}
		return op, err
	}
	switch v := all.(type) {
	case bool:
		op.ReplaceAll = v
	case string:
		on, err := strconv.ParseBool(strings.TrimSpace(v))
		op.ReplaceAll = on || err != nil && v != ""
	case float64:
		op.ReplaceAll = v != 0
	}
	return op, nil
}

// Schema of skill_manage: only the operations list, each action its own shape
func manageSchema() map[string]any {
	text := func(description string) map[string]any { return tools.Property("string", description) }
	op := func(action string, properties map[string]any, required ...string) map[string]any {
		props := map[string]any{"name": map[string]any{"type": "string"}, "action": map[string]any{"type": "string", "enum": []string{action}}}
		for key, value := range properties {
			props[key] = value
		}
		return map[string]any{"type": "object", "additionalProperties": false, "properties": props, "required": append([]string{"name", "action"}, required...)}
	}
	items := map[string]any{"anyOf": []any{
		op("create", map[string]any{"content": text("Full SKILL.md text (YAML frontmatter + markdown body)."), "category": text("Optional category subdir (e.g. 'devops').")}, "content"),
		op("patch", map[string]any{
			"old_string":  text("Text to find (same matching semantics as the patch tool)."),
			"new_string":  text("Replacement; empty string deletes the match."),
			"replace_all": tools.Property("boolean", "Replace all occurrences (default false)."),
			"file_path":   text("Optional supporting file (write_file's shape); default SKILL.md."),
		}, "old_string", "new_string"),
		op("patch", map[string]any{"content": text("Full SKILL.md rewrite (REPLACES the whole file; last resort).")}, "content"),
		op("write_file", map[string]any{
			"file_path":    text("Path RELATIVE to the skill's own directory, e.g. 'references/api.md' — no leading slash, never absolute; first segment references/, templates/, scripts/, or assets/."),
			"file_content": text("Full text of the supporting file."),
		}, "file_path", "file_content"),
		op("remove_file", map[string]any{"file_path": text("Supporting file (write_file's shape).")}, "file_path"),
		op("delete", map[string]any{"absorbed_into": text("Curator consolidation only: umbrella skill that absorbed this one (must exist).")}),
	}}
	description := "Create, update or delete skills: your procedural memory for recurring tasks. The call is an operations array (a single edit is a list of one); it applies atomically — any failure rolls every touched skill back. Ops: create (full SKILL.md; lands in the profile's skills directory or configured skills.create_dir; must precede that skill's other ops), patch (targeted old_string/new_string fix — preferred; content alone REPLACES the whole file, read it via skill_view() first), write_file/remove_file (supporting files), delete (sole op only). Existing skills are modified wherever they live. " +
		fmt.Sprintf("Description: max %d chars, 'Use when <trigger>. <behavior>.', never the category name (the index shows it). Write lessons, not logs: imperative rule + why, no PR numbers/dates/incident narration, one rule per lesson, references/ named by topic (extend before adding). skill_view() shows format conventions.", skills.IndexDescription)
	return tools.Schema(description, []string{"operations"}, map[string]any{
		"operations": map[string]any{
			"type":        "array",
			"description": "Ordered ops; each names its target skill (lowercase, hyphens/underscores, max 64 chars). Each action is its own shape; another action's text slot is invalid.",
			"items":       items,
		},
	})
}
