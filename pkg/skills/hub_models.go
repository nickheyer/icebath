package skills

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Search result metadata of one registry skill
type SkillMeta struct {
	Name, Description, Source, Identifier, TrustLevel string
	Repo, Path                                        *string
	Tags                                              []string
	Extra                                             *pyjson.Object
}

// One bundle member, Binary set when the registry delivered bytes rather than text
type BundleFile struct {
	Path   string
	Data   []byte
	Binary bool
}

// Downloaded skill awaiting quarantine, scan and install, files in registry order
type SkillBundle struct {
	Name                           string
	Files                          []BundleFile
	Source, Identifier, TrustLevel string
	Metadata                       *pyjson.Object
}

// Content of a bundle member by its exact path
func (b *SkillBundle) File(path string) ([]byte, bool) {
	for _, f := range b.Files {
		if f.Path == path {
			return f.Data, true
		}
	}
	return nil, false
}

// Field mapping of a search result in declaration order
func skillMetaToDict(m SkillMeta) *pyjson.Object {
	o := pyjson.NewObject()
	o.Set("name", m.Name)
	o.Set("description", m.Description)
	o.Set("source", m.Source)
	o.Set("identifier", m.Identifier)
	o.Set("trust_level", m.TrustLevel)
	for _, kv := range []struct {
		key string
		p   *string
	}{{"repo", m.Repo}, {"path", m.Path}} {
		if kv.p == nil {
			o.Set(kv.key, nil)
		} else {
			o.Set(kv.key, *kv.p)
		}
	}
	tags := make([]any, len(m.Tags))
	for i, t := range m.Tags {
		tags[i] = t
	}
	o.Set("tags", tags)
	if m.Extra == nil {
		o.Set("extra", pyjson.NewObject())
	} else {
		o.Set("extra", m.Extra)
	}
	return o
}

// Case insensitive substring match of the query against the joined fields, lists space joined
func matchesQuery(queryLower string, fields ...any) bool {
	parts := make([]string, len(fields))
	for i, f := range fields {
		if list, ok := f.([]any); ok {
			items := make([]string, len(list))
			for j, t := range list {
				items[j] = pyrepr.Str(t)
			}
			parts[i] = strings.Join(items, " ")
		} else if list, ok := f.([]string); ok {
			parts[i] = strings.Join(list, " ")
		} else {
			parts[i] = pyrepr.Str(f)
		}
	}
	return strings.Contains(pystr.Lower(strings.Join(parts, " ")), queryLower)
}

// Items matching the query in order converted to results until limit hits
func firstMatching[T any](queryLower string, items []T, fieldsOf func(T) []any, toMeta func(T) *SkillMeta, limit int) []SkillMeta {
	results := []SkillMeta{}
	for _, item := range items {
		if matchesQuery(queryLower, fieldsOf(item)...) {
			if meta := toMeta(item); meta != nil {
				results = append(results, *meta)
			}
		}
		if len(results) >= limit {
			break
		}
	}
	return results
}

// Ordering of trust levels, unknown levels rank as community
var TrustRank = map[string]int{"builtin": 2, "trusted": 1, "community": 0}

// Results deduped by identifier keeping the higher trust copy at the first position seen
func dedupeByTrust(results []SkillMeta) []SkillMeta {
	index := map[string]int{}
	out := []SkillMeta{}
	for _, r := range results {
		i, seen := index[r.Identifier]
		if !seen {
			index[r.Identifier] = len(out)
			out = append(out, r)
		} else if TrustRank[r.TrustLevel] > TrustRank[out[i].TrustLevel] {
			out[i] = r
		}
	}
	return out
}

// Consecutive failed catalog page fetches before a walk gives up as partial
const CatalogPageRetries = 5

