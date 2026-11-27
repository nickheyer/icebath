package skills

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"path"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/skills"
	"github.com/nickheyer/icebath/pkg/strs"
)

const (
	// Repository whose default branch carries the official optional skills newer than this build
	officialRepo = "nickheyer/icebath"
	// Folder of the official optional skills in it
	optionalPrefix = "pkg/skills/optional"
	remoteNote     = "Optional skill from icebath, downloaded from its repository when you install it"
)

// The official optional skills icebath ships, with those its repository has since added
type official struct {
	r *run

	once sync.Once
	// Folders of the optional skills on the repository's default branch, relative to their folder
	remote map[string]bool
}

func (o *official) ID() string { return "official" }

// Skills of the official offered source
func (o *official) offers() []skills.Offer {
	return slices.DeleteFunc(o.r.scope.Offers(), func(offer skills.Offer) bool { return offer.Source.Name != "official" })
}

func officialMeta(rel, name, description string, tags []string) Meta {
	return Meta{Name: name, Description: description, Source: "official", Identifier: "official/" + rel, Trust: "builtin", Repo: officialRepo,
		Path: optionalPrefix + "/" + rel, Tags: tags}
}

// What an offered skill's SKILL.md says of it, false when it can't be read
func offerMeta(offer skills.Offer) (Meta, bool) {
	data, err := fs.ReadFile(offer.Files, offer.Dir+"/SKILL.md")
	if err != nil || !utf8.Valid(data) {
		return Meta{}, false
	}
	fm, _ := skills.Parse(string(data))
	name := path.Base(offer.Dir)
	if v, ok := fm["name"]; ok && v != nil {
		name = fmt.Sprint(v)
	}
	description := ""
	if v, ok := fm["description"]; ok && v != nil {
		description = strs.Head(fmt.Sprint(v), 200)
	}
	return officialMeta(offer.Dir, name, description, tagsOf(fm.Meta()["tags"])), true
}

func (o *official) metas() []Meta {
	var out []Meta
	for _, offer := range o.offers() {
		if m, ok := offerMeta(offer); ok {
			out = append(out, m)
		}
	}
	return out
}

// Skills shipped here matching the query, then those of the repository's default branch not shipped here whose folder holds the query
func (o *official) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	q := strings.ToLower(query)
	var results []Meta
	local := map[string]bool{}
	for _, m := range o.metas() {
		local[strings.TrimPrefix(m.Identifier, "official/")] = true
		if matches(q, m.Name, m.Description, m.Tags) {
			results = append(results, m)
		}
		if len(results) >= limit {
			break
		}
	}
	if len(results) < limit {
		for _, rel := range slices.Sorted(maps.Keys(o.remoteDirs(ctx))) {
			if local[rel] || q != "" && !strings.Contains(strings.ToLower(rel), q) {
				continue
			}
			results = append(results, officialMeta(rel, path.Base(rel), remoteNote, []string{}))
			if len(results) >= limit {
				break
			}
		}
	}
	return results
}

// Folder of an official identifier, false when it climbs out of the tree
func officialRel(identifier string) (string, bool) {
	rel := path.Clean(strings.TrimPrefix(identifier, "official/"))
	if strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// The skill shipped here at its folder, or of its name; its upstream when it points at one; else the repository's copy
func (o *official) Fetch(ctx context.Context, identifier string) *Bundle {
	rel, ok := officialRel(identifier)
	if !ok {
		return nil
	}
	offers := o.offers()
	at := slices.IndexFunc(offers, func(offer skills.Offer) bool { return offer.Dir == rel })
	if at < 0 {
		at = slices.IndexFunc(offers, func(offer skills.Offer) bool { return path.Base(offer.Dir) == path.Base(rel) })
	}
	if at < 0 {
		return o.live(ctx, rel)
	}
	offer := offers[at]
	if data, err := fs.ReadFile(offer.Files, offer.Dir+"/SKILL.md"); err == nil {
		if repo, upstreamPath, ok := upstream(data); ok {
			return o.upstream(ctx, repo, upstreamPath, offer.Dir)
		}
	}
	bundle := &Bundle{Name: path.Base(offer.Dir), Source: "official", Identifier: "official/" + offer.Dir, Trust: "builtin"}
	fs.WalkDir(offer.Files, offer.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := fs.Stat(offer.Files, p); err != nil || !info.Mode().IsRegular() {
			return nil
		}
		rel := strings.TrimPrefix(p, offer.Dir+"/")
		if skipped(rel) {
			return nil
		}
		if data, err := fs.ReadFile(offer.Files, p); err == nil {
			bundle.Files = append(bundle.Files, File{Path: rel, Data: data})
		}
		return nil
	})
	if len(bundle.Files) == 0 {
		return nil
	}
	return bundle
}

// What a skill shipped here says of itself by its name, else the repository's one folder of the name
func (o *official) Inspect(ctx context.Context, identifier string) *Meta {
	name := path.Base(strings.TrimPrefix(identifier, "official/"))
	for _, m := range o.metas() {
		if m.Name == name {
			return &m
		}
	}
	if found := o.remoteMatches(ctx, name); len(found) == 1 {
		m := officialMeta(found[0], name, remoteNote, []string{})
		return &m
	}
	return nil
}

