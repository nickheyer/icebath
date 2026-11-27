package skills

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// One skill a registry offers, as a search finds it
type Meta struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Registry it comes from, such as official or clawhub
	Source     string `json:"source"`
	Identifier string `json:"identifier"`
	// builtin, trusted or community
	Trust string `json:"trust_level"`
	// GitHub repository and path of the skill in it, when it has one
	Repo string   `json:"repo,omitempty"`
	Path string   `json:"path,omitempty"`
	Tags []string `json:"tags"`
	// What the registry says of it besides: its provider, category, owner, installs, pages and audits
	Extra map[string]any `json:"extra,omitempty"`
}

// Text of an extra field, empty when it has none
func (m Meta) extra(key string) string {
	if v, ok := m.Extra[key]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

// Source as the user reads it: a GitHub tap's provider in place of github
func (m Meta) Label() string {
	if provider := m.extra("provider"); m.Source == "github" && provider != "" {
		return provider
	}
	return m.Source
}

// Category the registry files the skill under, empty for none
func (m Meta) Category() string {
	return m.extra("category")
}

// One file of a skill a registry delivered, by its path in the skill's folder
type File struct {
	Path string
	Data []byte
}

// A skill a registry delivered, waiting for quarantine, the scan and its install
type Bundle struct {
	Name       string
	Files      []File
	Source     string
	Identifier string
	Trust      string
	Metadata   map[string]any
}

// Content of a file of the bundle by its exact path
func (b *Bundle) file(path string) ([]byte, bool) {
	for _, f := range b.Files {
		if f.Path == path {
			return f.Data, true
		}
	}
	return nil, false
}

// Sets a file of the bundle in place when its path is there, else adds it
func (b *Bundle) set(path string, data []byte) {
	for i := range b.Files {
		if b.Files[i].Path == path {
			b.Files[i].Data = data
			return
		}
	}
	b.Files = append(b.Files, File{Path: path, Data: data})
}

// A registry skills are found in and fetched from
type Source interface {
	ID() string
	// Skills matching the query, at most limit; a source that can narrow by a GitHub tap's provider does so before the limit
	Search(ctx context.Context, query string, limit int, provider string) []Meta
	// The skill's files, nil when the source does not have it or could not deliver it
	Fetch(ctx context.Context, identifier string) *Bundle
	// What the source says of the skill, nil when it does not have it
	Inspect(ctx context.Context, identifier string) *Meta
	// Revision the skill's upstream is at now, empty when the source can't tell without fetching it
	Revision(ctx context.Context, identifier string) string
}

// Order of trust levels, the most trusted highest
func rank(trust string) int {
	switch trust {
	case "builtin":
		return 3
	case "trusted":
		return 2
	case "community":
		return 1
	}
	return 0
}

// Whether the query, lower case, is in the fields joined with spaces, lists joined with spaces too
func matches(query string, fields ...any) bool {
	parts := make([]string, len(fields))
	for i, field := range fields {
		switch v := field.(type) {
		case []string:
			parts[i] = strings.Join(v, " ")
		default:
			parts[i] = fmt.Sprint(v)
		}
	}
	return strings.Contains(strings.ToLower(strings.Join(parts, " ")), query)
}

// First n of a list
func head[T any](list []T, n int) []T {
	return list[:max(0, min(n, len(list)))]
}

// Results without repeated identifiers, the more trusted copy kept at the place the first was seen
func dedupe(results []Meta) []Meta {
	at := map[string]int{}
	var out []Meta
	for _, m := range results {
		i, seen := at[m.Identifier]
		switch {
		case !seen:
			at[m.Identifier] = len(out)
			out = append(out, m)
		case rank(m.Trust) > rank(out[i].Trust):
			out[i] = m
		}
	}
	return out
}

// Every source of the hub in the order it is asked: the official skills, the index, the registries, a URL, GitHub with the user's taps
func (r *run) sources() ([]Source, error) {
	taps, err := r.taps()
	if err != nil {
		return nil, err
	}
	return []Source{
		&official{r: r}, &indexSource{r: r}, &skillsSh{r: r}, &wellKnown{r: r}, &urlSource{r: r},
		&githubSource{r: r, taps: append(slices.Clone(defaultTaps), taps...)}, &clawhub{r: r}, &lobehub{r: r}, &browseSh{r: r},
	}, nil
}

// Sources the centralized index stands in for while it answers a search of every source
var indexed = map[string]bool{"github": true, "skills-sh": true, "clawhub": true, "lobehub": true, "well-known": true}

// Registries asked when the index found nothing for a query, GitHub left out to spare its anonymous rate limit
var fallbacks = map[string]bool{"skills-sh": true, "clawhub": true, "lobehub": true, "well-known": true}

// How long the registries asked after an index miss get
const fallbackBudget = 8 * time.Second

// Provider a source filter names, lower case, empty when it names a source
func providerOf(filter string) string {
	value := strings.ToLower(strings.TrimSpace(filter))
	for _, provider := range tapProviders {
		if strings.ToLower(provider) == value {
			return value
		}
	}
	return ""
}

// Sources a filter asks: one source and the official one, or all of them with the index in place of the registries it covers
func (r *run) active(ctx context.Context, sources []Source, filter string) []Source {
	effective := filter
	if providerOf(filter) != "" {
		effective = "all"
	}
	stands := false
	if effective == "all" {
		for _, src := range sources {
			if idx, ok := src.(*indexSource); ok && idx.available(ctx) {
				stands = true
			}
		}
	}
	var out []Source
	for _, src := range sources {
		id := src.ID()
		if effective != "all" && id != effective && id != "official" || stands && indexed[id] {
			continue
		}
		out = append(out, src)
	}
	return out
}

// How many skills a source found
type Count struct {
	Source string
	N      int
}

// What a search of several sources found: the results in the order the sources answered, how many each found, and those that did not
// answer in time
type found struct {
	results  []Meta
	counts   []Count
	timedOut []string
}

// Asks the sources at once until the deadline, results merged as each answers; a source still working when time runs out keeps going
// for a while, so what it finds is cached for the next search
func (r *run) fanOut(ctx context.Context, sources []Source, query string, limits map[string]int, provider string, deadline time.Time,
	done func(source string, n int), into *found) {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		for _, src := range sources {
			into.timedOut = append(into.timedOut, src.ID())
		}
		return
	}
	type answer struct {
		i       int
		results []Meta
	}
	answers := make(chan answer, len(sources))
	slots := make(chan struct{}, min(len(sources), 8))
	background, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	var wg sync.WaitGroup
	for i, src := range sources {
		limit, ok := limits[src.ID()]
		if !ok {
			limit = 50
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			answers <- answer{i, src.Search(background, query, limit, provider)}
		}()
	}
	go func() {
		wg.Wait()
		cancel()
	}()
	finished := make([]bool, len(sources))
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	for range sources {
		select {
		case a := <-answers:
			finished[a.i] = true
			results := a.results
			if provider != "" {
				results = slices.DeleteFunc(results, func(m Meta) bool { return strings.ToLower(m.extra("provider")) != provider })
			}
			id := sources[a.i].ID()
			into.counts = append(into.counts, Count{id, len(results)})
			into.results = append(into.results, results...)
			if done != nil {
				done(id, len(results))
			}
		case <-timer.C:
			var late []string
			for i, src := range sources {
				if !finished[i] {
					late = append(late, src.ID())
				}
			}
			slog.Debug("Skills browse timed out waiting for: " + strings.Join(late, ", "))
			into.timedOut = append(into.timedOut, late...)
			return
		}
	}
}

