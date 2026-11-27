package web

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/mcp"
	webconfig "github.com/nickheyer/icebath/internal/web/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

const allPaid = "All keyless web providers are pinned to paid tiers."

// Longest one call to a free plan's endpoint may take
const FreeTimeout = 30 * time.Second

// Calls a tool of a free plan's public MCP endpoint and returns the first text it answers with. Every failure, of the connection, the
// status or the tool, is a FreeError
func FreeMCP(ctx context.Context, endpoint, tool string, arguments map[string]any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, FreeTimeout)
	defer cancel()
	result, err := mcp.CallStateless(ctx, endpoint, tool, arguments)
	if status, ok := errors.AsType[*mcp.HTTPStatusError](err); ok {
		return "", FreeErrorf("HTTP %d: %s", status.Status, status.Body)
	}
	if err != nil {
		return "", FreeErrorf("%s", err.Error())
	}
	if result.IsError {
		var texts []string
		for _, block := range result.Content {
			if block != nil && block.Text != "" {
				texts = append(texts, block.Text)
			}
		}
		if len(texts) == 0 {
			return "", FreeErrorf("MCP tool call failed")
		}
		return "", FreeErrorf("%s", strings.Join(texts, " "))
	}
	if text := result.Text(); text != "" {
		return text, nil
	}
	return "", FreeErrorf("MCP response contained no text content")
}

// Whether free plans may answer when a backend has no key
func FreeOn(cfg *config.Config) bool {
	return cfg.GetBool(webconfig.FreeFallback)
}

// Whether a failed call to a backend is tried again on the free ring, never while free plans are off
func rescueOn(cfg *config.Config) bool {
	return cfg.GetBool(webconfig.FreeRescue) && FreeOn(cfg)
}

// Plan pinned for the backend in web.provider_tier: free, paid, or auto
func Tier(cfg *config.Config, name string) string {
	tiers, _ := cfg.Get(webconfig.ProviderTier).(map[string]any)
	switch tier, _ := tiers[name].(string); normal(tier) {
	case webconfig.Free:
		return webconfig.Free
	case webconfig.Paid:
		return webconfig.Paid
	}
	return webconfig.Auto
}

// Whether a call to the backend goes to its free plan: pinned free, or with no key while free plans are on, never when pinned paid
func Keyless(cfg *config.Config, name, key string) bool {
	switch Tier(cfg, name) {
	case webconfig.Free:
		return true
	case webconfig.Paid:
		return false
	}
	return strings.TrimSpace(key) == "" && FreeOn(cfg)
}

// Failure of a call to a free plan that the vendor or its endpoint reported
type FreeError struct {
	Message string
}

func (e *FreeError) Error() string { return e.Message }

func FreeErrorf(format string, args ...any) error {
	return &FreeError{Message: fmt.Sprintf(format, args...)}
}

func IsFreeError(err error) bool {
	_, ok := errors.AsType[*FreeError](err)
	return ok
}

// Message of a failed free plan call naming the key that would make the service reliable, and other backends when there are
func FreeFailure(title, kind string, err error, key, site string, others bool) string {
	alternative := ""
	if others {
		alternative = " or pick another web backend with /web"
	}
	return fmt.Sprintf("Keyless %s %s failed: %v. Set %s (%s)%s for reliable service.", title, kind, err, key, site, alternative)
}

var throttleMarkers = []string{"rate limit", "rate-limit", "ratelimit", "too many requests", "429", "quota exceeded", "slow down"}

// Whether a failure reads like a free plan's throttling
func throttled(message string) bool {
	lower := strings.ToLower(message)
	return slices.ContainsFunc(throttleMarkers, func(marker string) bool { return strings.Contains(lower, marker) })
}

var refused = regexp.MustCompile(`(?i)^\s*(?:keyless\s+\w+\s+search\s+failed:\s*)?(?:http(?:\s+status)?|status(?:\s+code)?|client\s+error|error\s+code)\s*[:=']*\s*(?:40[123]|5\d\d)\b`)

// Whether another free backend may answer a search this one failed
func failover(message string) bool {
	return throttled(message) || refused.MatchString(message)
}

// Free ring in the order a call starting at the backend walks it, without those pinned paid. A backend chosen or pinned free starts it
// where it is, the start of the ring when it is not on it; any other backend on the ring starts it where it is too, so the one picked
// answers first; anything else enters it at a random place
func (r *Registry) order(cfg *config.Config, name string) []Backend {
	ring := r.ring(0)
	at := slices.IndexFunc(ring, func(backend Backend) bool { return backend.Name == name })
	start := max(at, 0)
	if at < 0 && Tier(cfg, name) != webconfig.Free && !Picked(cfg, name) && len(ring) > 0 {
		start = -1
	}
	return slices.DeleteFunc(r.ring(start), func(backend Backend) bool { return Tier(cfg, backend.Name) == webconfig.Paid })
}