// Registry adapter contract every hub source implements
type SkillSource interface {
	Search(query string, limit int) ([]SkillMeta, error)
	Fetch(identifier string) (*SkillBundle, error)
	Inspect(identifier string) (*SkillMeta, error)
	SourceID() string
	TrustLevelFor(identifier string) string
	CurrentRevision(identifier string) (string, error)
}

// Defaults of a source, community trust for every identifier and no cheap revision probe
type SourceBase struct{ ID, Trust string }

// Source id of the adapter
func (b SourceBase) SourceID() string { return b.ID }

// Trust level given to every identifier, community when unset
func (b SourceBase) TrustLevelFor(string) string {
	if b.Trust == "" {
		return "community"
	}
	return b.Trust
}

// Empty revision so update checks take the full fetch path
func (b SourceBase) CurrentRevision(string) (string, error) { return "", nil }

var hubFrontmatterEnd = pyMust(`\n---\s*\n`)

// YAML frontmatter mapping of a SKILL.md, empty when absent or invalid
func hubParseFrontmatter(content string) map[string]any {
	content = strings.TrimLeft(content, string(rune(0xFEFF)))
	if !strings.HasPrefix(content, "---") {
		return map[string]any{}
	}
	rest := content[3:]
	m, err := hubFrontmatterEnd.FindStringMatch(rest)
	if err != nil || m == nil {
		return map[string]any{}
	}
	end, _ := m.ByteRange()
	parsed, err := yamlLoad(rest[:end])
	if err != nil {
		return map[string]any{}
	}
	if fm, ok := parsed.(map[string]any); ok {
		return fm
	}
	return map[string]any{}
}

// Tags under metadata.icebath or metadata.hermes, an empty list when absent
func hubTags(fm map[string]any) any {
	meta, ok := fm["metadata"].(map[string]any)
	if !ok {
		return []any{}
	}
	for _, key := range []string{"icebath", "hermes"} {
		if _, present := meta[key]; present {
			m, isMap := meta[key].(map[string]any)
			if !isMap {
				return []any{}
			}
			if tags, has := m["tags"]; has {
				return tags
			}
			return []any{}
		}
	}
	return []any{}
}

// Human facing provenance url of a bundle, explicit metadata first then the github tree
func sourceURLForBundle(b *SkillBundle) string {
	if b.Metadata != nil {
		for _, key := range []string{"source_url", "url"} {
			if v, ok := b.Metadata.Get(key); ok && pyTruthyJSON(v) {
				return pyrepr.Str(v)
			}
		}
	}
	if b.Source == "github" {
		parts := strings.SplitN(b.Identifier, "/", 3)
		if len(parts) >= 2 {
			suffix := ""
			if len(parts) == 3 {
				suffix = "/tree/main/" + parts[2]
			}
			return "https://github.com/" + parts[0] + "/" + parts[1] + suffix
		}
	}
	return b.Identifier
}

// Normalized slash path of a bundle controlled value, an error for absolute, traversal, colon or empty paths
func normalizeBundlePath(pathValue, fieldName string, allowNested bool) (string, error) {
	raw := pystr.Strip(pathValue)
	if raw == "" {
		return "", fmt.Errorf("Unsafe %s: empty path", fieldName)
	}
	normalized := strings.ReplaceAll(raw, "\\", "/")
	parts := []string{}
	for _, part := range strings.Split(normalized, "/") {
		if part != "" && part != "." {
			parts = append(parts, part)
		}
	}
	if strings.HasPrefix(normalized, "/") || len(parts) == 0 || slices.Contains(parts, "..") ||
		slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(p, ":") }) || (!allowNested && len(parts) != 1) {
		return "", fmt.Errorf("Unsafe %s: %s", fieldName, pathValue)
	}
	return strings.Join(parts, "/"), nil
}

// Single component hub skill name or the unsafe name error
func validateHubSkillName(name string) (string, error) {
	return normalizeBundlePath(name, "skill name", false)
}

// Nested install parent path or the unsafe path error
func validateInstallParentPath(category string) (string, error) {
	return normalizeBundlePath(category, "install parent path", true)
}

