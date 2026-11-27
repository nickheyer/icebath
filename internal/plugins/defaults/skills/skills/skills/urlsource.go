package skills

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/skills"
)

// A SKILL.md at a URL, with the supporting files it names on the same host
type urlSource struct {
	r *run
}

func (u *urlSource) ID() string { return "url" }

var installableName = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Whether a name can be a skill's: a lower case identifier other than the words a page of no name falls back on
func installable(name string) bool {
	candidate := strings.ToLower(strings.TrimSpace(name))
	switch candidate {
	case "", "skill", "readme", "index", "unnamed-skill":
		return false
	}
	return installableName.MatchString(candidate)
}

// Whether the identifier is an http URL of a markdown file this source takes, rather than a well-known one
func (u *urlSource) claims(identifier string) bool {
	ident := strings.TrimSpace(identifier)
	lower := strings.ToLower(ident)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") || strings.Contains(ident, wellKnownPath+"/") ||
		strings.HasSuffix(strings.TrimRight(ident, "/"), "/index.json") {
		return false
	}
	parsed, err := url.Parse(ident)
	return err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".md")
}

// Name of a skill at a URL: its frontmatter's when it is a usable one, else its folder's for a SKILL.md, else its file's; empty when none
// is usable
func urlName(fm skills.Frontmatter, raw string) string {
	if name, ok := fm["name"].(string); ok && installable(name) {
		return strings.TrimSpace(name)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	var parts []string
	for _, part := range strings.Split(parsed.Path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) >= 2 && strings.ToLower(parts[len(parts)-1]) == "skill.md" && installable(parts[len(parts)-2]) {
		return parts[len(parts)-2]
	}
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	if strings.HasSuffix(strings.ToLower(last), ".md") {
		last = last[:len(last)-3]
	}
	if installable(last) {
		return last
	}
	return ""
}

func (u *urlSource) Search(context.Context, string, int, string) []Meta { return nil }

// The page at the URL with its frontmatter and the name it resolves to, false when the URL is not taken or can't be read
func (u *urlSource) load(ctx context.Context, identifier string) (string, string, skills.Frontmatter, string, bool) {
	if !u.claims(identifier) {
		return "", "", nil, "", false
	}
	target := strings.TrimSpace(identifier)
	data, ok := u.r.fetch(ctx, target)
	if !ok {
		return "", "", nil, "", false
	}
	text := string(data)
	fm, _ := skills.Parse(text)
	return target, text, fm, urlName(fm, target), true
}

func (u *urlSource) Inspect(ctx context.Context, identifier string) *Meta {
	target, _, fm, name, ok := u.load(ctx, identifier)
	if !ok {
		return nil
	}
	description := ""
	if v := fm["description"]; config.Truthy(v) {
		description = fmt.Sprint(v)
	}
	return &Meta{Name: name, Description: description, Source: "url", Identifier: target, Trust: "community", Path: name,
		Tags: tagsOf(fm.Meta()["tags"]), Extra: map[string]any{"url": target, "awaiting_name": name == ""}}
}

// The SKILL.md and the supporting files it names on its own host; one that can't be fetched is left out with a warning
func (u *urlSource) Fetch(ctx context.Context, identifier string) *Bundle {
	target, text, _, name, ok := u.load(ctx, identifier)
	if !ok {
		return nil
	}
	named, ok := referenced(text)
	if !ok {
		return nil
	}
	origin, err := url.Parse(target)
	if err != nil {
		return nil
	}
	bundle := &Bundle{Files: []File{{Path: "SKILL.md", Data: []byte(text)}}, Source: "url", Identifier: target, Trust: "community"}
	for _, rel := range slices.Sorted(maps.Keys(named)) {
		ref, err := origin.Parse(escapePath(rel))
		if err != nil || ref.Host != origin.Host {
			return nil
		}
		data, ok := u.r.fetch(ctx, ref.String())
		if !ok {
			slog.Warn(fmt.Sprintf("URL skill %s: referenced support file %q could not be fetched from %s; skipping it", target, rel, ref))
			continue
		}
		bundle.Files = append(bundle.Files, File{Path: rel, Data: data})
	}
	if name != "" {
		safe, err := skillName(name)
		if err != nil {
			slog.Warn(fmt.Sprintf("URL skill %s produced unsafe skill name: %q", target, name))
			return nil
		}
		bundle.Name = safe
	}
	bundle.Metadata = map[string]any{"url": target, "source_url": target, "awaiting_name": bundle.Name == ""}
	return bundle
}

func (u *urlSource) Revision(context.Context, string) string { return "" }
