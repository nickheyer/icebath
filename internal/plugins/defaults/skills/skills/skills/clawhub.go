package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/pkg/httpx"
)

const (
	clawhubBase = "https://clawhub.ai/api/v1"
	// Longest a walk of the catalog for one page of it may take
	clawhubWalkBudget = 12 * time.Second
	// Largest skill archive downloaded, and largest file read from one
	clawhubZipMax  = 25 << 20
	clawhubFileMax = 500_000
)

var (
	clawhubSlug    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	clawhubTerms   = regexp.MustCompile(`[^a-z0-9]+`)
	clawhubQueryID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
)

// Slug and owner of a ClawHub identifier: slug, clawhub/slug, @owner/slug or owner/skills/slug; false for any other
func parseClawhub(identifier string) (string, string, bool) {
	raw := strings.TrimSpace(identifier)
	if raw == "" {
		return "", "", false
	}
	at := strings.HasPrefix(raw, "@")
	stripped := strings.TrimPrefix(strings.TrimPrefix(raw, "@"), "clawhub/")
	if rest, ok := strings.CutPrefix(stripped, "@"); ok {
		at, stripped = true, rest
	}
	var parts []string
	for _, part := range strings.Split(stripped, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	slug, owner := "", ""
	switch {
	case len(parts) == 1:
		slug = parts[0]
	case len(parts) == 2 && at || len(parts) == 3 && strings.ToLower(parts[1]) == "skills":
		owner, slug = parts[0], parts[len(parts)-1]
	default:
		return "", "", false
	}
	if !clawhubSlug.MatchString(slug) || owner != "" && !clawhubSlug.MatchString(owner) {
		return "", "", false
	}
	return slug, owner, true
}

// The clawhub/slug a ClawHub skill's catalog card names it by, false for anything that is no ClawHub identifier
func clawhubCard(identifier string) (string, bool) {
	slug, _, ok := parseClawhub(identifier)
	if !ok {
		return "", false
	}
	return "clawhub/" + slug, true
}

// Lower case words of a text
func terms(text string) []string {
	return slices.DeleteFunc(clawhubTerms.Split(strings.ToLower(text), -1), func(t string) bool { return t == "" })
}

// Results without repeated identifiers, or names for those without one, ignoring case, the first kept
func dedupeClawhub(results []Meta) []Meta {
	seen := map[string]bool{}
	var out []Meta
	for _, m := range results {
		key := m.Identifier
		if key == "" {
			key = m.Name
		}
		if key = strings.ToLower(key); !seen[key] {
			seen[key] = true
			out = append(out, m)
		}
	}
	return out
}

// How well a result answers a query: the identifier or name itself, starting with it, its words, holding it
func clawhubScore(query string, m Meta) int {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return 1
	}
	identifier, name, description := strings.ToLower(m.Identifier), strings.ToLower(m.Name), strings.ToLower(m.Description)
	qTerms, idTerms, nameTerms := terms(q), terms(identifier), terms(name)
	normID, normName := strings.Join(idTerms, " "), strings.Join(nameTerms, " ")
	prefixed := func(words []string) bool {
		return len(qTerms) > 0 && slices.Equal(words[:min(len(qTerms), len(words))], qTerms)
	}
	score := 0
	for _, c := range []struct {
		points int
		hit    bool
	}{{140, q == identifier}, {130, q == name}, {125, normID == q}, {120, normName == q}, {95, strings.HasPrefix(normID, q)},
		{90, strings.HasPrefix(normName, q)}, {70, prefixed(idTerms)}, {65, prefixed(nameTerms)}, {40, strings.Contains(identifier, q)},
		{35, strings.Contains(name, q)}, {10, strings.Contains(description, q)}} {
		if c.hit {
			score += c.points
		}
	}
	for _, term := range qTerms {
		if slices.Contains(idTerms, term) {
			score += 15
		}
		if slices.Contains(nameTerms, term) {
			score += 12
		}
		if strings.Contains(description, term) {
			score += 3
		}
	}
	return score
}

// The ClawHub registry, every skill of community trust
type clawhub struct {
	r *run
}

func (c *clawhub) ID() string { return "clawhub" }

// A ClawHub payload with a nested skill flattened into it, its latest version and owner kept beside
type clawhubPayload map[string]json.RawMessage

