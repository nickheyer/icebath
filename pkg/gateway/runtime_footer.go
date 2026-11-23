package gateway

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/settings"
)

const footerSep = " · "

// Cwd with the home directory collapsed to a tilde, empty when unset
func homeRelativeCwd(cwd string) string {
	if cwd == "" {
		return ""
	}
	home := pypath.Expanduser("~")
	p, err := pypath.Abspath(cwd)
	if err != nil {
		return cwd
	}
	if home != "" && (p == home || strings.HasPrefix(p, home+string(os.PathSeparator))) {
		return "~" + p[len(home):]
	}
	return p
}

// Model name with its vendor prefix dropped
func modelShort(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// Scope aware TERMINAL_CWD the footer falls back to, empty when unset
func envCwd(ctx context.Context) (string, error) {
	return tenv(ctx, "TERMINAL_CWD", "")
}

// Whether the footer is on for a platform and the fields it shows, the platform's own choice winning
func ResolveFooterConfig(ctx context.Context, platformKey string) (bool, []string) {
	pctx := settings.WithPlatform(ctx, platformKey)
	return settings.ReplyFooter.Get(pctx), settings.DisplayRuntimeFooterFields.Strings(pctx)
}

// Turn duration as under a second, whole seconds, or minutes and padded seconds
func formatLatency(seconds float64) string {
	if seconds < 1 {
		return "<1s"
	}
	total := int(math.RoundToEven(seconds))
	if total < 60 {
		return fmt.Sprintf("%ds", total)
	}
	return fmt.Sprintf("%dm%02ds", total/60, total%60)
}

// Inputs of one footer render, a zero context length or nil turn seconds standing for unmeasured
type FooterInputs struct {
	Model          string
	ContextTokens  int
	ContextLength  int
	Cwd            string
	TurnSeconds    *float64
	RequestedModel string
	ServedModel    string
}

// Footer line of the fields that have data, joined by the separator
func FormatRuntimeFooter(ctx context.Context, in FooterInputs, fields []string) (string, error) {
	var parts []string
	for _, field := range fields {
		var value string
		switch field {
		case "model":
			value = modelShort(in.Model)
		case "served_model":
			requested := in.RequestedModel
			if requested == "" {
				requested = in.Model
			}
			alias := modelShort(requested)
			if in.ServedModel != "" && in.ServedModel != alias && in.ServedModel != requested {
				value = alias + " → " + in.ServedModel
			}
		case "context_pct":
			if in.ContextLength > 0 && in.ContextTokens >= 0 {
				pct := int(math.RoundToEven(float64(in.ContextTokens) / float64(in.ContextLength) * 100))
				value = fmt.Sprintf("%d%%", max(0, min(100, pct)))
			}
		case "latency":
			if in.TurnSeconds != nil && *in.TurnSeconds >= 0 {
				value = formatLatency(*in.TurnSeconds)
			}
		case "cwd":
			cwd := in.Cwd
			if cwd == "" {
				fromEnv, err := envCwd(ctx)
				if err != nil {
					return "", err
				}
				cwd = fromEnv
			}
			value = homeRelativeCwd(cwd)
		}
		if value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, footerSep), nil
}

// Footer text for the final response, empty when the footer is off or no field has data
func BuildFooterLine(ctx context.Context, platformKey string, in FooterInputs) (string, error) {
	enabled, fields := ResolveFooterConfig(ctx, platformKey)
	if !enabled {
		return "", nil
	}
	if len(fields) == 0 {
		fields = config.AsStrings(settings.DisplayRuntimeFooterFields.Default)
	}
	return FormatRuntimeFooter(ctx, in, fields)
}