// Search on the free ring from the backend named, going on to the next while one is throttled or refuses. An error a backend returns
// stops the walk
func (r *Registry) FreeSearch(ctx context.Context, cfg *config.Config, name, query string, limit int) (*Results, error) {
	order := r.order(cfg, name)
	if len(order) == 0 {
		return Failed(allPaid), nil
	}
	var results *Results
	for _, backend := range order {
		var err error
		if results, err = backend.Free.Search(ctx, query, limit); err != nil {
			return nil, err
		}
		if results.Success || !failover(results.Error) {
			if results.Success && backend.Name != name {
				results.data().ServedBy = backend.Name
			}
			return results, nil
		}
	}
	names := make([]string, len(order))
	for i, backend := range order {
		names[i] = backend.Name
	}
	results.Error = fmt.Sprintf("%s (all keyless vendors unavailable: %s)", results.Error, strings.Join(names, ", "))
	return results, nil
}

// Pages read on the free ring from the backend named, going on to the next only while every page came back throttled. An error a
// backend returns stops the walk
func (r *Registry) FreeExtract(ctx context.Context, cfg *config.Config, name string, urls []string) ([]*Page, error) {
	order := r.order(cfg, name)
	if len(order) == 0 {
		return Unreadable(urls, allPaid), nil
	}
	var pages []*Page
	for _, backend := range order {
		var err error
		if pages, err = backend.Free.Extract(ctx, slices.Clone(urls)); err != nil {
			return nil, err
		}
		if len(pages) == 0 || slices.ContainsFunc(pages, func(page *Page) bool { return page.Error == "" || Origin(page.Error) || !throttled(page.Error) }) {
			break
		}
	}
	return pages, nil
}

// Whether a failed call to the backend is tried once more on the free ring: rescue is on and the call did not go to the ring already
func (r *Registry) Rescues(cfg *config.Config, backend Backend) bool {
	return rescueOn(cfg) && (backend.Free == nil || backend.Free.Used == nil || !backend.Free.Used(cfg))
}

// Search the free ring answers in place of the backend that failed it, saying so, else the failure of both
func (r *Registry) RescueSearch(ctx context.Context, cfg *config.Config, name, failure, query string, limit int) (*Results, error) {
	rescued, err := r.FreeSearch(ctx, cfg, name, query, limit)
	if err != nil {
		return nil, err
	}
	if rescued.Success {
		data := rescued.data()
		data.RescuedFrom = name
		data.BackendError = fmt.Sprintf("Configured backend '%s' failed this call (%s); result served by the keyless free tier. The next call will use '%s' again.",
			name, strs.Head(cmp.Or(failure, "unknown error"), 300), name)
		return rescued, nil
	}
	return Failed(fmt.Sprintf("%s (keyless rescue also failed: %s)", cmp.Or(failure, "search failed"), cmp.Or(rescued.Error, "unknown"))), nil
}

// Pages the free ring reads in place of a backend that failed every one. Pages the website blocklist refused stay as they are, and when
// the ring fails them all as well the backend's failures stand
func (r *Registry) RescueExtract(ctx context.Context, cfg *config.Config, name string, urls []string, pages []*Page) ([]*Page, error) {
	aligned := len(pages) == len(urls)
	var again []int
	for i, page := range pages {
		if !aligned || !refusedByPolicy(page) {
			again = append(again, i)
		}
	}
	if len(again) == 0 {
		return pages, nil
	}
	retry := urls
	if aligned {
		retry = make([]string, len(again))
		for i, at := range again {
			retry[i] = urls[at]
		}
	}
	rescued, err := r.FreeExtract(ctx, cfg, name, retry)
	if err != nil {
		return nil, err
	}
	if len(rescued) > 0 && !slices.ContainsFunc(rescued, func(page *Page) bool { return page.Error == "" }) {
		return pages, nil
	}
	if aligned && len(rescued) == len(again) {
		out := slices.Clone(pages)
		for i, at := range again {
			out[at] = rescued[i]
		}
		return out, nil
	}
	return rescued, nil
}

// Whether the website blocklist refused the page, which no other backend is asked to read
func refusedByPolicy(page *Page) bool {
	return page.Blocked != nil || strings.Contains(strings.ToLower(page.Error), "blocked by website policy")
}