// Nested bundle member path or the unsafe path error
func validateBundleRelPath(relPath string) (string, error) {
	return normalizeBundlePath(relPath, "bundle file path", true)
}

// Lock install path that is relative, traversal free and ends with the skill name
func normalizeLockInstallPath(installPath, skillName string) (string, error) {
	safeName, err := validateHubSkillName(skillName)
	if err != nil {
		return "", err
	}
	normalized, err := normalizeBundlePath(installPath, "install path", true)
	if err != nil {
		return "", err
	}
	if parts := strings.Split(normalized, "/"); parts[len(parts)-1] != safeName {
		return "", fmt.Errorf("Unsafe install path: %s", installPath)
	}
	return normalized, nil
}

var (
	allowedSupportDirs   = map[string]bool{"references": true, "templates": true, "scripts": true, "assets": true, "examples": true}
	localLinkRe          = pyMust(`(?m)(?:\]\(|` + "`" + `|(?:^|[\s"']))((?:references|templates|scripts|assets|examples)/[^\s)` + "`" + `"'<>]+)`)
	suspiciousLocalRefRe = pyMust(`(?:references|templates|scripts|assets|examples)/(?:[^\s)` + "`" + `"'<>]*/)?\.\.(?:/|$)`)
	valuelessQueryFlagRe = pyMust(`(?:[A-Za-z0-9_~-]|%[0-9A-Fa-f]{2})+\Z`)
	samedirLinkRe        = pyMust(`\]\(([^)\s"'<>]+)`)
	samedirNameRe        = pyMust(`^(?:\./)?[A-Za-z0-9][A-Za-z0-9._-]*$`)
	globCharRe           = pyMust(`[*?<>]`)
	alnumEndRe           = pyMust(`[A-Za-z0-9]$`)
)

// Python pattern compiled with Python matching semantics, panicking on an invalid pattern
func pyMust(p string) *regexp2.Regexp {
	re, err := pyre.Compile(p)
	if err != nil {
		panic(err)
	}
	return re
}

// Whether the regexp2 pattern matches anywhere in s
func reSearch(re *regexp2.Regexp, s string) bool {
	ok, err := re.MatchString(s)
	return err == nil && ok
}

// First capture group of every match in order
func reGroups(re *regexp2.Regexp, s string) []string {
	var out []string
	m, err := re.FindStringMatch(s)
	for err == nil && m != nil {
		out = append(out, m.GroupByNumber(1).String())
		m, err = re.FindNextMatch(m)
	}
	return out
}

// Whether a url query is real key value or token flag syntax rather than glob prose
func queryIsConcrete(query string) bool {
	for _, part := range strings.Split(query, "&") {
		if k, _, ok := strings.Cut(part, "="); ok && k != "" {
			continue
		}
		m, err := valuelessQueryFlagRe.FindStringMatch(part)
		if err != nil || m == nil {
			return false
		}
		if start, _ := m.ByteRange(); start != 0 {
			return false
		}
	}
	return true
}