// Asks the sources a filter selects at once, then the registries the index covers when it found nothing for a query
func (r *run) parallel(ctx context.Context, sources []Source, query string, limits map[string]int, filter string, timeout time.Duration,
	done func(source string, n int)) found {
	var out found
	active := r.active(ctx, sources, filter)
	if len(active) == 0 {
		return out
	}
	provider := providerOf(filter)
	deadline := time.Now().Add(timeout)
	r.fanOut(ctx, active, query, limits, provider, deadline, done, &out)
	if strings.TrimSpace(query) == "" || provider != "" || !slices.ContainsFunc(active, func(s Source) bool { return s.ID() == "skills-index" }) {
		return out
	}
	if !slices.ContainsFunc(out.counts, func(c Count) bool { return c.Source == "skills-index" && c.N == 0 }) {
		return out
	}
	var more []Source
	for _, src := range sources {
		if fallbacks[src.ID()] && !slices.Contains(active, src) {
			more = append(more, src)
		}
	}
	if len(more) > 0 {
		r.fanOut(ctx, more, query, limits, provider, minTime(deadline, time.Now().Add(fallbackBudget)), done, &out)
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Search of the sources a filter selects, without repeats, the most trusted first, at most limit
func (r *run) unified(ctx context.Context, sources []Source, query, filter string, limit int) []Meta {
	results := dedupe(r.parallel(ctx, sources, query, nil, filter, 30*time.Second, nil).results)
	slices.SortStableFunc(results, func(a, b Meta) int { return rank(b.Trust) - rank(a.Trust) })
	return head(results, limit)
}

// Skills asked of each source when browsing from the command line, and from a screen
var (
	browseLimits = map[string]int{"skills-index": 1_000_000, "official": 200, "skills-sh": 200, "well-known": 50, "github": 200, "clawhub": 500,
		"lobehub": 500, "browse-sh": 500}
	screenLimits = map[string]int{"skills-index": 5000, "official": 100, "skills-sh": 100, "well-known": 25, "github": 100, "clawhub": 50,
		"lobehub": 50, "browse-sh": 500}
)

// One page of browsing
type Page struct {
	Skills []Meta
	// Page shown, from 1, of how many
	Page, Pages int
	// Skills of every page, and the place of the page's first among them
	Total, Start int
	// How many official skills there are
	Official int
	// How many skills each source found, in the order they answered
	Counts []Count
	// Sources that did not answer in time
	TimedOut []string
}

// One page of every skill the sources a filter selects offer: no repeats, the most trusted first, official ones before others of their
// trust, then by name
func (r *run) browse(ctx context.Context, page, size int, filter string, screen bool, done func(source string, n int)) (Page, error) {
	size = max(1, min(size, 100))
	sources, err := r.sources()
	if err != nil {
		return Page{}, err
	}
	limits := browseLimits
	if screen {
		limits = screenLimits
	}
	got := r.parallel(ctx, sources, "", limits, filter, 30*time.Second, done)
	all := dedupe(got.results)
	slices.SortStableFunc(all, func(a, b Meta) int {
		if d := rank(b.Trust) - rank(a.Trust); d != 0 {
			return d
		}
		if (a.Source == "official") != (b.Source == "official") {
			if a.Source == "official" {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	pages := max(1, (len(all)+size-1)/size)
	page = max(1, min(page, pages))
	start := (page - 1) * size
	out := Page{Page: page, Pages: pages, Total: len(all), Start: start, Counts: got.counts, TimedOut: got.timedOut}
	if start < len(all) {
		out.Skills = all[start:min(len(all), start+size)]
	}
	for _, m := range all {
		if m.Source == "official" {
			out.Official++
		}
	}
	return out, nil
}