func (o *official) Revision(context.Context, string) string { return "" }

// Folders of the repository's optional skills whose name is the one given
func (o *official) remoteMatches(ctx context.Context, name string) []string {
	var out []string
	for rel := range o.remoteDirs(ctx) {
		if path.Base(rel) == name {
			out = append(out, rel)
		}
	}
	slices.Sort(out)
	return out
}

// Folders of the optional skills on the repository's default branch, read once per operation through the index cache; none offline
func (o *official) remoteDirs(ctx context.Context) map[string]bool {
	o.once.Do(func() {
		var dirs []string
		if !o.r.cached("official_optional_dirs", cacheTTL, &dirs) || len(dirs) == 0 {
			dirs = nil
			if t := o.r.gh.tree(ctx, officialRepo); t != nil {
				prefix, suffix := optionalPrefix+"/", "/SKILL.md"
				for _, entry := range t.entries {
					rel, ok := strings.CutPrefix(entry.Path, prefix)
					if entry.Type == "blob" && ok && strings.HasSuffix(rel, suffix) {
						if dir := strings.TrimSuffix(rel, suffix); dir != "" && !skills.Excluded(rel) {
							dirs = append(dirs, dir)
						}
					}
				}
			}
			if len(dirs) > 0 {
				o.r.keep("official_optional_dirs", dirs)
			}
		}
		o.remote = map[string]bool{}
		for _, dir := range dirs {
			o.remote[dir] = true
		}
	})
	return o.remote
}

// The repository's copy of an optional skill this build does not ship, found by its folder's name when no category matches
func (o *official) live(ctx context.Context, rel string) *Bundle {
	var parts []string
	for _, part := range strings.Split(strings.Trim(rel, "/"), "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 || slices.Contains(parts, "..") {
		return nil
	}
	rel = strings.Join(parts, "/")
	if !o.remoteDirs(ctx)[rel] {
		found := o.remoteMatches(ctx, parts[len(parts)-1])
		if len(found) != 1 {
			return nil
		}
		rel = found[0]
	}
	t := o.r.gh.tree(ctx, officialRepo)
	if t == nil {
		return nil
	}
	bundle := &Bundle{Name: path.Base(rel), Source: "official", Identifier: "official/" + rel, Trust: "builtin"}
	prefix := optionalPrefix + "/" + rel + "/"
	for _, entry := range t.entries {
		file, ok := strings.CutPrefix(entry.Path, prefix)
		if !ok || !entry.regular() || skipped(file) {
			continue
		}
		data, ok := o.r.gh.bytes(ctx, officialRepo, entry.Path, "")
		if !ok {
			slog.Warn("Live-repo optional skill fetch failed for " + entry.Path)
			return nil
		}
		bundle.set(file, data)
	}
	data, ok := bundle.file("SKILL.md")
	if !ok {
		return nil
	}
	if repo, upstreamPath, ok := upstream(data); ok {
		return o.upstream(ctx, repo, upstreamPath, rel)
	}
	slog.Info(fmt.Sprintf("Optional skill '%s' fetched from live repo (not in local checkout)", rel))
	return bundle
}

// Repository and path a catalog entry's SKILL.md points at through metadata.icebath.upstream, false for a skill kept here whole
func upstream(content []byte) (string, string, bool) {
	if !utf8.Valid(content) {
		return "", "", false
	}
	fm, _ := skills.Parse(string(content))
	pointer, _ := fm.Meta()["upstream"].(map[string]any)
	if pointer == nil {
		return "", "", false
	}
	field := func(key string) string {
		if v, ok := pointer[key]; ok && v != nil {
			return strings.Trim(strings.TrimSpace(fmt.Sprint(v)), "/")
		}
		return ""
	}
	repo, upstreamPath := field("repo"), field("path")
	if repo == "" || strings.Count(repo, "/") != 1 || upstreamPath == "" {
		return "", "", false
	}
	var parts []string
	for _, part := range strings.Split(upstreamPath, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 || slices.Contains(parts, "..") {
		return "", "", false
	}
	return repo, strings.Join(parts, "/"), true
}

// A skill its upstream maintains, fetched from GitHub and kept as an official one of trusted rather than built in trust
func (o *official) upstream(ctx context.Context, repo, upstreamPath, rel string) *Bundle {
	b := o.r.gh.fetch(ctx, repo+"/"+upstreamPath)
	if b == nil {
		slog.Warn(fmt.Sprintf("Upstream fetch failed for optional skill %s (%s:%s)", rel, repo, upstreamPath))
		return nil
	}
	metadata := maps.Clone(b.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["upstream_repo"], metadata["upstream_path"] = repo, upstreamPath
	return &Bundle{Name: b.Name, Files: b.Files, Source: "official", Identifier: "official/" + rel, Trust: "trusted", Metadata: metadata}
}