// Safe support and same directory paths a SKILL.md references, ok false on a traversal attempt, err the urlsplit ValueError
func referencedSupportPaths(skillMD string) (paths map[string]bool, ok bool, err error) {
	normalized := strings.ReplaceAll(skillMD, "\\", "/")
	if reSearch(suspiciousLocalRefRe, normalized) {
		return nil, false, nil
	}
	paths = map[string]bool{}
	for _, group := range reGroups(localLinkRe, normalized) {
		candidate := pystr.RstripChars(group, ".,;:")
		parsed, err := pyurl.Split(candidate)
		if err != nil {
			return nil, false, err
		}
		raw := pyurl.Unquote(parsed.Path)
		if strings.HasSuffix(candidate, "?") || strings.ContainsAny(raw, "*?[]") || (parsed.Query != "" && !queryIsConcrete(parsed.Query)) {
			continue
		}
		safe, err := validateBundleRelPath(raw)
		if err != nil {
			return nil, false, nil
		}
		if allowedSupportDirs[strings.SplitN(safe, "/", 2)[0]] {
			base := safe[strings.LastIndex(safe, "/")+1:]
			if reSearch(globCharRe, safe) || !reSearch(alnumEndRe, base) {
				continue
			}
			paths[safe] = true
		}
	}
	for _, group := range reGroups(samedirLinkRe, normalized) {
		raw := pystr.RstripChars(group, ".,;:")
		parsed, err := pyurl.Split(raw)
		if err != nil {
			return nil, false, err
		}
		name := strings.TrimPrefix(pyurl.Unquote(parsed.Path), "./")
		if name == "" || strings.Contains(raw, "://") || strings.HasPrefix(raw, "mailto:") || strings.HasPrefix(raw, "#") || strings.HasPrefix(raw, "/") {
			continue
		}
		if strings.HasPrefix(name, "..") {
			slog.Default().Warn("SKILL.md links outside the skill directory; installing without it", "link", raw)
			continue
		}
		if strings.Contains(name, "/") || pystr.Casefold(name) == "skill.md" || !strings.Contains(strings.TrimLeft(name, "."), ".") || !reSearch(samedirNameRe, name) {
			continue
		}
		safe, err := validateBundleRelPath(name)
		if err != nil {
			return nil, false, nil
		}
		paths[safe] = true
	}
	folded := map[string][]string{}
	for _, p := range slices.Sorted(maps.Keys(paths)) {
		folded[pystr.Casefold(p)] = append(folded[pystr.Casefold(p)], p)
	}
	for _, group := range folded {
		if len(group) > 1 {
			for _, p := range group {
				delete(paths, p)
			}
		}
	}
	return paths, true, nil
}

// Search result rebuilt from its cached field mapping, the TypeError SkillMeta(**item) raises on a bad item
func skillMetaFromDict(v any) (SkillMeta, error) {
	o, ok := v.(*pyjson.Object)
	if !ok {
		return SkillMeta{}, fmt.Errorf("SkillMeta() argument after ** must be a mapping, not %s", pyrepr.TypeName(v))
	}
	var m SkillMeta
	seen := map[string]bool{}
	str := func(key string, val any) (string, error) {
		s, isStr := val.(string)
		if !isStr {
			return "", fmt.Errorf("SkillMeta field %s holds %s, not str", key, pyrepr.TypeName(val))
		}
		return s, nil
	}
	for _, key := range o.Keys {
		val := o.Vals[key]
		seen[key] = true
		var err error
		switch key {
		case "name":
			m.Name, err = str(key, val)
		case "description":
			m.Description, err = str(key, val)
		case "source":
			m.Source, err = str(key, val)
		case "identifier":
			m.Identifier, err = str(key, val)
		case "trust_level":
			m.TrustLevel, err = str(key, val)
		case "repo", "path":
			var p *string
			if val != nil {
				s, serr := str(key, val)
				p, err = &s, serr
			}
			if key == "repo" {
				m.Repo = p
			} else {
				m.Path = p
			}
		case "tags":
			list, isList := val.([]any)
			if !isList {
				return m, fmt.Errorf("SkillMeta field tags holds %s, not list", pyrepr.TypeName(val))
			}
			m.Tags = make([]string, 0, len(list))
			for _, t := range list {
				s, serr := str("tags", t)
				if serr != nil {
					return m, serr
				}
				m.Tags = append(m.Tags, s)
			}
		case "extra":
			ex, isObj := val.(*pyjson.Object)
			if !isObj {
				return m, fmt.Errorf("SkillMeta field extra holds %s, not dict", pyrepr.TypeName(val))
			}
			m.Extra = ex
		default:
			return m, fmt.Errorf("SkillMeta.__init__() got an unexpected keyword argument '%s'", key)
		}
		if err != nil {
			return m, err
		}
	}
	for _, key := range []string{"name", "description", "source", "identifier", "trust_level"} {
		if !seen[key] {
			return m, fmt.Errorf("SkillMeta.__init__() missing 1 required positional argument: '%s'", key)
		}
	}
	return m, nil
}

