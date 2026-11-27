package web

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/text/unicode/norm"

	"github.com/nickheyer/icebath/internal/config"
	ddgsconfig "github.com/nickheyer/icebath/internal/plugins/defaults/webbackends/ddgs/config"
	"github.com/nickheyer/icebath/internal/web"
)

// One search as every engine is asked it
type query struct {
	text, region, safesearch, timelimit string
}

// Country and language of a region such as us-en
func split(region string) (string, string) {
	country, language, _ := strings.Cut(region, "-")
	return country, language
}

// Search engine ddgs asks, and the provider whose index it serves: one answer per provider is enough
type engine struct {
	provider string
	search   func(ctx context.Context, b *browser, q query) ([]result, error)
}

// One hit as an engine gives it
type result struct {
	title, href, body string
}

// Result with its fields normalized the way ddgs keeps them
func hit(title, href, body string) result {
	return result{title: normalText(title), href: normalURL(href), body: normalText(body)}
}

var tags = regexp.MustCompile(`<.*?>`)

// Text with its tags dropped, its entities unescaped, control, format, private and unassigned characters left out, its whitespace
// collapsed
func normalText(text string) string {
	text = norm.NFC.String(html.UnescapeString(tags.ReplaceAllString(text, "")))
	text = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z) {
			return r
		}
		return -1
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

// Address percent-decoded, its spaces as plus signs
func normalURL(address string) string {
	return strings.ReplaceAll(unquote(address), " ", "+")
}

// Percent escapes decoded, one that is malformed kept as written: Python's unquote
func unquote(text string) string {
	if !strings.Contains(text, "%") {
		return text
	}
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '%' && i+2 < len(text) {
			if decoded, err := hex.DecodeString(text[i+1 : i+3]); err == nil {
				b.Write(decoded)
				i += 2
				continue
			}
		}
		b.WriteByte(text[i])
	}
	return strings.ToValidUTF8(b.String(), "\uFFFD")
}

// Hits of the search: the engines asked, their results merged and ranked, cut to the limit
func lookup(ctx context.Context, cfg *config.Config, text string, limit int) ([]web.Hit, error) {
	if text == "" {
		return nil, errors.New("query is mandatory.")
	}
	q, proxy, err := read(cfg, text)
	if err != nil {
		return nil, err
	}
	connections := transport(proxy)
	defer connections.CloseIdleConnections()
	results, err := gather(ctx, connections, q, limit)
	if err != nil {
		return nil, err
	}
	hits := make([]web.Hit, len(results))
	for i, r := range results {
		hits[i] = web.Hit{Title: r.title, URL: r.href, Description: r.body, Position: i + 1}
	}
	return hits, nil
}

// Search the settings ask for and the proxy it goes through, or which setting is wrong
func read(cfg *config.Config, text string) (query, *url.URL, error) {
	q := query{
		text:       text,
		region:     cfg.GetString(ddgsconfig.Region),
		safesearch: strings.ToLower(cfg.GetString(ddgsconfig.SafeSearch)),
		timelimit:  cfg.GetString(ddgsconfig.TimeLimit),
	}
	switch {
	case strings.Count(q.region, "-") != 1:
		return query{}, nil, fmt.Errorf("%s is %q, not a country and a language such as us-en.", ddgsconfig.Region, q.region)
	case !slices.Contains([]string{"on", "moderate", "off"}, q.safesearch):
		return query{}, nil, fmt.Errorf("%s is %q, not on, moderate or off.", ddgsconfig.SafeSearch, q.safesearch)
	case !slices.Contains([]string{"", "d", "w", "m", "y"}, q.timelimit):
		return query{}, nil, fmt.Errorf("%s is %q, not d, w, m or y.", ddgsconfig.TimeLimit, q.timelimit)
	}
	proxy, err := proxyURL(cfg.GetString(ddgsconfig.Proxy))
	return q, proxy, err
}

// Proxy the setting names, none when it is empty; tb is the Tor Browser's
func proxyURL(setting string) (*url.URL, error) {
	setting = strings.TrimSpace(setting)
	switch setting {
	case "":
		return nil, nil
	case "tb":
		setting = "socks5h://127.0.0.1:9150"
	}
	address, err := url.Parse(setting)
	if err != nil || address.Scheme == "" || address.Host == "" {
		return nil, fmt.Errorf("%s is %q, not a proxy address such as socks5h://127.0.0.1:9150.", ddgsconfig.Proxy, setting)
	}
	return address, nil
}

// Connections of one search, through the proxy when there is one
func transport(proxy *url.URL) *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{}
	}
	connections := base.Clone()
	if proxy != nil {
		connections.Proxy = http.ProxyURL(proxy)
	}
	return connections
}

