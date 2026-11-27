package models

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/tools/diff"
)

// What checking the model a switch names found: whether the switch goes ahead, and what the user is told of the model
type Verdict struct {
	Accepted bool
	Message  string
}

func accept(message string) Verdict { return Verdict{Accepted: true, Message: message} }

func reject(message string) Verdict { return Verdict{Message: message} }

// Close matches of a model a catalog lacks, as the line that suggests them; empty when none is close
func suggestions(model string, ids []string, cutoff float64) string {
	byLower := map[string]string{}
	var pool []string
	for _, id := range ids {
		lower := strings.ToLower(id)
		if _, seen := byLower[lower]; !seen {
			pool = append(pool, lower)
		}
		byLower[lower] = id
	}
	close := diff.Close(strings.ToLower(model), pool, 3, cutoff)
	if len(close) == 0 {
		return ""
	}
	quoted := make([]string, len(close))
	for i, id := range close {
		quoted[i] = "`" + byLower[id] + "`"
	}
	return "\n  Similar models: " + strings.Join(quoted, ", ")
}

// Loopback, private network and single label hosts, which name an endpoint of the user's rather than a vendor's
func private(host string) bool {
	host = strings.TrimRight(strings.ToLower(host), ".")
	switch {
	case host == "":
		return false
	case host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.Contains(host, ".") && net.ParseIP(host) == nil:
		return true
	}
	for _, suffix := range []string{".local", ".lan", ".internal", ".home", ".localdomain"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified())
}

func hostOf(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

// Whether a model id with spaces may be a real one on the route: a local server, a custom endpoint, or a base URL of the user's own
// rather than the kind's
func spaced(route provider.Route, spec provider.Spec) bool {
	if route.Local || spec.Local || route.Spec == "custom" {
		return true
	}
	host := hostOf(route.BaseURL)
	if host == "" {
		return false
	}
	stock := hostOf(spec.BaseURL)
	return private(host) || stock != "" && host != stock
}

// Checks the model a switch to the route names, its id never rewritten: refused when it is empty, a generation model, holds spaces no
// vendor catalog does, or the provider's live listing lacks it; taken with a note when only a curated list or no list could be read, or
// the endpoint's listing may leave models out; up to three close matches suggested for one not listed
func (m *Models) Validate(ctx context.Context, cfg *config.Config, route provider.Route) Verdict {
	model := strings.TrimSpace(route.Model)
	spec := m.Spec(route)
	switch {
	case model == "":
		return reject("Model name cannot be empty.")
	case IsKnownNonChatModel(model):
		return reject("`" + model + "` is a generation model and cannot be used for chat. Pick a chat model, or use image generation for image models.")
	case strings.ContainsFunc(model, unicode.IsSpace) && !spaced(route, spec):
		return reject("Model names cannot contain spaces.")
	}
	label := cmp.Or(spec.DisplayName, spec.Name, route.Provider)
	catalog := m.Catalog(ctx, cfg, route, false)
	if len(catalog.IDs) == 0 {
		return accept(fmt.Sprintf("Note: could not reach the %s API to validate `%s`. If the service isn't down, this model may not be valid.", label, model))
	}
	listed := func(id string) bool {
		return slices.ContainsFunc(catalog.IDs, func(have string) bool { return strings.EqualFold(have, id) })
	}
	if listed(model) || routed(route, spec) && variantBase(model) != "" && listed(variantBase(model)) {
		return accept("")
	}
	cutoff := 0.5
	if route.Mode == provider.ModeAnthropicMessages || spec.Mode == provider.ModeAnthropicMessages {
		cutoff = 0.4
	}
	similar := suggestions(model, catalog.IDs, cutoff)
	switch {
	case catalog.Fallback:
		return accept(fmt.Sprintf("Note: `%s` was not found in the %s curated catalog and the /models endpoint was unreachable.", model, label) + similar +
			"\n  The model may still work if it exists on the provider.")
	case route.Spec == "custom":
		return accept(fmt.Sprintf("Note: `%s` was not found in this custom endpoint's model listing (%s). It may still work if the server supports "+
			"hidden or aliased models.", model, cmp.Or(spec.ModelsURL, strings.TrimRight(route.BaseURL, "/")+"/models")) + similar)
	case route.Spec == "anthropic":
		return accept(fmt.Sprintf("Note: `%s` was not found in Anthropic's /v1/models listing. It may still work if you have early-access or snapshot "+
			"IDs.", model) + similar)
	case route.Mode == provider.ModeAnthropicMessages:
		return accept(fmt.Sprintf("Note: `%s` is not named in this endpoint's model listing (it may still serve it under an alias).", model) + similar +
			"\n  The model name has been accepted without verification.")
	case route.Local || spec.Local:
		return accept(fmt.Sprintf("Note: `%s` was not found in this local server's model listing. It may still work if the server supports hidden "+
			"or aliased models.", model) + similar)
	}
	return reject(fmt.Sprintf("Model `%s` was not found in this provider's model listing.%s", model, similar))
}