func coerceClawhub(raw json.RawMessage) clawhubPayload {
	var outer clawhubPayload
	if json.Unmarshal(raw, &outer) != nil || outer == nil {
		return nil
	}
	var nested clawhubPayload
	if json.Unmarshal(outer["skill"], &nested) != nil || nested == nil {
		return outer
	}
	for _, key := range []string{"latestVersion", "owner"} {
		if v, ok := outer[key]; ok && string(v) != "null" {
			if _, has := nested[key]; !has {
				nested[key] = v
			}
		}
	}
	return nested
}

// Text of a field, empty when it is no text
func (p clawhubPayload) text(key string) string {
	var s string
	json.Unmarshal(p[key], &s)
	return s
}

// Owner handle of a payload, empty when it has none
func (p clawhubPayload) owner() string {
	var owner struct {
		Handle string `json:"handle"`
	}
	if json.Unmarshal(p["owner"], &owner) == nil && owner.Handle != "" {
		return strings.TrimSpace(owner.Handle)
	}
	return strings.TrimSpace(p.text("owner"))
}

// Whether the payload is the expected owner's, or names none
func (p clawhubPayload) ownedBy(expected string) bool {
	actual := p.owner()
	return expected == "" || actual == "" || strings.EqualFold(actual, expected)
}

// Tags of a listing: a list, or the keys of a version map but latest
func (p clawhubPayload) tags() []string {
	var list []any
	if json.Unmarshal(p["tags"], &list) == nil && list != nil {
		return tagsOf(list)
	}
	var versions map[string]json.RawMessage
	out := []string{}
	if json.Unmarshal(p["tags"], &versions) == nil {
		for key := range versions {
			if key != "latest" {
				out = append(out, key)
			}
		}
	}
	slices.Sort(out)
	return out
}

// A listing item as a search result, nil without a slug
func (p clawhubPayload) meta() *Meta {
	slug := p.text("slug")
	if slug == "" {
		return nil
	}
	first := func(keys ...string) string {
		for _, key := range keys {
			if v := p.text(key); v != "" {
				return v
			}
		}
		return ""
	}
	name := first("displayName", "name")
	if name == "" {
		name = slug
	}
	extra := map[string]any{}
	if owner := p.owner(); owner != "" {
		extra["owner"] = owner
	}
	return &Meta{Name: name, Description: first("summary", "description"), Source: "clawhub", Identifier: slug, Trust: "community",
		Tags: p.tags(), Extra: extra}
}

func ownerQuery(owner string) url.Values {
	if owner == "" {
		return nil
	}
	return url.Values{"owner": {owner}}
}

// Slug and detail of an identifier's skill, false when it can't be parsed or read, or is someone else's
func (c *clawhub) detail(ctx context.Context, identifier string) (string, clawhubPayload, bool) {
	slug, owner, ok := parseClawhub(identifier)
	if !ok {
		return "", nil, false
	}
	var raw json.RawMessage
	if !c.r.getJSON(ctx, clawhubBase+"/skills/"+slug, request{query: ownerQuery(owner)}, &raw) {
		return "", nil, false
	}
	data := coerceClawhub(raw)
	if data == nil || !data.ownedBy(owner) {
		return "", nil, false
	}
	return slug, data, true
}

// The skill the query names as a slug, directly or as its words joined with the endings slugs usually have
func (c *clawhub) exact(ctx context.Context, query string) *Meta {
	query = strings.TrimSpace(query)
	words := terms(query)
	var candidates []string
	if slug, _, ok := parseClawhub(query); ok {
		candidates = []string{slug}
	} else if !strings.Contains(query, "/") && clawhubSlug.MatchString(query) {
		candidates = []string{query}
	}
	if len(words) > 0 {
		base := strings.Join(words, "-")
		if len(words) >= 2 {
			for _, ending := range []string{"agent", "skill", "tool", "assistant", "playbook"} {
				candidates = append(candidates, base+"-"+ending)
			}
		}
		candidates = append(candidates, base)
	}
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if m := c.Inspect(ctx, candidate); m != nil {
			return m
		}
	}
	return nil
}

