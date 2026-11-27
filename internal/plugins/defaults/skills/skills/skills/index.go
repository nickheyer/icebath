package skills

import (
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nickheyer/icebath/internal/web"
)

const (
	// The centralized index of every registry's skills, rebuilt upstream
	indexURL = "https://icebath.app/api/skills-index.json"
	indexTTL = 6 * time.Hour
)

// The index as it was when this build was made, for when the live one can't be had
//
//go:embed skills-index.json.zst
var shippedArchive []byte

type index struct {
	Skills []indexEntry `json:"skills"`
}

type indexEntry struct {
	Name        web.Text       `json:"name"`
	Description web.Text       `json:"description"`
	Source      web.Text       `json:"source"`
	Identifier  web.Text       `json:"identifier"`
	Trust       web.Text       `json:"trust_level"`
	Repo        web.Text       `json:"repo"`
	Path        web.Text       `json:"path"`
	Tags        web.Texts      `json:"tags"`
	Extra       map[string]any `json:"extra"`
	// GitHub identifier the skill was found at
	Resolved web.Text `json:"resolved_github_id"`
}

// Provider of an entry, lower case
func (e indexEntry) provider() string {
	if v, ok := e.Extra["provider"]; ok && v != nil {
		return strings.ToLower(fmt.Sprint(v))
	}
	return ""
}

func (e indexEntry) meta() Meta {
	return Meta{Name: string(e.Name), Description: string(e.Description), Source: cmp.Or(string(e.Source), "skills-index"),
		Identifier: string(e.Identifier), Trust: cmp.Or(string(e.Trust), "community"), Repo: string(e.Repo), Path: string(e.Path),
		Tags: append([]string{}, e.Tags...), Extra: e.Extra}
}

// An index document, nil when it is not one
func decodeIndex(data []byte) *index {
	var doc struct {
		Skills *[]indexEntry `json:"skills"`
	}
	if json.Unmarshal(data, &doc) != nil || doc.Skills == nil {
		return nil
	}
	return &index{Skills: *doc.Skills}
}

// The index icebath ships, decoded once
func (h *Hub) shippedIndex() *index {
	h.loading.Lock()
	defer h.loading.Unlock()
	if h.shipped == nil {
		h.shipped = &index{}
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
		if err == nil {
			defer decoder.Close()
			var raw []byte
			if raw, err = decoder.DecodeAll(shippedArchive, nil); err == nil {
				if idx := decodeIndex(raw); idx != nil {
					h.shipped = idx
				}
			}
		}
		if err != nil {
			slog.Warn("The shipped skills index can't be read", "error", err)
		}
	}
	return h.shipped
}

// The index: the copy cached in the last six hours, else the live one, else the cached copy however old, else the one icebath ships
func (r *run) loadIndex(ctx context.Context) *index {
	file := filepath.Join(r.cacheDir(), "skills-index.json")
	if info, err := os.Stat(file); err == nil && time.Since(info.ModTime()) <= indexTTL {
		if data, err := os.ReadFile(file); err == nil {
			if idx := decodeIndex(data); idx != nil {
				return idx
			}
		}
	}
	resp, err := r.get(ctx, indexURL, request{timeout: 15 * time.Second, follow: true})
	switch {
	case err != nil:
		slog.Debug("Skills index fetch failed", "error", err)
	case !resp.ok():
		slog.Debug(fmt.Sprintf("Skills index fetch returned %d", resp.status))
	default:
		if idx := decodeIndex(resp.body); idx != nil {
			if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
				os.WriteFile(file, resp.body, 0o644)
			}
			return idx
		}
	}
	if data, err := os.ReadFile(file); err == nil {
		if idx := decodeIndex(data); idx != nil {
			return idx
		}
	}
	return r.hub.shippedIndex()
}

// The skills of the centralized index, which stands in for the registries it covers
type indexSource struct {
	r       *run
	once    sync.Once
	entries []indexEntry
}

func (x *indexSource) ID() string { return "skills-index" }

func (x *indexSource) skills(ctx context.Context) []indexEntry {
	x.once.Do(func() { x.entries = x.r.loadIndex(ctx).Skills })
	return x.entries
}

// Whether the index holds any skill
func (x *indexSource) available(ctx context.Context) bool {
	return len(x.skills(ctx)) > 0
}

