package model

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/portal"
)

type ReadinessQuery struct {
	Requested          string
	StrictProfileScope bool
	Resolve            func(ctx context.Context) (model string, route *Route, err error)
	Configured         func(ctx context.Context, strictProfileScope bool) (bool, error)
}

type Readiness struct {
	OK            bool
	Provider      string
	Model         string
	Source        string
	Error         string
	ErrorCode     string
	FreeTierRoute bool
	resolved      bool
}

func (r Readiness) MarshalJSON() ([]byte, error) {
	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	switch {
	case !r.resolved:
		out := map[string]any{"ok": false, "error": r.Error}
		if r.ErrorCode != "" {
			out["error_code"] = r.ErrorCode
		}
		return json.Marshal(out)
	case !r.OK:
		return json.Marshal(map[string]any{"ok": false, "provider": r.Provider, "model": nullable(r.Model), "source": nullable(r.Source), "error": r.Error})
	}
	return json.Marshal(map[string]any{"ok": true, "provider": nullable(r.Provider), "model": nullable(r.Model),
		"source": nullable(r.Source), "free_tier_route": r.FreeTierRoute})
}

func ReadinessFailure(err error) Readiness {
	out := Readiness{Error: err.Error()}
	var local *LocalRuntimeUnavailableError
	if errors.As(err, &local) {
		out.ErrorCode = local.Code()
	}
	return out
}

func (r *Router) CheckReadiness(ctx context.Context, q ReadinessQuery) (Readiness, error) {
	if q.Configured == nil {
		return Readiness{}, errors.New("model: the readiness probe needs the provider-configured check")
	}
	x, err := r.begin(ctx)
	if err != nil {
		return Readiness{}, err
	}
	live, err := x.liveConfig()
	if err != nil {
		return Readiness{}, err
	}
	configuredModel := live.Model.Default.ID
	var model string
	var rt *Route
	switch {
	case q.Resolve != nil:
		model, rt, err = q.Resolve(x.ctx)
	case q.Requested != "":
		model = configuredModel
		rt, err = r.Resolve(x.ctx, Target{Provider: q.Requested, Model: configuredModel})
	default:
		var entry *config.FallbackEntry
		rt, entry, err = r.ResolveWithFallback(x.ctx, Target{Model: configuredModel})
		if err == nil {
			model = configuredModel
			if entry != nil && entry.Model != "" {
				model = entry.Model
			}
			if model == "" {
				model = rt.Model
			}
		}
	}
	if err != nil {
		return Readiness{}, err
	}
	configured, err := q.Configured(x.ctx, q.StrictProfileScope)
	if err != nil {
		return Readiness{}, err
	}
	provider := firstOf(rt.Provider, "provider")
	source := rt.Source
	pinned := q.Requested
	if pinned == "" && !live.Model.Bare {
		pinned = strings.TrimSpace(live.Model.Provider)
	}
	blamed := firstOf(pinned, provider)
	fail := func(message, src string) Readiness {
		return Readiness{Provider: blamed, Model: model, Source: src, Error: message, resolved: true}
	}
	if !configured && provider == "bedrock" && (source == "iam-role" || source == "aws-sdk-default-chain") {
		return fail("No icebath provider is configured.", source), nil
	}
	text := strings.TrimSpace(keyText(rt.Credential))
	usable := rt.Credential.Kind == auth.CredToken || rt.Credential.Kind == auth.CredSigned ||
		slices.Contains([]string{"aws-sdk", noKeyPlaceholder}, text) || auth.HasUsableSecret(text, 4) || rt.Command != ""
	if !usable {
		return fail("No usable credentials found for "+blamed+"."+x.missingCredentialsSuffix(blamed), rt.Source), nil
	}
	return Readiness{OK: true, Provider: rt.Provider, Model: model, Source: rt.Source, resolved: true,
		FreeTierRoute: provider == portal.Provider && portal.RouteIsWelcomeHost(rt.BaseURL)}, nil
}