// Results ranked by how well they answer the query, the skill it names as a slug first; the listing as it came when none scores
func (c *clawhub) rank(ctx context.Context, query string, results []Meta, limit int) []Meta {
	q := strings.TrimSpace(query)
	if q == "" {
		return head(dedupeClawhub(results), limit)
	}
	type scored struct {
		m     Meta
		score int
	}
	var ranked []scored
	for _, m := range results {
		if s := clawhubScore(q, m); s > 0 {
			ranked = append(ranked, scored{m, s})
		}
	}
	slices.SortStableFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if d := strings.Compare(strings.ToLower(a.m.Name), strings.ToLower(b.m.Name)); d != 0 {
			return d
		}
		return strings.Compare(strings.ToLower(a.m.Identifier), strings.ToLower(b.m.Identifier))
	})
	var filtered []Meta
	for _, s := range ranked {
		filtered = append(filtered, s.m)
	}
	filtered = dedupeClawhub(filtered)
	if exact := c.exact(ctx, q); exact != nil {
		kept := []Meta{*exact}
		for _, m := range filtered {
			if clawhubScore(q, m) >= 20 {
				kept = append(kept, m)
			}
		}
		filtered = dedupeClawhub(kept)
	}
	if len(filtered) > 0 || clawhubQueryID.MatchString(q) {
		return head(filtered, limit)
	}
	return head(dedupeClawhub(results), limit)
}

// The skill a query of two words or more names as a slug, else the catalog's best matches, else the listing API's; the whole catalog
// for an empty query
func (c *clawhub) Search(ctx context.Context, query string, limit int, _ string) []Meta {
	query = strings.TrimSpace(query)
	if query != "" {
		if len(terms(query)) >= 2 {
			if direct := c.exact(ctx, query); direct != nil {
				return []Meta{*direct}
			}
		}
		if results := c.searchCatalog(ctx, query, limit); len(results) > 0 {
			return results
		}
	} else if catalog := c.walk(ctx, max(limit, 0)); len(catalog) > 0 {
		deduped := dedupeClawhub(catalog)
		if limit > 0 {
			return head(deduped, limit)
		}
		return deduped
	}
	key := fmt.Sprintf("clawhub_search_listing_v1_%s_%d", md5hex(query), limit)
	var cached []Meta
	if c.r.cached(key, cacheTTL, &cached) {
		return c.rank(ctx, query, cached, limit)
	}
	var raw json.RawMessage
	if !c.r.getJSON(ctx, clawhubBase+"/skills", request{timeout: 15 * time.Second, query: url.Values{"search": {query}, "limit": {strconv.Itoa(limit)}}}, &raw) {
		return nil
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		var wrapped struct {
			Items []json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &wrapped) != nil || wrapped.Items == nil {
			return nil
		}
		items = wrapped.Items
	}
	results := []Meta{}
	for _, item := range head(items, limit) {
		if m := coerceClawhub(item).meta(); m != nil {
			results = append(results, *m)
		}
	}
	final := c.rank(ctx, query, results, limit)
	c.r.keep(key, final)
	return final
}

// Best matches of the catalog for a query, cached per query
func (c *clawhub) searchCatalog(ctx context.Context, query string, limit int) []Meta {
	key := "clawhub_search_catalog_v1_" + md5hex(fmt.Sprintf("%s|%d", query, limit))
	var cached []Meta
	if c.r.cached(key, cacheTTL, &cached) {
		return head(cached, limit)
	}
	catalog := c.walk(ctx, 0)
	if len(catalog) == 0 {
		return nil
	}
	results := c.rank(ctx, query, catalog, limit)
	c.r.keep(key, results)
	return results
}

// The catalog walked page by page through its cursors, cached only when the walk finished; at most enough for most items when that is
// given, within its time budget
func (c *clawhub) walk(ctx context.Context, most int) []Meta {
	const key = "clawhub_catalog_v1"
	var cached []Meta
	if c.r.cached(key, cacheTTL, &cached) {
		return cached
	}
	bounded := most > 0
	deadline := time.Now().Add(clawhubWalkBudget)
	cursor := ""
	results := []Meta{}
	seen := map[string]bool{}
	partial, failures := false, 0
	for range 750 {
		if bounded && time.Now().After(deadline) {
			partial = true
			break
		}
		query := url.Values{"limit": {"200"}}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var page struct {
			Items      []json.RawMessage `json:"items"`
			NextCursor any               `json:"nextCursor"`
		}
		if !c.r.getJSON(ctx, clawhubBase+"/skills", request{timeout: 30 * time.Second, query: query}, &page) {
			failures++
			if failures >= catalogRetries {
				partial = true
				break
			}
			wait := min(time.Duration(1<<failures)*time.Second, 8*time.Second)
			if bounded {
				wait = 500 * time.Millisecond
			}
			if !pause(ctx, wait) {
				partial = true
				break
			}
			continue
		}
		failures = 0
		if len(page.Items) == 0 {
			break
		}
		for _, item := range page.Items {
			p := coerceClawhub(item)
			if slug := p.text("slug"); slug != "" && !seen[slug] {
				seen[slug] = true
				if m := p.meta(); m != nil {
					results = append(results, *m)
				}
			}
		}
		cursor, _ = page.NextCursor.(string)
		if cursor == "" {
			break
		}
		if bounded && len(results) >= most {
			partial = true
			break
		}
	}
	if !partial {
		c.r.keep(key, results)
	}
	return results
}

