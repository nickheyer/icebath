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
	return "\nSimilar models: " + strings.Join(quoted, ", ")
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
		return reject("Name the model to use.")
	case IsKnownNonChatModel(model):
		return reject("`" + model + "` makes images or other media and can't chat. Pick a chat model, and set image models in /settings under Images.")
	case strings.ContainsFunc(model, unicode.IsSpace) && !spaced(route, spec):
		return reject("A model name can't hold spaces.")
	}
	label := cmp.Or(spec.DisplayName, spec.Name, route.Provider)
	catalog := m.Catalog(ctx, cfg, route, false)
	if len(catalog.IDs) == 0 {
		return accept(fmt.Sprintf("icebath could not reach %s to check `%s`. If %s is not down, the model may not exist.", label, model, label))
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
		return accept(fmt.Sprintf("`%s` is not in icebath's list of %s models, and %s could not be asked. It still works if %s has it.", model,
			label, label, label) + similar)
	case route.Spec == "custom":
		return accept(fmt.Sprintf("`%s` is not in the list of models at %s. It still works if the server has hidden models or other names for "+
			"them.", model, cmp.Or(spec.ModelsURL, strings.TrimRight(route.BaseURL, "/")+"/models")) + similar)
	case route.Spec == "anthropic":
		return accept(fmt.Sprintf("`%s` is not in Anthropic's list of models. It still works if your account has early access or the model is a "+
			"dated version.", model) + similar)
	case route.Mode == provider.ModeAnthropicMessages:
		return accept(fmt.Sprintf("`%s` is not in this provider's list of models, so it was taken without a check. The provider may know it by "+
			"another name.", model) + similar)
	case route.Local || spec.Local:
		return accept(fmt.Sprintf("`%s` is not in the list of models on this computer's server. It still works if the server has hidden models "+
			"or other names for them.", model) + similar)
	}
	return reject(fmt.Sprintf("This provider has no model `%s`. Type /model to see the ones it has.%s", model, similar))
}
