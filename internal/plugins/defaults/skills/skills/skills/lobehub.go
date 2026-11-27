package skills

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/internal/web"
	"github.com/nickheyer/icebath/pkg/strs"
)

const lobeHubIndex = "https://chat-agents.lobehub.com/index.json"

// The LobeHub marketplace of agents, each turned into a SKILL.md when fetched
type lobehub struct {
	r      *run
	once   sync.Once
	agents []lobeAgent
}

type lobeMeta struct {
	Title       web.Text  `json:"title"`
	Description web.Text  `json:"description"`
	Tags        web.Texts `json:"tags"`
}

type lobeAgent struct {
	Identifier web.Text  `json:"identifier"`
	Meta       *lobeMeta `json:"meta"`
	// An agent without meta carries these itself
	lobeMeta
	Config struct {
		SystemRole web.Text `json:"systemRole"`
	} `json:"config"`
}

// What the agent says of itself, from its meta or itself
func (a lobeAgent) about() lobeMeta {
	if a.Meta != nil {
		return *a.Meta
	}
	return a.lobeMeta
}

func (a lobeAgent) title() string {
	if title := string(a.about().Title); title != "" {
		return title
	}
	return string(a.Identifier)
}

func (l *lobehub) ID() string { return "lobehub" }

// The marketplace's agents through the index cache: a list, or the agents of a mapping
func (l *lobehub) list(ctx context.Context) []lobeAgent {
	l.once.Do(func() {
		var raw json.RawMessage
		if !l.r.cached("lobehub_index", cacheTTL, &raw) {
			if !l.r.getJSON(ctx, lobeHubIndex, request{timeout: 30 * time.Second}, &raw) || !web.Holds(raw) {
				return
			}
			l.r.keep("lobehub_index", raw)
		}
		var wrapped struct {
			Agents []lobeAgent `json:"agents"`
		}
		if json.Unmarshal(raw, &l.agents) != nil && json.Unmarshal(raw, &wrapped) == nil {
			l.agents = wrapped.Agents
		}
	})
	return l.agents
}

func lobeMetaOf(a lobeAgent, name, description string) Meta {
	return Meta{Name: name, Description: description, Source: "lobehub", Identifier: "lobehub/" + name, Trust: "community",
		Tags: append([]string{}, a.about().Tags...), Extra: map[string]any{}}
}

// Agents whose title, description or tags hold the query
func (l *lobehub) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	q := strings.ToLower(query)
	var out []Meta
	for _, a := range l.list(ctx) {
		if len(out) >= limit {
			break
		}
		about := a.about()
		if !matches(q, a.title(), string(about.Description), []string(about.Tags)) {
			continue
		}
		identifier := string(a.Identifier)
		if identifier == "" {
			identifier = strings.ReplaceAll(strings.ToLower(a.title()), " ", "-")
		}
		out = append(out, lobeMetaOf(a, identifier, strs.Head(string(about.Description), 200)))
	}
	return out
}

func lobeID(identifier string) string {
	if rest, ok := strings.CutPrefix(identifier, "lobehub/"); ok {
		return rest
	}
	return identifier
}

// The agent turned into a SKILL.md
func (l *lobehub) Fetch(ctx context.Context, identifier string) *Bundle {
	id := lobeID(identifier)
	var agent lobeAgent
	var raw json.RawMessage
	if !l.r.getJSON(ctx, "https://chat-agents.lobehub.com/"+id+".json", request{timeout: 15 * time.Second}, &raw) || !web.Holds(raw) ||
		json.Unmarshal(raw, &agent) != nil {
		return nil
	}
	return &Bundle{Name: id, Files: []File{{Path: "SKILL.md", Data: []byte(lobeSkill(agent))}}, Source: "lobehub", Identifier: "lobehub/" + id,
		Trust: "community", Metadata: map[string]any{}}
}

func (l *lobehub) Inspect(ctx context.Context, identifier string) *Meta {
	id := lobeID(identifier)
	for _, a := range l.list(ctx) {
		if string(a.Identifier) == id {
			m := lobeMetaOf(a, id, string(a.about().Description))
			return &m
		}
	}
	return nil
}

func (l *lobehub) Revision(context.Context, string) string { return "" }

// SKILL.md of a LobeHub agent: its name, description and tags as frontmatter, its system role as the instructions
func lobeSkill(a lobeAgent) string {
	about := a.about()
	identifier := string(a.Identifier)
	if identifier == "" {
		identifier = "lobehub-agent"
	}
	title := string(about.Title)
	if title == "" {
		title = identifier
	}
	description := string(about.Description)
	role := string(a.Config.SystemRole)
	if role == "" {
		role = "(No system role defined)"
	}
	fm := []string{"---", "name: " + identifier, "description: " + strs.Head(description, 500), "metadata:", "  icebath:",
		"    tags: [" + strings.Join(about.Tags, ", ") + "]", "  lobehub:", "    source: lobehub", "---"}
	body := []string{"# " + title, "", description, "", "## Instructions", "", role}
	return strings.Join(fm, "\n") + "\n\n" + strings.Join(body, "\n") + "\n"
}