// The skill downloaded as its archive, or as the files its version lists when the archive has no SKILL.md
func (c *clawhub) Fetch(ctx context.Context, identifier string) *Bundle {
	slug, data, ok := c.detail(ctx, identifier)
	if !ok {
		return nil
	}
	_, owner, _ := parseClawhub(identifier)
	if owner == "" {
		owner = data.owner()
	}
	latest := c.latest(ctx, slug, data, owner)
	if latest == "" {
		slog.Warn(fmt.Sprintf("ClawHub fetch failed for %s: could not resolve latest version", slug))
		return nil
	}
	bundle := &Bundle{Name: slug, Source: "clawhub", Trust: "community"}
	if !c.download(ctx, slug, latest, owner, bundle) {
		return nil
	}
	if _, has := bundle.file("SKILL.md"); !has {
		var raw json.RawMessage
		if c.r.getJSON(ctx, clawhubBase+"/skills/"+slug+"/versions/"+latest, request{query: ownerQuery(owner)}, &raw) {
			var version clawhubPayload
			if json.Unmarshal(raw, &version) == nil && version != nil && version.ownedBy(owner) {
				if files := c.files(ctx, version); len(files) > 0 {
					bundle.Files = files
				}
				var nested clawhubPayload
				if _, has := bundle.file("SKILL.md"); !has && json.Unmarshal(version["version"], &nested) == nil && nested != nil {
					if files := c.files(ctx, nested); len(files) > 0 {
						bundle.Files = files
					}
				}
			}
		}
	}
	if _, has := bundle.file("SKILL.md"); !has {
		slog.Warn(fmt.Sprintf("ClawHub fetch for %s resolved version %s but could not retrieve file content", slug, latest))
		return nil
	}
	bundle.Identifier = slug
	if owner != "" {
		bundle.Identifier = "@" + owner + "/" + slug
	}
	return bundle
}

// What the skill's detail says of it, under its owner's identifier when it has one
func (c *clawhub) Inspect(ctx context.Context, identifier string) *Meta {
	slug, data, ok := c.detail(ctx, identifier)
	if !ok {
		return nil
	}
	if data.text("slug") == "" {
		data["slug"], _ = json.Marshal(slug)
	}
	m := data.meta()
	_, owner, _ := parseClawhub(identifier)
	if owner == "" {
		owner = data.owner()
	}
	if m != nil && owner != "" {
		m.Identifier = "@" + owner + "/" + slug
		m.Extra["owner"] = owner
	}
	return m
}

func (c *clawhub) Revision(context.Context, string) string { return "" }

// Latest version of a skill: its detail's, else the first its versions list
func (c *clawhub) latest(ctx context.Context, slug string, data clawhubPayload, owner string) string {
	var latest struct {
		Version any `json:"version"`
	}
	json.Unmarshal(data["latestVersion"], &latest)
	if version, ok := latest.Version.(string); ok && version != "" {
		return version
	}
	var tags struct {
		Latest any `json:"latest"`
	}
	json.Unmarshal(data["tags"], &tags)
	if version, ok := tags.Latest.(string); ok && version != "" {
		return version
	}
	var raw json.RawMessage
	if !c.r.getJSON(ctx, clawhubBase+"/skills/"+slug+"/versions", request{query: ownerQuery(owner)}, &raw) {
		return ""
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		var wrapped struct {
			Items []json.RawMessage `json:"items"`
		}
		json.Unmarshal(raw, &wrapped)
		items = wrapped.Items
	}
	if len(items) == 0 {
		return ""
	}
	var first struct {
		Version any `json:"version"`
	}
	json.Unmarshal(items[0], &first)
	version, _ := first.Version.(string)
	return version
}