// Search results from the shared index cache, ok false on miss or expiry
func (h *Hub) cachedMetas(key string) ([]SkillMeta, bool, error) {
	cached, err := h.readIndexCache(key)
	if err != nil || cached == nil {
		return nil, false, err
	}
	list, ok := cached.([]any)
	if !ok {
		return nil, false, fmt.Errorf("'%s' object is not iterable as SkillMeta items", pyrepr.TypeName(cached))
	}
	out := make([]SkillMeta, 0, len(list))
	for _, item := range list {
		m, err := skillMetaFromDict(item)
		if err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	return out, true, nil
}

// Writes search results to the shared index cache
func (h *Hub) cacheMetas(key string, metas []SkillMeta) error {
	items := make([]any, len(metas))
	for i, m := range metas {
		items[i] = skillMetaToDict(m)
	}
	return h.writeIndexCache(key, items)
}

// Cached value passing valid, otherwise computed and written back when not nil
func (h *Hub) memoJSON(key string, compute func() (any, error), valid func(any) bool) (any, error) {
	cached, err := h.readIndexCache(key)
	if err != nil {
		return nil, err
	}
	if valid == nil {
		valid = func(c any) bool { return c != nil }
	}
	if valid(cached) {
		return cached, nil
	}
	data, err := compute()
	if err != nil {
		return nil, err
	}
	if data != nil {
		if err := h.writeIndexCache(key, data); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// Errors the plain hub GET helpers let through, every other transport failure reads as no answer
func hubPassThrough(err error) error {
	var blocked *tools.SSRFConnectionBlocked
	if errors.As(err, &blocked) {
		return err
	}
	var ude *pystr.UnicodeDecodeError
	if errors.As(err, &ude) {
		return err
	}
	return nil
}

// Plain GET decoded as JSON, nil on a non 200 status or a transport or decode failure
func (h *Hub) getJSON(ctx context.Context, rawURL string, o hubGet) (any, error) {
	if o.Timeout == 0 {
		o.Timeout = defaultHubHTTPTimeout
	}
	resp, err := h.skillsHubHTTPGet(ctx, rawURL, o)
	if err != nil {
		return nil, hubPassThrough(err)
	}
	if resp.StatusCode != 200 {
		return nil, nil
	}
	v, err := resp.JSON()
	if err != nil {
		return nil, hubPassThrough(err)
	}
	return v, nil
}

// Plain GET body text on 200, ok false on any other status or a transport failure
func (h *Hub) getText(ctx context.Context, rawURL string, o hubGet) (string, bool, error) {
	if o.Timeout == 0 {
		o.Timeout = defaultHubHTTPTimeout
	}
	resp, err := h.skillsHubHTTPGet(ctx, rawURL, o)
	if err != nil {
		return "", false, hubPassThrough(err)
	}
	if resp.StatusCode != 200 {
		return "", false, nil
	}
	return resp.Text(), true, nil
}

// Policy guarded GET body text on 200
func (h *Hub) fetchText(ctx context.Context, rawURL string) (string, bool) {
	resp := h.guardedHTTPGet(ctx, rawURL, defaultHubHTTPTimeout, nil)
	if resp == nil || resp.StatusCode != 200 {
		return "", false
	}
	return resp.Text(), true
}

// Policy guarded GET body bytes on 200
func (h *Hub) fetchBytes(ctx context.Context, rawURL string) ([]byte, bool) {
	resp := h.guardedHTTPGet(ctx, rawURL, defaultHubHTTPTimeout, nil)
	if resp == nil || resp.StatusCode != 200 {
		return nil, false
	}
	return resp.Content, true
}
