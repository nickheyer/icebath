package skills

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	HermesIndexURL = "https://hermes-agent.nousresearch.com/docs/api/skills-index.json"
	HermesIndexTTL = 6 * 3600.0
)

// Sources the centralized index covers, skipped while it answers an unfiltered search
var apiSourceIDs = map[string]bool{"github": true, "skills-sh": true, "clawhub": true, "lobehub": true, "well-known": true}

// Registries asked after an index miss, GitHub excluded to protect the anonymous rate limit
var indexMissFallbackIDs = map[string]bool{"skills-sh": true, "clawhub": true, "lobehub": true, "well-known": true}

var indexMissFallbackBudget = 8 * time.Second

func (h *Hub) hermesIndexCacheFile() string {
	return filepath.Join(h.IndexCacheDir(), "hermes-index.json")
}

// Centralized skills index cached for the TTL, the stale cache on any fetch failure
func (h *Hub) loadHermesIndex(ctx context.Context) any {
	cached, err := readJSONIfFresh(h.hermesIndexCacheFile(), HermesIndexTTL)
	if err != nil {
		hubRaise(err)
	}
	if cached != nil {
		return cached
	}
	log := h.S.logger()
	var data any
	for _, enc := range []string{"gzip, deflate", "identity"} {
		resp, err := h.skillsHubHTTPGet(ctx, HermesIndexURL, hubGet{Timeout: 15 * time.Second, FollowRedirects: true,
			Headers: map[string]string{"Accept-Encoding": enc}})
		if err != nil {
			if pass := hubPassThrough(err); pass != nil {
				hubRaise(pass)
			}
			log.Debug(fmt.Sprintf("Hermes index fetch failed: %s", err))
			return h.loadStaleIndexCache()
		}
		if resp.StatusCode != 200 {
			log.Debug(fmt.Sprintf("Hermes index fetch returned %d", resp.StatusCode))
			return h.loadStaleIndexCache()
		}
		body, derr := pyhttpx.DecodeContent(resp.Header, resp.Content)
		if derr != nil {
			log.Debug(fmt.Sprintf("Hermes index decode failed (Accept-Encoding=%s): %s", enc, derr))
			continue
		}
		decoded := *resp
		decoded.Content = body
		v, jerr := decoded.JSON()
		if jerr != nil {
			if pass := hubPassThrough(jerr); pass != nil {
				hubRaise(pass)
			}
			log.Debug(fmt.Sprintf("Hermes index fetch failed: %s", jerr))
			return h.loadStaleIndexCache()
		}
		data = v
		break
	}
	obj, ok := data.(*pyjson.Object)
	if !ok {
		return h.loadStaleIndexCache()
	}
	if _, has := obj.Get("skills"); !has {
		return h.loadStaleIndexCache()
	}
	if text, err := pyjson.Dumps(obj); err == nil {
		file := h.hermesIndexCacheFile()
		if os.MkdirAll(filepath.Dir(file), 0o755) == nil {
			os.WriteFile(file, []byte(text), 0o644)
		}
	} else {
		hubRaise(err)
	}
	return obj
}

// Cached index regardless of age
func (h *Hub) loadStaleIndexCache() any {
	v, err := readJSONIfFresh(h.hermesIndexCacheFile(), math.Inf(1))
	if err != nil {
		hubRaise(err)
	}
	return v
}

// Every configured source adapter in priority order, a taps file failure raised to the caller boundary
func (h *Hub) createSourceRouter(auth *GitHubAuth) []SkillSource {
	if auth == nil {
		auth = NewGitHubAuth(h.S)
	}
	taps, err := h.Taps("").ListTaps()
	if err != nil {
		hubRaise(err)
	}
	extra := tapsFromList(taps)
	return []SkillSource{NewOptionalSkillSource(h, auth), NewHermesIndexSource(h, auth), NewSkillsShSource(h, auth),
		NewWellKnownSkillSource(h), NewUrlSource(h), NewGitHubSource(h, auth, extra), NewClawHubSource(h),
		NewLobeHubSource(h), NewBrowseShSource(h)}
}

// Sources able to narrow by tap provider before their own limit cut
type providerSearcher interface {
	SearchProvider(ctx context.Context, query string, limit int, providerFilter string) ([]SkillMeta, error)
}

// Results of one source, empty on any failure
func (h *Hub) searchOneSource(ctx context.Context, src SkillSource, query string, limit int, providerFilter string) (sid string, out []SkillMeta) {
	sid = src.SourceID()
	defer func() {
		if r := recover(); r != nil {
			h.S.logger().Debug(fmt.Sprintf("Search failed for %s: %v", sid, r))
			out = []SkillMeta{}
		}
	}()
	var err error
	ps, narrows := src.(providerSearcher)
	switch src.(type) {
	case *HermesIndexSource, *GitHubSource:
	default:
		narrows = false
	}
	if providerFilter != "" && narrows {
		out, err = ps.SearchProvider(ctx, query, limit, providerFilter)
	} else {
		out, err = src.Search(ctx, query, limit)
	}
	if err != nil {
		h.S.logger().Debug(fmt.Sprintf("Search failed for %s: %s", sid, err))
		return sid, []SkillMeta{}
	}
	return sid, out
}