// Text files a version lists: written in it, else fetched from the address each names
func (c *clawhub) files(ctx context.Context, version clawhubPayload) []File {
	var inline map[string]any
	if json.Unmarshal(version["files"], &inline) == nil && inline != nil {
		var out []File
		for path, content := range inline {
			if text, ok := content.(string); ok {
				out = append(out, File{Path: path, Data: []byte(text)})
			}
		}
		slices.SortFunc(out, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		return out
	}
	var listed []map[string]any
	json.Unmarshal(version["files"], &listed)
	bundle := &Bundle{}
	for _, file := range listed {
		first := func(keys ...string) any {
			for _, key := range keys {
				if v := file[key]; config.Truthy(v) {
					return v
				}
			}
			return file[keys[len(keys)-1]]
		}
		name, ok := first("path", "name").(string)
		if !ok || name == "" {
			continue
		}
		if text, ok := file["content"].(string); ok {
			bundle.set(name, []byte(text))
		} else if address, ok := first("rawUrl", "downloadUrl", "url").(string); ok && strings.HasPrefix(address, "http") {
			if data, ok := c.r.fetch(ctx, address); ok {
				bundle.set(name, data)
			}
		}
	}
	return bundle.Files
}

// The text files of the skill's archive into the bundle, a rate limited download tried again; false when the archive holds a file that
// can't be read for its encryption or compression
func (c *clawhub) download(ctx context.Context, slug, version, owner string, bundle *Bundle) bool {
	query := url.Values{"slug": {slug}, "version": {version}}
	if owner != "" {
		query.Set("owner", owner)
	}
	const attempts = 3
	for attempt := range attempts {
		resp := c.r.guarded(ctx, clawhubBase+"/download", request{query: query, timeout: 30 * time.Second, limit: clawhubZipMax})
		if resp == nil {
			return true
		}
		if resp.status == 429 {
			wait := 5 * time.Second
			if after, ok := httpx.RetryAfter(resp.header.Get("Retry-After")); ok {
				wait = after
			}
			wait = min(wait, 15*time.Second).Truncate(time.Second)
			slog.Debug(fmt.Sprintf("ClawHub download rate-limited for %s, retrying in %s (attempt %d/%d)", slug, wait, attempt+1, attempts))
			if attempt < attempts-1 && !pause(ctx, wait) {
				return true
			}
			continue
		}
		if resp.status != 200 {
			slog.Debug(fmt.Sprintf("ClawHub ZIP download for %s v%s returned %d", slug, version, resp.status))
			return true
		}
		if resp.over {
			slog.Debug(fmt.Sprintf("Skipping oversized ClawHub ZIP for %s v%s: exceeded %d bytes", slug, version, clawhubZipMax))
			return true
		}
		archive, err := zip.NewReader(bytes.NewReader(resp.body), int64(len(resp.body)))
		if err != nil {
			slog.Warn(fmt.Sprintf("ClawHub returned invalid ZIP for %s v%s", slug, version))
			return true
		}
		return c.unzip(archive, slug, version, bundle)
	}
	slog.Debug(fmt.Sprintf("ClawHub ZIP download exhausted retries for %s v%s", slug, version))
	return true
}

// Safe, small, UTF-8 members of an archive into the bundle; false for a member whose encryption or compression can't be read
func (c *clawhub) unzip(archive *zip.Reader, slug, version string, bundle *Bundle) bool {
	for _, member := range archive.File {
		if strings.HasSuffix(member.Name, "/") {
			continue
		}
		name, err := bundlePath(member.Name)
		if err != nil {
			slog.Debug("Skipping unsafe ZIP member path: " + member.Name)
			continue
		}
		if member.UncompressedSize64 > clawhubFileMax {
			slog.Debug(fmt.Sprintf("Skipping large file in ZIP: %s (%d bytes)", name, member.UncompressedSize64))
			continue
		}
		if member.Flags&0x1 != 0 {
			slog.Warn(fmt.Sprintf("ClawHub archive of %s v%s holds an encrypted file %s", slug, version, member.Name))
			return false
		}
		rc, err := member.Open()
		if err == zip.ErrAlgorithm {
			slog.Warn(fmt.Sprintf("ClawHub archive of %s v%s compresses %s in a way that can't be read", slug, version, member.Name))
			return false
		}
		if err != nil {
			slog.Warn(fmt.Sprintf("ClawHub returned invalid ZIP for %s v%s", slug, version))
			return true
		}
		data, err := io.ReadAll(io.LimitReader(rc, clawhubFileMax+1))
		rc.Close()
		if err != nil {
			slog.Warn(fmt.Sprintf("ClawHub returned invalid ZIP for %s v%s", slug, version))
			return true
		}
		if !utf8.Valid(data) {
			slog.Debug("Skipping non-text file in ZIP: " + name)
			continue
		}
		bundle.set(name, data)
	}
	return true
}
