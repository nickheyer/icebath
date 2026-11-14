package tool

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/internal/model/wire"
)

const (
	ConnectorPrefix = "connectors__"
	ConnectorBatch  = "connectors__execute"
	mcpToolPrefix   = "mcp__"
	connectorEmoji  = "🔗"
	mcpEmoji        = "🔌"
	searchEmoji     = "🔎"
	describeEmoji   = "📖"
)

type Label struct {
	Kind    string
	App     string
	Action  string
	Emoji   string
	Name    string
	Preview string
}

func (l Label) Text() string {
	if l.Action != "" {
		return l.App + " · " + l.Action
	}
	return l.App
}

func (l Label) MarshalJSON() ([]byte, error) {
	return wire.Object([]wire.Member{
		{Key: "kind", Value: wire.Quote(l.Kind)},
		{Key: "app", Value: wire.Quote(l.App)},
		{Key: "action", Value: wire.Quote(l.Action)},
		{Key: "emoji", Value: wire.Quote(l.Emoji)},
		{Key: "text", Value: wire.Quote(l.Text())},
		{Key: "name", Value: wire.Quote(l.Name)},
		{Key: "preview", Value: wire.Quote(l.Preview)},
	}), nil
}

type LabelDisplay interface {
	Verb(name string) string
	Preview(name string, args Args) string
}

func ParseConnectorName(name string) (connector, tool string, ok bool) {
	parts := strings.SplitN(name, "__", 3)
	if len(parts) != 3 || parts[0] != "connectors" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func appTitles() map[string]string {
	return map[string]string{
		"discord":        "Discord",
		"figma":          "Figma",
		"github":         "GitHub",
		"gmail":          "Gmail",
		"googlecalendar": "Google Calendar",
		"googledocs":     "Google Docs",
		"googledrive":    "Google Drive",
		"jira":           "Jira",
		"linear":         "Linear",
		"notion":         "Notion",
		"outlook":        "Outlook",
		"slack":          "Slack",
		"stripe_mcp":     "Stripe",
		"todoist":        "Todoist",
	}
}

func AppTitle(slug string) string {
	key := strings.TrimSpace(slug)
	if known, ok := appTitles()[strings.ToLower(key)]; ok {
		return known
	}
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(key))
	for i, w := range words {
		r := []rune(w)
		words[i] = strings.ToUpper(string(r[:1])) + string(r[1:])
	}
	if title := strings.Join(words, " "); title != "" {
		return title
	}
	return key
}

func actionPhrase(tool, app string) string {
	text := strings.TrimSpace(tool)
	if app != "" {
		prefix := strings.ToUpper(strings.ReplaceAll(app, "-", "_")) + "_"
		if strings.HasPrefix(strings.ToUpper(text), prefix) {
			text = text[len(prefix):]
		}
	}
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(text))
	return strings.ToLower(strings.Join(words, " "))
}

func preview(d LabelDisplay, name string, args Args) string {
	if len(args) == 0 {
		return ""
	}
	return d.Preview(name, args)
}

func labelForName(r *Registry, at Scope, name string, args Args, d LabelDisplay) (Label, bool) {
	if strings.TrimSpace(name) == "" {
		return Label{}, false
	}
	if connector, tool, ok := ParseConnectorName(name); ok {
		action := actionPhrase(tool, connector)
		if alias, ok := args.String("connector_alias"); ok && strings.TrimSpace(alias) != "" {
			action = fmt.Sprintf("%s (%s)", action, strings.TrimSpace(alias))
		}
		return Label{Kind: "connector", App: AppTitle(connector), Action: action, Emoji: connectorEmoji, Name: name, Preview: preview(d, name, args)}, true
	}
	if rest, ok := strings.CutPrefix(name, mcpToolPrefix); ok {
		if server, tool, found := strings.Cut(rest, "__"); found && tool != "" {
			return Label{Kind: "mcp", App: AppTitle(server), Action: actionPhrase(tool, ""), Emoji: mcpEmoji, Name: name, Preview: preview(d, name, args)}, true
		}
	}
	app := d.Verb(name)
	if app == "" {
		app = AppTitle(name)
	}
	return Label{Kind: "tool", App: app, Emoji: r.Emoji(name, at), Name: name, Preview: preview(d, name, args)}, true
}

func argsOf(v any) Args {
	switch t := v.(type) {
	case Args:
		return t
	case map[string]any:
		return Args(t)
	}
	return nil
}

func falsy(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case bool:
		return !t
	case string:
		return t == ""
	case float64:
		return t == 0
	case int:
		return t == 0
	case json.Number:
		f, err := t.Float64()
		return err == nil && f == 0
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

func Labels(r *Registry, at Scope, name string, args Args, d LabelDisplay) []Label {
	switch name {
	case "tool_search":
		first := ""
		var queries []any
		switch q := args["queries"].(type) {
		case string:
			queries = []any{q}
		case []any:
			queries = q
		}
		for _, q := range queries {
			if text := fmt.Sprint(q); !falsy(q) && strings.TrimSpace(text) != "" {
				first = text
				break
			}
		}
		return []Label{{Kind: "tool", App: "Searching tools", Action: first, Emoji: searchEmoji, Name: name}}
	case "tool_describe":
		count := 0
		switch n := args["names"].(type) {
		case string:
			count = 1
		case []any:
			count = len(n)
		}
		action := fmt.Sprintf("%d tools", count)
		if count == 1 {
			action = "1 tool"
		}
		return []Label{{Kind: "tool", App: "Reading tool details", Action: action, Emoji: describeEmoji, Name: name}}
	case "tool_call":
	default:
		l, ok := labelForName(r, at, name, args, d)
		if !ok || l.Kind == "tool" {
			return nil
		}
		return []Label{l}
	}
	entries, ok := args["calls"].([]any)
	if !ok {
		entries = []any{map[string]any(args)}
	}
	out := make([]Label, 0, len(entries))
	for _, entry := range entries {
		e := argsOf(entry)
		callName, _ := e["name"].(string)
		callArgs := argsOf(e["arguments"])
		if l, ok := labelForName(r, at, callName, callArgs, d); ok {
			out = append(out, l)
			continue
		}
		out = append(out, Label{Kind: "tool", App: "Unnamed call", Emoji: DefaultEmoji, Preview: preview(d, "", callArgs)})
	}
	return out
}
