package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"golang.org/x/text/encoding/charmap"
)

var clawhubSlugRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Slug and owner of a ClawHub identifier, ok false for GitHub style or malformed ids
func clawhubParseIdentifier(identifier string) (slug, owner string, hasOwner, ok bool) {
	raw := pystr.Strip(identifier)
	if raw == "" {
		return "", "", false, false
	}
	hadAt := strings.HasPrefix(raw, "@")
	stripped := strings.TrimPrefix(strings.TrimPrefix(raw, "@"), "clawhub/")
	if strings.HasPrefix(stripped, "@") {
		hadAt = true
		stripped = strings.TrimPrefix(stripped, "@")
	}
	var parts []string
	for _, p := range strings.Split(stripped, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	switch {
	case len(parts) == 1:
		slug = parts[0]
	case (len(parts) == 2 && hadAt) || (len(parts) == 3 && pystr.Lower(parts[1]) == "skills"):
		owner, slug, hasOwner = parts[0], parts[len(parts)-1], true
	default:
		return "", "", false, false
	}
	if !clawhubSlugRe.MatchString(slug) || (hasOwner && !clawhubSlugRe.MatchString(owner)) {
		return "", "", false, false
	}
	return slug, owner, hasOwner, true
}

// Owner less clawhub/<slug> catalog card id of a ClawHub identifier, ok false for anything else
func clawhubCatalogIdentifier(identifier any) (string, bool) {
	s, isStr := identifier.(string)
	if !isStr {
		return "", false
	}
	slug, _, _, ok := clawhubParseIdentifier(s)
	if !ok {
		return "", false
	}
	return "clawhub/" + slug, true
}

var clawhubTermSplit = regexp.MustCompile(`[^a-z0-9]+`)

var clawhubSearchQueryRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// Lowercased alphanumeric terms of a query
func clawhubQueryTerms(query string) []string {
	out := []string{}
	for _, t := range clawhubTermSplit.Split(pystr.Lower(query), -1) {
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Results deduped by lowercased identifier, the name when the identifier is empty, first wins
func clawhubDedupeResults(results []SkillMeta) []SkillMeta {
	seen := map[string]bool{}
	out := []SkillMeta{}
	for _, r := range results {
		key := r.Identifier
		if key == "" {
			key = r.Name
		}
		key = pystr.Lower(key)
		if !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	return out
}

// Relevance of a result to a query from exact, prefix, term and substring hits
func clawhubSearchScore(query string, meta SkillMeta) int {
	queryNorm := pystr.Lower(pystr.Strip(query))
	if queryNorm == "" {
		return 1
	}
	identifier, name, description := pystr.Lower(meta.Identifier), pystr.Lower(meta.Name), pystr.Lower(meta.Description)
	qTerms, idTerms, nameTerms := clawhubQueryTerms(queryNorm), clawhubQueryTerms(identifier), clawhubQueryTerms(name)
	normID, normName := strings.Join(idTerms, " "), strings.Join(nameTerms, " ")
	prefixed := func(terms []string) bool {
		return len(qTerms) > 0 && slices.Equal(terms[:min(len(qTerms), len(terms))], qTerms)
	}
	score := 0
	for _, c := range []struct {
		points int
		hit    bool
	}{{140, queryNorm == identifier}, {130, queryNorm == name}, {125, normID == queryNorm}, {120, normName == queryNorm},
		{95, strings.HasPrefix(normID, queryNorm)}, {90, strings.HasPrefix(normName, queryNorm)},
		{70, prefixed(idTerms)}, {65, prefixed(nameTerms)}, {40, strings.Contains(identifier, queryNorm)},
		{35, strings.Contains(name, queryNorm)}, {10, strings.Contains(description, queryNorm)}} {
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

// First non empty str among the values
func clawhubFirstStr(values ...any) string {
	for _, v := range values {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Overrides of the ClawHub internals the hermes tests patch
type clawhubSeams struct {
	stream    func(rawURL string, params pyhttpx.Params, timeout time.Duration) *pyhttpx.StreamResponse
	sleep     func(d time.Duration)
	monotonic func() float64
	catalog   func(maxItems int) []SkillMeta
}

// ClawHub registry, every skill community trust
type ClawHubSource struct {
	SourceBase
	h                                          *Hub
	BaseURL                                    string
	CatalogWalkBudgetSeconds                   float64
	ZipDownloadMaxBytes, ZipDownloadChunkBytes int64
	seams                                      clawhubSeams
}

// ClawHub source with the upstream endpoint and download bounds
func NewClawHubSource(h *Hub) *ClawHubSource {
	return &ClawHubSource{SourceBase: SourceBase{ID: "clawhub"}, h: h, BaseURL: "https://clawhub.ai/api/v1",
		CatalogWalkBudgetSeconds: 12, ZipDownloadMaxBytes: 25 * 1024 * 1024, ZipDownloadChunkBytes: 64 * 1024}
}

func (c *ClawHubSource) sleep(seconds float64) {
	d := time.Duration(seconds * float64(time.Second))
	if c.seams.sleep != nil {
		c.seams.sleep(d)
		return
	}
	time.Sleep(d)
}

func (c *ClawHubSource) monotonic() float64 {
	if c.seams.monotonic != nil {
		return c.seams.monotonic()
	}
	return float64(time.Since(processStart)) / float64(time.Second)
}

var processStart = time.Now()

// Plain JSON GET whose escaping exceptions are raised to the method boundary
func (c *ClawHubSource) getJSON(ctx context.Context, rawURL string, timeout time.Duration, params pyhttpx.Params) any {
	v, err := c.h.getJSON(ctx, rawURL, hubGet{Timeout: timeout, Params: params})
	if err != nil {
		hubRaise(err)
	}
	return v
}

// Tag list of a listing, mapping keys apart from latest for a version tag map
func clawhubNormalizeTags(tags any) []string {
	switch t := tags.(type) {
	case []any:
		out := make([]string, len(t))
		for i, v := range t {
			out[i] = pyrepr.Str(v)
		}
		return out
	case *pyjson.Object:
		out := []string{}
		for _, k := range t.Keys {
			if k != "latest" {
				out = append(out, k)
			}
		}
		return out
	}
	return []string{}
}

// Listing shape with a nested skill flattened, latestVersion and owner carried beside it
func clawhubCoerceSkillPayload(data any) *pyjson.Object {
	o, ok := data.(*pyjson.Object)
	if !ok {
		return nil
	}
	nv, _ := o.Get("skill")
	nested, ok := nv.(*pyjson.Object)
	if !ok {
		return o
	}
	merged := pyjson.NewObject()
	for _, k := range nested.Keys {
		v, _ := nested.Get(k)
		merged.Set(k, v)
	}
	if lv, _ := o.Get("latestVersion"); lv != nil {
		if _, has := merged.Get("latestVersion"); !has {
			merged.Set("latestVersion", lv)
		}
	}
	if ov, has := o.Get("owner"); has {
		if _, mHas := merged.Get("owner"); !mHas {
			merged.Set("owner", ov)
		}
	}
	return merged
}

// Stripped owner handle of a payload, empty when absent
func clawhubOwnerFromPayload(data *pyjson.Object) string {
	if data == nil {
		return ""
	}
	owner, _ := data.Get("owner")
	if o, ok := owner.(*pyjson.Object); ok {
		owner, _ = o.Get("handle")
	}
	if s, ok := owner.(string); ok {
		return pystr.Strip(s)
	}
	return ""
}

// True without an expected owner or when the payload owner is absent or equal ignoring case
func clawhubOwnerMatches(expected string, data *pyjson.Object) bool {
	if expected == "" {
		return true
	}
	actual := clawhubOwnerFromPayload(data)
	return actual == "" || pystr.Lower(actual) == pystr.Lower(expected)
}

// Search result of a listing item, nil without a slug
func clawhubItemToMeta(item any) *SkillMeta {
	slug, ok := pyGet(item, "slug", nil).(string)
	if !ok || slug == "" {
		return nil
	}
	o := item.(*pyjson.Object)
	owner := clawhubOwnerFromPayload(o)
	firstTruthy := func(keys ...string) any {
		for _, k := range keys {
			if v, _ := o.Get(k); pyrepr.Truthy(v) {
				return v
			}
		}
		return nil
	}
	name := slug
	if v := firstTruthy("displayName", "name"); v != nil {
		name = pyrepr.Str(v)
	}
	desc := ""
	if v := firstTruthy("summary", "description"); v != nil {
		desc = pyrepr.Str(v)
	}
	extra := pyjson.NewObject()
	if owner != "" {
		extra.Set("owner", owner)
	}
	return &SkillMeta{Name: name, Description: desc, Source: "clawhub", Identifier: slug, TrustLevel: "community",
		Tags: clawhubNormalizeTags(pyGet(o, "tags", []any{})), Extra: extra}
}

func ownerParams(owner string) pyhttpx.Params {
	if owner == "" {
		return nil
	}
	return pyhttpx.Params{{"owner", owner}}
}

// Slug and detail payload of an identifier, ok false when unparsable, missing or owned by someone else
func (c *ClawHubSource) skillDetail(ctx context.Context, identifier string) (string, *pyjson.Object, bool) {
	slug, owner, _, ok := clawhubParseIdentifier(identifier)
	if !ok {
		return "", nil, false
	}
	data := clawhubCoerceSkillPayload(c.getJSON(ctx, c.BaseURL+"/skills/"+slug, 0, ownerParams(owner)))
	if data == nil || !clawhubOwnerMatches(owner, data) {
		return "", nil, false
	}
	return slug, data, true
}

// Detail of the slug the query spells directly or through its suffixed term forms
func (c *ClawHubSource) exactSlugMeta(ctx context.Context, query string) *SkillMeta {
	query = pystr.Strip(query)
	terms := clawhubQueryTerms(query)
	var candidates []string
	if slug, _, _, ok := clawhubParseIdentifier(query); ok {
		candidates = []string{slug}
	} else if !strings.Contains(query, "/") && clawhubSlugRe.MatchString(query) {
		candidates = []string{query}
	}
	if len(terms) > 0 {
		base := strings.Join(terms, "-")
		if len(terms) >= 2 {
			for _, s := range []string{"agent", "skill", "tool", "assistant", "playbook"} {
				candidates = append(candidates, base+"-"+s)
			}
		}
		candidates = append(candidates, base)
	}
	seen := map[string]bool{}
	for _, cand := range candidates {
		if seen[cand] {
			continue
		}
		seen[cand] = true
		if m := c.inspect(ctx, cand); m != nil {
			return m
		}
	}
	return nil
}

// Results ranked by score with the exact slug first, the raw listing when nothing scores
func (c *ClawHubSource) finalizeSearchResults(ctx context.Context, query string, results []SkillMeta, limit int) []SkillMeta {
	queryNorm := pystr.Strip(query)
	if queryNorm == "" {
		return pyHeadMetas(clawhubDedupeResults(results), limit)
	}
	type scored struct {
		m     SkillMeta
		score int
	}
	var ranked []scored
	for _, m := range results {
		if s := clawhubSearchScore(queryNorm, m); s > 0 {
			ranked = append(ranked, scored{m, s})
		}
	}
	slices.SortStableFunc(ranked, func(a, b scored) int {
		if a.score != b.score {
			return b.score - a.score
		}
		if r := strings.Compare(pystr.Lower(a.m.Name), pystr.Lower(b.m.Name)); r != 0 {
			return r
		}
		return strings.Compare(pystr.Lower(a.m.Identifier), pystr.Lower(b.m.Identifier))
	})
	filtered := make([]SkillMeta, len(ranked))
	for i, r := range ranked {
		filtered[i] = r.m
	}
	filtered = clawhubDedupeResults(filtered)
	if exact := c.exactSlugMeta(ctx, queryNorm); exact != nil {
		kept := []SkillMeta{*exact}
		for _, m := range filtered {
			if clawhubSearchScore(queryNorm, m) >= 20 {
				kept = append(kept, m)
			}
		}
		filtered = clawhubDedupeResults(kept)
	}
	if len(filtered) > 0 || clawhubSearchQueryRe.MatchString(queryNorm) {
		return pyHeadMetas(filtered, limit)
	}
	return pyHeadMetas(clawhubDedupeResults(results), limit)
}

// Direct slug hit, catalog search, or the paginated catalog, falling back to the listing API
func (c *ClawHubSource) Search(ctx context.Context, query string, limit int) (out []SkillMeta, err error) {
	defer hubRecover(&err)
	query = pystr.Strip(query)
	if query != "" {
		if len(clawhubQueryTerms(query)) >= 2 {
			if direct := c.exactSlugMeta(ctx, query); direct != nil {
				return []SkillMeta{*direct}, nil
			}
		}
		if results := c.searchCatalog(ctx, query, limit); len(results) > 0 {
			return results, nil
		}
	} else if catalog := c.loadCatalogIndex(ctx, max(limit, 0)); len(catalog) > 0 {
		deduped := clawhubDedupeResults(catalog)
		if limit > 0 {
			return pyHeadMetas(deduped, limit), nil
		}
		return deduped, nil
	}
	key := fmt.Sprintf("clawhub_search_listing_v1_%x_%d", md5.Sum([]byte(query)), limit)
	cached, ok, cerr := c.h.cachedMetas(key)
	if cerr != nil {
		return nil, cerr
	}
	if ok {
		return c.finalizeSearchResults(ctx, query, cached, limit), nil
	}
	data := c.getJSON(ctx, c.BaseURL+"/skills", 15*time.Second, pyhttpx.Params{{"search", query}, {"limit", strconv.Itoa(limit)}})
	skillsData := data
	if o, isObj := data.(*pyjson.Object); isObj {
		skillsData = pyGet(o, "items", data)
	}
	list, isList := skillsData.([]any)
	if !isList {
		return []SkillMeta{}, nil
	}
	if limit < 0 {
		list = list[:max(len(list)+limit, 0)]
	} else {
		list = list[:min(limit, len(list))]
	}
	results := []SkillMeta{}
	for _, item := range list {
		if m := clawhubItemToMeta(item); m != nil {
			results = append(results, *m)
		}
	}
	final := c.finalizeSearchResults(ctx, query, results, limit)
	if err := c.h.cacheMetas(key, final); err != nil {
		return nil, err
	}
	return final, nil
}

// Skill downloaded as the ZIP bundle, the version metadata files when the ZIP lacks SKILL.md
func (c *ClawHubSource) Fetch(ctx context.Context, identifier string) (b *SkillBundle, err error) {
	defer hubRecover(&err)
	slug, data, ok := c.skillDetail(ctx, identifier)
	if !ok {
		return nil, nil
	}
	_, owner, _, _ := clawhubParseIdentifier(identifier)
	if owner == "" {
		owner = clawhubOwnerFromPayload(data)
	}
	log := c.h.S.logger()
	latest := c.resolveLatestVersion(ctx, slug, data, owner)
	if latest == "" {
		log.Warn(fmt.Sprintf("ClawHub fetch failed for %s: could not resolve latest version", slug))
		return nil, nil
	}
	hasSkill := func(files []BundleFile) bool {
		return slices.ContainsFunc(files, func(f BundleFile) bool { return f.Path == "SKILL.md" })
	}
	files := c.downloadZip(ctx, slug, latest, owner)
	if !hasSkill(files) {
		vd := c.getJSON(ctx, c.BaseURL+"/skills/"+slug+"/versions/"+latest, 0, ownerParams(owner))
		if vo, isObj := vd.(*pyjson.Object); isObj && clawhubOwnerMatches(owner, vo) {
			if extracted := c.extractFiles(ctx, vo); len(extracted) > 0 {
				files = extracted
			}
			nested, isNested := pyGet(vo, "version", pyjson.NewObject()).(*pyjson.Object)
			if !hasSkill(files) && isNested {
				if extracted := c.extractFiles(ctx, nested); len(extracted) > 0 {
					files = extracted
				}
			}
		}
	}
	if !hasSkill(files) {
		log.Warn(fmt.Sprintf("ClawHub fetch for %s resolved version %s but could not retrieve file content", slug, latest))
		return nil, nil
	}
	id := slug
	if owner != "" {
		id = "@" + owner + "/" + slug
	}
	return &SkillBundle{Name: slug, Files: files, Source: "clawhub", Identifier: id, TrustLevel: "community"}, nil
}

// Detail metadata of a skill with its owner qualified identifier
func (c *ClawHubSource) Inspect(ctx context.Context, identifier string) (m *SkillMeta, err error) {
	defer hubRecover(&err)
	return c.inspect(ctx, identifier), nil
}

func (c *ClawHubSource) inspect(ctx context.Context, identifier string) *SkillMeta {
	slug, data, ok := c.skillDetail(ctx, identifier)
	if !ok {
		return nil
	}
	item := pyjson.NewObject()
	for _, k := range data.Keys {
		v, _ := data.Get(k)
		item.Set(k, v)
	}
	if v, _ := data.Get("slug"); pyrepr.Truthy(v) {
		item.Set("slug", v)
	} else {
		item.Set("slug", slug)
	}
	meta := clawhubItemToMeta(item)
	_, owner, _, _ := clawhubParseIdentifier(identifier)
	if owner == "" {
		owner = clawhubOwnerFromPayload(data)
	}
	if meta != nil && owner != "" {
		meta.Identifier = "@" + owner + "/" + slug
		meta.Extra.Set("owner", owner)
	}
	return meta
}

// Ranked catalog matches through the per query cache
func (c *ClawHubSource) searchCatalog(ctx context.Context, query string, limit int) []SkillMeta {
	key := fmt.Sprintf("clawhub_search_catalog_v1_%x", md5.Sum([]byte(fmt.Sprintf("%s|%d", query, limit))))
	cached, ok, err := c.h.cachedMetas(key)
	if err != nil {
		hubRaise(err)
	}
	if ok {
		return pyHeadMetas(cached, limit)
	}
	catalog := c.loadCatalogIndex(ctx, 0)
	if len(catalog) == 0 {
		return []SkillMeta{}
	}
	results := c.finalizeSearchResults(ctx, query, catalog, limit)
	if err := c.h.cacheMetas(key, results); err != nil {
		hubRaise(err)
	}
	return results
}

// Cursor walk of the catalog, cached only when the walk completed
func (c *ClawHubSource) loadCatalogIndex(ctx context.Context, maxItems int) []SkillMeta {
	if c.seams.catalog != nil {
		return c.seams.catalog(maxItems)
	}
	const key = "clawhub_catalog_v1"
	cached, ok, err := c.h.cachedMetas(key)
	if err != nil {
		hubRaise(err)
	}
	if ok {
		return cached
	}
	cursor := ""
	results := []SkillMeta{}
	seen := map[string]bool{}
	bounded := maxItems > 0
	deadline := 0.0
	if bounded {
		deadline = c.monotonic() + c.CatalogWalkBudgetSeconds
	}
	partial, failures := false, 0
	for range 750 {
		if bounded && c.monotonic() > deadline {
			partial = true
			break
		}
		params := pyhttpx.Params{{"limit", "200"}}
		if cursor != "" {
			params = append(params, [2]string{"cursor", cursor})
		}
		data := c.getJSON(ctx, c.BaseURL+"/skills", 30*time.Second, params)
		if data == nil {
			failures++
			if failures >= CatalogPageRetries {
				partial = true
				break
			}
			if bounded {
				c.sleep(0.5)
			} else {
				c.sleep(math.Min(math.Pow(2, float64(failures)), 8))
			}
			continue
		}
		failures = 0
		obj, isObj := data.(*pyjson.Object)
		var items []any
		if isObj {
			iv, _ := obj.Get("items")
			if _, has := obj.Get("items"); !has {
				iv = []any{}
			}
			items, _ = iv.([]any)
		}
		if len(items) == 0 {
			break
		}
		for _, item := range items {
			slug, isStr := pyGet(item, "slug", nil).(string)
			if isStr && slug != "" && !seen[slug] {
				seen[slug] = true
				if m := clawhubItemToMeta(item); m != nil {
					results = append(results, *m)
				}
			}
		}
		cursor = ""
		if isObj {
			cv, _ := obj.Get("nextCursor")
			cursor, _ = cv.(string)
		}
		if cursor == "" {
			break
		}
		if bounded && len(results) >= maxItems {
			partial = true
			break
		}
	}
	if !partial {
		if err := c.h.cacheMetas(key, results); err != nil {
			hubRaise(err)
		}
	}
	return results
}

// Latest version from the payload, else the first entry of the versions listing
func (c *ClawHubSource) resolveLatestVersion(ctx context.Context, slug string, data *pyjson.Object, owner string) string {
	latest, _ := data.Get("latestVersion")
	tags, _ := data.Get("tags")
	var lv, tv any
	if o, ok := latest.(*pyjson.Object); ok {
		lv, _ = o.Get("version")
	}
	if o, ok := tags.(*pyjson.Object); ok {
		tv, _ = o.Get("latest")
	}
	if v := clawhubFirstStr(lv, tv); v != "" {
		return v
	}
	vd := c.getJSON(ctx, c.BaseURL+"/skills/"+slug+"/versions", 0, ownerParams(owner))
	if o, ok := vd.(*pyjson.Object); ok {
		vd, _ = o.Get("items")
	}
	if list, ok := vd.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(*pyjson.Object); ok {
			v, _ := first.Get("version")
			return clawhubFirstStr(v)
		}
	}
	return ""
}

// Owner handle from the detail API with three attempts honouring Retry-After on 429
func (c *ClawHubSource) fetchOwnerHandle(ctx context.Context, slug string) string {
	url := c.BaseURL + "/skills/" + slug
	const maxAttempts = 3
	for attempt := range maxAttempts {
		delay := 2.0 * math.Pow(2, float64(attempt))
		var reason string
		resp, err := c.h.skillsHubHTTPGet(ctx, url, hubGet{Timeout: 20 * time.Second})
		if err != nil {
			if pass := hubPassThrough(err); pass != nil {
				hubRaise(pass)
			}
			reason = "transport error"
		} else {
			switch {
			case resp.StatusCode == 200:
				raw, jerr := resp.JSON()
				if jerr != nil {
					return ""
				}
				return clawhubOwnerFromPayload(clawhubCoerceSkillPayload(raw))
			case resp.StatusCode == 429:
				if ra, ok := pyhttpx.RetryAfterSeconds(resp.Header); ok {
					delay = ra
				}
				reason = "HTTP 429"
			case resp.StatusCode >= 500 && resp.StatusCode < 600:
				reason = fmt.Sprintf("HTTP %d", resp.StatusCode)
			default:
				return ""
			}
		}
		if attempt >= maxAttempts-1 {
			return ""
		}
		c.h.S.logger().Debug(fmt.Sprintf("_fetch_owner_handle(%s): %s on attempt %d/%d, retrying in %.1fs", slug, reason, attempt+1, maxAttempts, delay))
		c.sleep(delay)
	}
	return ""
}

// Fills missing owner handles in place through a worker pool, stopping on budget or fifty straight failures
func (c *ClawHubSource) EnrichOwners(ctx context.Context, skills []SkillMeta, maxWorkers int, budgetSeconds *float64) int {
	var needs []int
	for i, s := range skills {
		if s.Source != "clawhub" {
			continue
		}
		var owner any
		if s.Extra != nil {
			owner, _ = s.Extra.Get("owner")
		}
		if !pyrepr.Truthy(owner) {
			needs = append(needs, i)
		}
	}
	if len(needs) == 0 {
		return 0
	}
	log := c.h.S.logger()
	deadline := 0.0
	if budgetSeconds != nil {
		deadline = c.monotonic() + *budgetSeconds
	}
	type done struct {
		idx    int
		handle string
	}
	jobs := make(chan int)
	results := make(chan done, len(needs))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range max(maxWorkers, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				var handle string
				var err error
				func() {
					defer hubRecover(&err)
					handle = c.fetchOwnerHandle(ctx, skills[idx].Identifier)
				}()
				if err != nil {
					handle = ""
				}
				results <- done{idx, handle}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, idx := range needs {
			select {
			case jobs <- idx:
			case <-stop:
				return
			}
		}
	}()
	enriched, failures, processed := 0, 0, 0
	for processed < len(needs) {
		r := <-results
		processed++
		if budgetSeconds != nil && c.monotonic() > deadline {
			log.Warn(fmt.Sprintf("ClawHub owner enrichment: budget of %.0fs exhausted after %d/%d (%d enriched) — shipping the rest without owner handles.",
				*budgetSeconds, processed, len(needs), enriched))
			break
		}
		if r.handle != "" {
			failures = 0
			if skills[r.idx].Extra == nil {
				skills[r.idx].Extra = pyjson.NewObject()
			}
			skills[r.idx].Extra.Set("owner", r.handle)
			enriched++
		} else {
			failures++
		}
		if processed%1000 == 0 {
			log.Info(fmt.Sprintf("ClawHub owner enrichment: %d/%d processed, %d enriched", processed, len(needs), enriched))
		}
		if failures >= 50 {
			log.Warn(fmt.Sprintf("ClawHub owner enrichment: %d consecutive failures — aborting early (%d/%d processed, %d enriched). The ClawHub API may be down or rate-limited.",
				50, processed, len(needs), enriched))
			break
		}
	}
	close(stop)
	wg.Wait()
	return enriched
}

// Text files of version metadata, inline content first then a guarded raw URL fetch
func (c *ClawHubSource) extractFiles(ctx context.Context, versionData *pyjson.Object) []BundleFile {
	files := []BundleFile{}
	fl, _ := versionData.Get("files")
	if o, ok := fl.(*pyjson.Object); ok {
		for _, k := range o.Keys {
			if v, _ := o.Get(k); v != nil {
				if s, isStr := v.(string); isStr {
					files = append(files, BundleFile{Path: k, Data: []byte(s)})
				}
			}
		}
		return files
	}
	list, _ := fl.([]any)
	for _, fm := range list {
		meta, ok := fm.(*pyjson.Object)
		if !ok {
			continue
		}
		first := func(keys ...string) any {
			for _, k := range keys {
				if v, _ := meta.Get(k); pyrepr.Truthy(v) {
					return v
				}
			}
			v, _ := meta.Get(keys[len(keys)-1])
			return v
		}
		fname, isStr := first("path", "name").(string)
		inline, _ := meta.Get("content")
		rawURL := first("rawUrl", "downloadUrl", "url")
		if !isStr || fname == "" {
			continue
		}
		if s, ok := inline.(string); ok {
			setBundleFile(&files, fname, []byte(s), false)
		} else if u, ok := rawURL.(string); ok && strings.HasPrefix(u, "http") {
			if content, ok := c.h.fetchText(ctx, u); ok {
				setBundleFile(&files, fname, []byte(content), false)
			}
		}
	}
	return files
}

func (c *ClawHubSource) stream(ctx context.Context, rawURL string, params pyhttpx.Params, timeout time.Duration) *pyhttpx.StreamResponse {
	if c.seams.stream != nil {
		return c.seams.stream(rawURL, params, timeout)
	}
	return c.h.guardedHTTPStream(ctx, rawURL, params, timeout)
}

// Text members of the bounded streamed skill ZIP, retrying a rate limited download
func (c *ClawHubSource) downloadZip(ctx context.Context, slug, version, owner string) []BundleFile {
	files := []BundleFile{}
	params := pyhttpx.Params{{"slug", slug}, {"version", version}}
	if owner != "" {
		params = append(params, [2]string{"owner", owner})
	}
	log := c.h.S.logger()
	const maxRetries = 3
	for attempt := range maxRetries {
		archive, retryAfter, stop := c.readZipResponse(ctx, slug, version, params, attempt, maxRetries)
		if stop {
			return files
		}
		if retryAfter >= 0 {
			if attempt < maxRetries-1 {
				c.sleep(float64(retryAfter))
			}
			continue
		}
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			log.Warn(fmt.Sprintf("ClawHub returned invalid ZIP for %s v%s", slug, version))
			return files
		}
		if bad := c.extractZip(zr, &files); bad {
			log.Warn(fmt.Sprintf("ClawHub returned invalid ZIP for %s v%s", slug, version))
		}
		return files
	}
	log.Debug(fmt.Sprintf("ClawHub ZIP download exhausted retries for %s v%s", slug, version))
	return files
}

// One download attempt, the archive bytes or the capped 429 delay, stop set when the download ends empty
func (c *ClawHubSource) readZipResponse(ctx context.Context, slug, version string, params pyhttpx.Params, attempt, maxRetries int) ([]byte, int, bool) {
	log := c.h.S.logger()
	resp := c.stream(ctx, c.BaseURL+"/download", params, 30*time.Second)
	if resp == nil {
		return nil, -1, true
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		wait := 5.0
		if ra, ok := pyhttpx.RetryAfterSeconds(resp.Header); ok {
			wait = ra
		}
		if math.IsInf(wait, 0) {
			hubRaise(errors.New("cannot convert float infinity to integer"))
		}
		retryAfter := min(int(wait), 15)
		log.Debug(fmt.Sprintf("ClawHub download rate-limited for %s, retrying in %ds (attempt %d/%d)", slug, retryAfter, attempt+1, maxRetries))
		return nil, retryAfter, false
	}
	if resp.StatusCode != 200 {
		log.Debug(fmt.Sprintf("ClawHub ZIP download for %s v%s returned %d", slug, version, resp.StatusCode))
		return nil, -1, true
	}
	if cl := pyhttpx.HeaderGet(resp.Header, "content-length", ""); cl != "" {
		declared := int64(0)
		if n, ok := pyint.Parse(cl); ok && n.IsInt64() {
			declared = n.Int64()
		} else if ok {
			declared = math.MaxInt64
		}
		if declared > c.ZipDownloadMaxBytes {
			log.Debug(fmt.Sprintf("Skipping oversized ClawHub ZIP for %s v%s: %d bytes", slug, version, declared))
			return nil, -1, true
		}
	}
	var archive bytes.Buffer
	buf := make([]byte, max(c.ZipDownloadChunkBytes, 1))
	var total int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > c.ZipDownloadMaxBytes {
				log.Debug(fmt.Sprintf("Skipping oversized ClawHub ZIP for %s v%s: exceeded %d bytes", slug, version, c.ZipDownloadMaxBytes))
				return nil, -1, true
			}
			archive.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Debug(fmt.Sprintf("ClawHub ZIP download failed for %s v%s: %s", slug, version, err))
			return nil, -1, true
		}
	}
	return archive.Bytes(), -1, false
}

// Safe small UTF-8 members of the archive, bad set when a member read hits a corrupt entry
func (c *ClawHubSource) extractZip(zr *zip.Reader, files *[]BundleFile) (bad bool) {
	log := c.h.S.logger()
	byName := map[string]*zip.File{}
	names := make([]string, len(zr.File))
	for i, f := range zr.File {
		names[i] = zipMemberName(f)
		byName[names[i]] = f
	}
	for i, f := range zr.File {
		filename := names[i]
		if strings.HasSuffix(filename, "/") {
			continue
		}
		name, err := validateBundleRelPath(filename)
		if err != nil {
			log.Debug(fmt.Sprintf("Skipping unsafe ZIP member path: %s", filename))
			continue
		}
		if f.UncompressedSize64 > 500_000 {
			log.Debug(fmt.Sprintf("Skipping large file in ZIP: %s (%d bytes)", name, f.UncompressedSize64))
			continue
		}
		member := byName[filename]
		if member.Flags&1 != 0 {
			hubRaise(fmt.Errorf("File %s is encrypted, password required for extraction", pyrepr.Repr(filename)))
		}
		rc, err := member.Open()
		if errors.Is(err, zip.ErrAlgorithm) {
			hubRaise(errors.New("That compression method is not supported"))
		}
		if err != nil {
			return true
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return true
		}
		if !utf8.Valid(data) {
			log.Debug(fmt.Sprintf("Skipping non-text file in ZIP: %s", name))
			continue
		}
		setBundleFile(files, name, data, false)
	}
	return false
}

// Member name as CPython zipfile decodes it, cp437 without the UTF-8 flag and cut at a NUL
func zipMemberName(f *zip.File) string {
	name := f.Name
	if f.Flags&0x800 == 0 {
		if decoded, err := charmap.CodePage437.NewDecoder().String(name); err == nil {
			name = decoded
		}
	}
	if i := strings.IndexByte(name, 0); i >= 0 {
		name = name[:i]
	}
	return name
}