// Matches in order: the name itself, a name starting with the query, the provider itself, a word of the name or provider, the query in
// the name, anywhere else
func (x *indexSource) Search(ctx context.Context, query string, limit int, provider string) []Meta {
	entries := x.skills(ctx)
	if want := strings.ToLower(strings.TrimSpace(provider)); want != "" {
		entries = slices.DeleteFunc(slices.Clone(entries), func(e indexEntry) bool { return e.provider() != want })
	}
	var out []Meta
	if strings.TrimSpace(query) == "" {
		for _, e := range head(entries, limit) {
			out = append(out, e.meta())
		}
		return out
	}
	q := strings.ToLower(query)
	type hit struct {
		rank, at int
	}
	var hits []hit
	for i, e := range entries {
		name, provider := strings.ToLower(string(e.Name)), e.provider()
		hay := strings.Join([]string{name, strings.ToLower(string(e.Description)), strings.ToLower(strings.Join(e.Tags, " ")),
			strings.ToLower(string(e.Identifier)), provider}, " ")
		if !strings.Contains(hay, q) {
			continue
		}
		ranks := []bool{name == q, strings.HasPrefix(name, q), provider == q,
			slices.Contains(strings.Fields(name), q) || slices.Contains(strings.Fields(provider), q), strings.Contains(name, q), true}
		hits = append(hits, hit{slices.Index(ranks, true), i})
	}
	slices.SortStableFunc(hits, func(a, b hit) int { return cmp.Or(a.rank-b.rank, a.at-b.at) })
	for _, h := range head(hits, limit) {
		out = append(out, entries[h.at].meta())
	}
	return out
}

// Prefixes of identifiers the index and the registries write differently
var indexPrefixes = []string{"skills-sh/", "skills.sh/", "official/", "github/", "clawhub/"}

func stripPrefix(value string, prefixes []string) string {
	for _, p := range prefixes {
		if rest, ok := strings.CutPrefix(value, p); ok {
			return rest
		}
	}
	return value
}

// Entry of the identifier: the same one, else the same once source prefixes are dropped from both
func (x *indexSource) find(ctx context.Context, identifier string) *indexEntry {
	entries := x.skills(ctx)
	for i := range entries {
		if string(entries[i].Identifier) == identifier {
			return &entries[i]
		}
	}
	normalized := stripPrefix(identifier, indexPrefixes)
	for i := range entries {
		if stripPrefix(string(entries[i].Identifier), indexPrefixes) == normalized {
			return &entries[i]
		}
	}
	return nil
}

// The skill from GitHub at the identifier the index resolved, or at its repository and path; a skills.sh entry found by its folder's
// name in its repository when neither answers
func (x *indexSource) Fetch(ctx context.Context, identifier string) *Bundle {
	entry := x.find(ctx, identifier)
	if entry == nil {
		return nil
	}
	repo, path := string(entry.Repo), string(entry.Path)
	trimmed := strings.TrimRight(path, "/")
	slug := trimmed[strings.LastIndex(trimmed, "/")+1:]
	fromSkillsSh := entry.Source == "skills.sh" || entry.Source == "skills-sh"
	candidates := []string{string(entry.Resolved)}
	if repo != "" && path != "" {
		candidates = append(candidates, repo+"/"+path)
	}
	var bundle *Bundle
	for _, candidate := range candidates {
		if candidate != "" {
			if bundle = x.r.gh.fetch(ctx, candidate); bundle != nil {
				break
			}
		}
	}
	if bundle == nil {
		if !fromSkillsSh || repo == "" || slug == "" {
			return nil
		}
		found := x.r.gh.findSkill(ctx, repo, slug)
		if found == "" {
			found = x.r.gh.rootSkill(ctx, repo)
		}
		if found == "" {
			return nil
		}
		if bundle = x.r.gh.fetch(ctx, found); bundle == nil {
			return nil
		}
	}
	if fromSkillsSh && bundle.Name == "skills" {
		bundle.Name = slug
	}
	bundle.Source, bundle.Identifier = cmp.Or(string(entry.Source), "skills-index"), identifier
	return bundle
}

func (x *indexSource) Inspect(ctx context.Context, identifier string) *Meta {
	if entry := x.find(ctx, identifier); entry != nil {
		m := entry.meta()
		return &m
	}
	return nil
}

func (x *indexSource) Revision(context.Context, string) string { return "" }
