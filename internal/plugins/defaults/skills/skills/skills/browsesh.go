package skills

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync"

	"github.com/nickheyer/icebath/internal/web"
)

const browseShCatalog = "https://browse.sh/api/skills"

// Browserbase's browse.sh catalog of browser automation skills, one per site
type browseSh struct {
	r     *run
	once  sync.Once
	items []browseItem
}

type browseItem struct {
	Slug              web.Text        `json:"slug"`
	Name              web.Text        `json:"name"`
	Title             web.Text        `json:"title"`
	Description       *web.Text       `json:"description"`
	Hostname          web.Text        `json:"hostname"`
	Category          web.Text        `json:"category"`
	SourceURL         web.Text        `json:"sourceUrl"`
	RecommendedMethod web.Text        `json:"recommendedMethod"`
	Proxies           json.RawMessage `json:"proxies"`
	InstallCount      json.RawMessage `json:"installCount"`
	Tags              web.Texts       `json:"tags"`
}

// A JSON value as Go reads it, the fallback when there is none
func value(raw json.RawMessage, fallback any) any {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return fallback
	}
	return v
}

func (b *browseSh) ID() string { return "browse-sh" }

// The catalog through the index cache
func (b *browseSh) catalog(ctx context.Context) []browseItem {
	b.once.Do(func() {
		if b.r.cached("browse_sh_catalog", cacheTTL, &b.items) && len(b.items) > 0 {
			return
		}
		var data struct {
			Skills []browseItem `json:"skills"`
		}
		if b.r.getJSON(ctx, browseShCatalog, request{}, &data) {
			b.items = data.Skills
			if len(b.items) > 0 {
				b.r.keep("browse_sh_catalog", b.items)
			}
		}
	})
	return b.items
}

// An item as a search result, nil without a slug and a name
func (b *browseSh) meta(item browseItem) *Meta {
	if item.Slug == "" || item.Name == "" {
		return nil
	}
	description := string(item.Title)
	if description == "" {
		description = string(item.Name)
	}
	if item.Description != nil {
		description = string(*item.Description)
	}
	if runes := []rune(description); len(runes) > 1024 {
		description = string(runes[:1021]) + "..."
	}
	return &Meta{Name: string(item.Name), Description: description, Source: "browse-sh", Identifier: "browse-sh/" + string(item.Slug), Trust: "community",
		Tags: append([]string{}, item.Tags...), Extra: map[string]any{"slug": string(item.Slug), "hostname": string(item.Hostname),
			"category": string(item.Category), "source_url": string(item.SourceURL), "recommended_method": string(item.RecommendedMethod),
			"proxies": value(item.Proxies, false), "install_count": value(item.InstallCount, 0)}}
}

func (b *browseSh) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	q := strings.ToLower(query)
	var out []Meta
	for _, item := range b.catalog(ctx) {
		if len(out) >= limit {
			break
		}
		description := ""
		if item.Description != nil {
			description = string(*item.Description)
		}
		if matches(q, string(item.Name), string(item.Title), description, string(item.Hostname), string(item.Category), []string(item.Tags)) {
			if m := b.meta(item); m != nil {
				out = append(out, *m)
			}
		}
	}
	return out
}

// The catalog's item of an identifier's slug
func (b *browseSh) item(ctx context.Context, identifier string) *browseItem {
	slug := strings.TrimPrefix(identifier, "browse-sh/")
	if slug == "" {
		return nil
	}
	items := b.catalog(ctx)
	for i := range items {
		if string(items[i].Slug) == slug {
			return &items[i]
		}
	}
	return nil
}

func (b *browseSh) Inspect(ctx context.Context, identifier string) *Meta {
	if item := b.item(ctx, identifier); item != nil {
		return b.meta(*item)
	}
	return nil
}

// The skill's SKILL.md from the address its detail names, else from its source when that is raw GitHub content
func (b *browseSh) Fetch(ctx context.Context, identifier string) *Bundle {
	item := b.item(ctx, identifier)
	if item == nil {
		return nil
	}
	slug := string(item.Slug)
	address := ""
	var data struct {
		SkillMdURL web.Text `json:"skillMdUrl"`
	}
	if b.r.getJSON(ctx, browseShCatalog+"/"+slug, request{follow: true}, &data) && strings.HasPrefix(string(data.SkillMdURL), "http") {
		address = string(data.SkillMdURL)
	} else if source := string(item.SourceURL); source != "" {
		if u, err := url.Parse(source); err == nil && (u.Hostname() == "raw.githubusercontent.com" || strings.HasSuffix(u.Hostname(), ".raw.githubusercontent.com")) {
			address = source
		}
	}
	if address == "" {
		return nil
	}
	content, ok := b.r.getText(ctx, address, request{follow: true})
	if !ok {
		return nil
	}
	name := slug[strings.LastIndex(slug, "/")+1:]
	if m := b.meta(*item); m != nil {
		name = m.Name
	}
	return &Bundle{Name: name, Files: []File{{Path: "SKILL.md", Data: []byte(content)}}, Source: "browse-sh", Identifier: identifier, Trust: "community",
		Metadata: map[string]any{"slug": slug, "hostname": string(item.Hostname), "source_url": string(item.SourceURL), "skill_md_url": address}}
}

func (b *browseSh) Revision(context.Context, string) string { return "" }