// Sources to query for a source filter, API sources skipped while the index stands in for them
func selectActiveSources(sources []SkillSource, sourceFilter string) []SkillSource {
	effective := sourceFilter
	if providerFilterOf(sourceFilter) != "" {
		effective = "all"
	}
	indexAvailable := false
	if effective == "all" {
		for _, src := range sources {
			if a, ok := src.(interface{ IsAvailable() bool }); ok && src.SourceID() == "hermes-index" && a.IsAvailable() {
				indexAvailable = true
				break
			}
		}
	}
	var active []SkillSource
	for _, src := range sources {
		sid := src.SourceID()
		if effective != "all" && sid != effective && sid != "official" {
			continue
		}
		if indexAvailable && apiSourceIDs[sid] {
			continue
		}
		active = append(active, src)
	}
	return active
}

// Registries to ask after the index answered a non empty unfiltered query with nothing
func indexMissFallbackSources(sources, active []SkillSource, query string, counts *pyjson.Object, providerFilter string) []SkillSource {
	if pystr.Strip(query) == "" || providerFilter != "" || !slices.ContainsFunc(active, func(s SkillSource) bool { return s.SourceID() == "hermes-index" }) {
		return nil
	}
	if n, ok := counts.Get("hermes-index"); !ok || n != 0 {
		return nil
	}
	var out []SkillSource
	for _, src := range sources {
		if indexMissFallbackIDs[src.SourceID()] && !slices.Contains(active, src) {
			out = append(out, src)
		}
	}
	return out
}

type sourceResult struct {
	idx     int
	sid     string
	results []SkillMeta
}

// Queries the sources in parallel until the deadline, merging finished results in completion order
func (h *Hub) fanOut(ctx context.Context, active []SkillSource, query string, limits map[string]int, providerFilter string,
	deadline time.Time, onSourceDone func(string, int), all *[]SkillMeta, counts *pyjson.Object, timedOut *[]string) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		for _, src := range active {
			*timedOut = append(*timedOut, src.SourceID())
		}
		return
	}
	done := make(chan sourceResult, len(active))
	slots := make(chan struct{}, min(len(active), 8))
	cancelled := make(chan struct{})
	for i, src := range active {
		limit, ok := limits[src.SourceID()]
		if !ok {
			limit = 50
		}
		go func() {
			select {
			case slots <- struct{}{}:
			case <-cancelled:
				return
			}
			defer func() { <-slots }()
			select {
			case <-cancelled:
				return
			default:
			}
			sid, res := h.searchOneSource(ctx, src, query, limit, providerFilter)
			done <- sourceResult{i, sid, res}
		}()
	}
	finished := make([]bool, len(active))
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	defer close(cancelled)
	for range active {
		select {
		case r := <-done:
			finished[r.idx] = true
			res := r.results
			if providerFilter != "" {
				res = filterResultsByProvider(res, providerFilter)
			}
			counts.Set(r.sid, len(res))
			*all = append(*all, res...)
			if onSourceDone != nil {
				onSourceDone(r.sid, len(res))
			}
		case <-timer.C:
			var late []string
			for i, src := range active {
				if !finished[i] {
					late = append(late, src.SourceID())
				}
			}
			*timedOut = append(*timedOut, late...)
			if len(late) > 0 {
				h.S.logger().Debug(fmt.Sprintf("Skills browse timed out waiting for: %s", strings.Join(late, ", ")))
			}
			return
		}
	}
}

// Merged results, per source counts in completion order and the ids of sources that timed out
func (h *Hub) ParallelSearchSources(ctx context.Context, sources []SkillSource, query string, perSourceLimits map[string]int,
	sourceFilter string, overallTimeout time.Duration, onSourceDone func(string, int)) ([]SkillMeta, *pyjson.Object, []string) {
	all, counts, timedOut := []SkillMeta{}, pyjson.NewObject(), []string{}
	active := selectActiveSources(sources, sourceFilter)
	providerFilter := providerFilterOf(sourceFilter)
	if len(active) == 0 {
		return all, counts, timedOut
	}
	deadline := time.Now().Add(overallTimeout)
	h.fanOut(ctx, active, query, perSourceLimits, providerFilter, deadline, onSourceDone, &all, counts, &timedOut)
	if fallback := indexMissFallbackSources(sources, active, query, counts, providerFilter); len(fallback) > 0 {
		fd := time.Now().Add(indexMissFallbackBudget)
		if deadline.Before(fd) {
			fd = deadline
		}
		h.fanOut(ctx, fallback, query, perSourceLimits, providerFilter, fd, onSourceDone, &all, counts, &timedOut)
	}
	return all, counts, timedOut
}

// Parallel search deduped by trust and stably ranked by trust before the limit cut
func (h *Hub) UnifiedSearch(ctx context.Context, query string, sources []SkillSource, sourceFilter string, limit int) []SkillMeta {
	all, _, _ := h.ParallelSearchSources(ctx, sources, query, nil, sourceFilter, 30*time.Second, nil)
	deduped := dedupeByTrust(all)
	slices.SortStableFunc(deduped, func(a, b SkillMeta) int { return TrustRank[b.TrustLevel] - TrustRank[a.TrustLevel] })
	return pyHeadMetas(deduped, limit)
}