// One engine's search under way
type run struct {
	engine  engine
	over    chan struct{}
	results []result
	err     error
}

func (r *run) done() bool {
	select {
	case <-r.over:
		return true
	default:
		return false
	}
}

// Results of the engines asked a few at a time, one skipped once another of its provider answered, until enough pages are in; the
// engines still running then are stopped and waited for. The error of the last engine that failed when none answered
func gather(ctx context.Context, connections http.RoundTripper, q query, limit int) ([]result, error) {
	ctx, stop := context.WithCancel(ctx)
	var started []*run
	defer func() {
		stop()
		for _, r := range started {
			<-r.over
		}
	}()
	engines := order()
	providers := map[string]bool{}
	for _, e := range engines {
		providers[e.provider] = true
	}
	workers := min(len(providers), (limit+9)/10+1)
	finished := make(chan struct{}, len(engines))
	answered := map[string]bool{}
	pages := merge{best: map[string]result{}, counts: map[string]int{}}
	var pending []*run
	var failure error
	for i, e := range engines {
		if answered[e.provider] {
			continue
		}
		r := &run{engine: e, over: make(chan struct{})}
		go func() {
			r.results, r.err = e.search(ctx, newBrowser(connections), q)
			close(r.over)
			finished <- struct{}{}
		}()
		started = append(started, r)
		pending = append(pending, r)
		if len(pending) >= workers || i+1 >= workers {
			if err := wait(ctx, pending, finished); err != nil {
				return nil, err
			}
			var running []*run
			for _, p := range pending {
				switch {
				case !p.done():
					running = append(running, p)
				case p.err != nil:
					failure = p.err
				case len(p.results) > 0:
					pages.add(p.results)
					answered[p.engine.provider] = true
				}
			}
			pending = running
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(pages.keys) >= limit {
			break
		}
	}
	ranked := rank(pages.results(), q.text)
	switch {
	case len(ranked) > 0:
		return ranked[:min(limit, len(ranked))], nil
	case failure != nil:
		return nil, failure
	}
	return nil, errors.New("No results found.")
}

// Waits until every run is over or one failed, at most the patience of a request: concurrent.futures' wait for the first exception.
// The error of the search when it ends first
func wait(ctx context.Context, runs []*run, finished <-chan struct{}) error {
	timer := time.NewTimer(patience)
	defer timer.Stop()
	for {
		over := true
		for _, r := range runs {
			switch {
			case !r.done():
				over = false
			case r.err != nil:
				return nil
			}
		}
		if over {
			return nil
		}
		select {
		case <-finished:
		case <-timer.C:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Results of several engines, each page once with the longest body given for it, counting the engines that gave it
type merge struct {
	keys   []string
	best   map[string]result
	counts map[string]int
}

func (m *merge) add(results []result) {
	for _, r := range results {
		if kept, ok := m.best[r.href]; !ok || utf8.RuneCountInString(r.body) > utf8.RuneCountInString(kept.body) {
			m.best[r.href] = r
		}
		if m.counts[r.href] == 0 {
			m.keys = append(m.keys, r.href)
		}
		m.counts[r.href]++
	}
}

// Pages given by the most engines first, the first given first among equals
func (m *merge) results() []result {
	keys := slices.Clone(m.keys)
	slices.SortStableFunc(keys, func(a, b string) int { return m.counts[b] - m.counts[a] })
	out := make([]result, len(keys))
	for i, key := range keys {
		out[i] = m.best[key]
	}
	return out
}

// Wikipedia first, then the pages matching a word of the query in title and body, in the title, in the body, and the rest; Wikimedia
// category pages left out. Words are three letters or more
func rank(results []result, text string) []result {
	var words []string
	for _, word := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' }) {
		if utf8.RuneCountInString(word) >= 3 {
			words = append(words, word)
		}
	}
	matches := func(text string) bool {
		text = strings.ToLower(text)
		return slices.ContainsFunc(words, func(word string) bool { return strings.Contains(text, word) })
	}
	var groups [5][]result
	for _, r := range results {
		if strings.Contains(r.title, "Category:") && strings.Contains(r.title, "Wikimedia") {
			continue
		}
		title, body := matches(r.title), matches(r.body)
		group := 4
		switch {
		case strings.Contains(r.href, "wikipedia.org"):
			group = 0
		case title && body:
			group = 1
		case title:
			group = 2
		case body:
			group = 3
		}
		groups[group] = append(groups[group], r)
	}
	return slices.Concat(groups[:]...)
}
