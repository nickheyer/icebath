package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/internal/web"
)

// Folder of a site's skills under its well-known path
const wellKnownPath = "/.well-known/skills"

// Skills a site publishes at /.well-known/skills/index.json
type wellKnown struct {
	r *run
}

func (w *wellKnown) ID() string { return "well-known" }

// A site's index of skills
type siteIndex struct {
	IndexURL string      `json:"index_url"`
	BaseURL  string      `json:"base_url"`
	Skills   []siteSkill `json:"skills"`
}

type siteSkill struct {
	Name        json.RawMessage `json:"name"`
	Description web.Text        `json:"description"`
	Files       json.RawMessage `json:"files"`
}

// Name of an entry, empty when it has none that is text
func (s siteSkill) name() string {
	var name string
	json.Unmarshal(s.Name, &name)
	return name
}

// Files an entry lists, SKILL.md alone when it lists none
func (s siteSkill) files() []any {
	var files []any
	if json.Unmarshal(s.Files, &files) != nil || files == nil {
		return []any{"SKILL.md"}
	}
	return files
}

func wellKnownWrap(base, name string) string {
	return "well-known:" + strings.TrimRight(base, "/") + "/" + name
}

func (w *wellKnown) meta(indexURL, base, name, description string, files []any, endpoint string) Meta {
	extra := map[string]any{"index_url": indexURL, "base_url": base, "files": files}
	if endpoint != "" {
		extra["endpoint"] = endpoint
	}
	return Meta{Name: name, Description: description, Source: "well-known", Identifier: wellKnownWrap(base, name), Trust: "community", Path: name,
		Tags: []string{}, Extra: extra}
}

// Address of the index an http query names, false for any other query
func wellKnownIndex(query string) (string, bool) {
	query = strings.TrimSpace(query)
	if !strings.HasPrefix(query, "http://") && !strings.HasPrefix(query, "https://") {
		return "", false
	}
	if strings.HasSuffix(query, "/index.json") {
		return query, true
	}
	if before, _, ok := strings.Cut(query, wellKnownPath+"/"); ok {
		return before + wellKnownPath + "/index.json", true
	}
	return strings.TrimRight(query, "/") + wellKnownPath + "/index.json", true
}

// Entries of the index a query names, at most limit
func (w *wellKnown) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	indexURL, ok := wellKnownIndex(query)
	if !ok {
		return nil
	}
	idx := w.index(ctx, indexURL)
	if idx == nil {
		return nil
	}
	var out []Meta
	for _, entry := range head(idx.Skills, limit) {
		if name := entry.name(); name != "" {
			out = append(out, w.meta(idx.IndexURL, idx.BaseURL, name, string(entry.Description), entry.files(), ""))
		}
	}
	return out
}

// Where an identifier's skill is published: its index, its site's base, its name and its folder; false when it is no well-known address
type wellKnownID struct {
	index, base, name, skill string
}

func parseWellKnown(identifier string) (wellKnownID, bool) {
	raw := strings.TrimPrefix(identifier, "well-known:")
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return wellKnownID{}, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return wellKnownID{}, false
	}
	fragment := u.Fragment
	u.Fragment, u.RawFragment = "", ""
	clean := u.String()
	var id wellKnownID
	if strings.HasSuffix(clean, "/index.json") {
		if fragment == "" {
			return wellKnownID{}, false
		}
		id.base, id.name = strings.TrimSuffix(clean, "/index.json"), fragment
		id.skill = id.base + "/" + id.name
	} else {
		id.skill = strings.TrimRight(strings.TrimSuffix(clean, "/SKILL.md"), "/")
		if !strings.Contains(id.skill, wellKnownPath+"/") {
			return wellKnownID{}, false
		}
		at := strings.LastIndex(id.skill, "/")
		id.base, id.name = id.skill[:at], id.skill[at+1:]
	}
	id.index = id.base + "/index.json"
	return id, true
}

// The index document of a site through the index cache, nil when it can't be had
func (w *wellKnown) index(ctx context.Context, indexURL string) *siteIndex {
	key := "well_known_index_" + md5hex(indexURL)
	var cached siteIndex
	if w.r.cached(key, cacheTTL, &cached) && cached.Skills != nil {
		return &cached
	}
	resp := w.r.guarded(ctx, indexURL, request{})
	if !resp.ok() {
		return nil
	}
	var doc struct {
		Skills *[]siteSkill `json:"skills"`
	}
	if json.Unmarshal(resp.body, &doc) != nil {
		return nil
	}
	idx := &siteIndex{IndexURL: indexURL, BaseURL: strings.TrimSuffix(indexURL, "/index.json"), Skills: []siteSkill{}}
	if doc.Skills != nil {
		idx.Skills = *doc.Skills
	}
	w.r.keep(key, idx)
	return idx
}

// The index's entry of the name
func (w *wellKnown) entry(ctx context.Context, indexURL, name string) *siteSkill {
	idx := w.index(ctx, indexURL)
	if idx == nil {
		return nil
	}
	for i := range idx.Skills {
		if idx.Skills[i].name() == name {
			return &idx.Skills[i]
		}
	}
	return nil
}

// What the skill's SKILL.md says of it
func (w *wellKnown) Inspect(ctx context.Context, identifier string) *Meta {
	id, ok := parseWellKnown(identifier)
	if !ok {
		return nil
	}
	entry := w.entry(ctx, id.index, id.name)
	if entry == nil {
		return nil
	}
	data, ok := w.r.fetch(ctx, id.skill+"/SKILL.md")
	if !ok {
		return nil
	}
	fm, _ := skills.Parse(string(data))
	name := id.name
	if v := fm["name"]; config.Truthy(v) {
		name = fmt.Sprint(v)
	}
	description := string(entry.Description)
	if v := fm["description"]; config.Truthy(v) {
		description = fmt.Sprint(v)
	}
	m := w.meta(id.index, id.base, name, description, entry.files(), id.skill)
	return &m
}

// Every file the index lists for the skill, nil when one is unsafe or missing, or there is no SKILL.md
func (w *wellKnown) Fetch(ctx context.Context, identifier string) *Bundle {
	id, ok := parseWellKnown(identifier)
	if !ok {
		return nil
	}
	name, err := skillName(id.name)
	if err != nil {
		slog.Warn("Well-known skill identifier contained unsafe skill name: " + identifier)
		return nil
	}
	entry := w.entry(ctx, id.index, id.name)
	if entry == nil {
		return nil
	}
	files := entry.files()
	if len(files) == 0 {
		files = []any{"SKILL.md"}
	}
	bundle := &Bundle{Name: name, Source: "well-known", Identifier: wellKnownWrap(id.base, name), Trust: "community",
		Metadata: map[string]any{"index_url": id.index, "base_url": id.base, "endpoint": id.skill, "files": files}}
	for _, item := range files {
		rel, ok := item.(string)
		if !ok || rel == "" {
			continue
		}
		safe, err := bundlePath(rel)
		if err != nil {
			slog.Warn(fmt.Sprintf("Well-known skill %s advertised unsafe file path: %q", identifier, rel))
			return nil
		}
		data, ok := w.r.fetch(ctx, id.skill+"/"+safe)
		if !ok {
			return nil
		}
		bundle.set(safe, data)
	}
	if _, ok := bundle.file("SKILL.md"); !ok {
		return nil
	}
	return bundle
}

func (w *wellKnown) Revision(context.Context, string) string { return "" }
