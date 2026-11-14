package providers

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/nickheyer/icebath/internal/model/auth"
	"github.com/nickheyer/icebath/internal/model/wire"
)

type portalProfile struct {
	base
	d Deps
}

func newPortal(d Deps) Profile {
	return &portalProfile{d: d, base: base{spec: Spec{
		Name: ProviderPortal, Aliases: []string{"icebath-portal"}, EnvVars: []string{"ICEBATH_PORTAL_API_KEY"},
		DisplayName: "heyer.app", Description: "heyer.app — icebath model family", SignupURL: "https://icebath.app/",
		BaseURL: PortalInferenceURL, AuthType: authTypeOAuthDeviceCode,
	}}}
}

func PortalTags(ctx context.Context, version, sessionID string) []string {
	tags := []string{"product=icebath", "client=icebath-client-v" + version}
	effective := Conversation(ctx)
	if effective == "" {
		effective = sessionID
	}
	if effective != "" {
		tags = append(tags, "conversation="+effective)
	}
	return tags
}

func (p *portalProfile) AuxModel(ctx context.Context, vision bool) string {
	if p.d.Catalog == nil {
		return ""
	}
	return p.d.Catalog.PortalRecommendedAuxModel(ctx, vision)
}

func (p *portalProfile) ExtraBody(ctx context.Context, f RouteFacts) (map[string]json.RawMessage, error) {
	body := map[string]json.RawMessage{"tags": wire.Strings(PortalTags(ctx, p.d.Net.Version, f.SessionID))}
	if sticky := AffinityKey(ctx, f.SessionID); sticky != "" {
		body["session_id"] = wire.Quote(sticky)
	}
	if len(f.DataUse) > 0 {
		prefs := (&wire.Dict{}).SetString("data_collection", "deny")
		if slices.Contains(f.DataUse, "zero_retention") {
			prefs.SetBool("zdr", true)
		}
		body["provider"] = prefs.JSON()
	}
	return body, nil
}

func portalCannotDisableReasoning(caps *ReasoningCaps) bool {
	if caps == nil {
		return true
	}
	return !caps.Supports || caps.Mandatory
}

func (p *portalProfile) APIExtras(ctx context.Context, f RouteFacts) (Extras, error) {
	if !f.SupportsReasoning {
		return Extras{}, nil
	}
	if f.Reasoning == nil {
		return Extras{Body: map[string]json.RawMessage{"reasoning": json.RawMessage(`{"enabled":true,"effort":"medium"}`)}}, nil
	}
	if !f.Reasoning.Enabled && portalCannotDisableReasoning(f.Caps) {
		return Extras{}, nil
	}
	return Extras{Body: map[string]json.RawMessage{"reasoning": reasoningObject(f.Reasoning)}}, nil
}

func (p *portalProfile) RuntimeKey(e *auth.Entry) (auth.Secret, bool) {
	return p.d.Portal.RuntimeKey(e)
}

func (p *portalProfile) StateSeedable(state json.RawMessage) bool {
	return p.d.Portal.StateSeedable(state)
}

func (p *portalProfile) EntitlementMessage(ctx context.Context) string {
	return p.d.Portal.PortalEntitlementMessage(ctx, "Icebath Portal model access")
}
