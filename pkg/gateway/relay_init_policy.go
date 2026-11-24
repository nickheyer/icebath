package gateway

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Connector relevance policy of a fronted platform, nil when relay is generic or nothing non default is configured
func RelayRelevancePolicy(ctx context.Context, platform *string) *jsonutil.Object {
	name := ""
	if platform == nil {
		name = RelayPlatformIdentity()[0]
	} else {
		name = *platform
	}
	if name == "" || name == "relay" {
		return nil
	}
	requireMention, freeResponse := relayPolicyConfig(ctx, name)
	allowBotsEnv := strings.TrimSpace(strings.ToLower(getenvStr(SettingsContext(ctx), strings.ToUpper(name)+"_ALLOW_BOTS", "")))
	allowOtherBots := allowBotsEnv == "mentions" || allowBotsEnv == "all"
	if requireMention == nil && len(freeResponse) == 0 && !allowOtherBots {
		return nil
	}
	scopes := make([]any, len(freeResponse))
	for i, s := range freeResponse {
		scopes[i] = s
	}
	return jsonutil.ObjectOf("platform", name, "requireAddress", anyutil.IsTrue(requireMention),
		"freeResponseScopes", scopes, "allowOtherBots", allowOtherBots)
}

// Configured require mention value, nil when unset, and the free response scopes, abandoned at the first malformed block
func relayPolicyConfig(ctx context.Context, platform string) (requireMention any, freeResponse []string) {
	cfg := config.EffectiveUserConfig(ctx)
	platCfg, ok := relayPolicyPlatformBlock(cfg, platform)
	if !ok {
		return nil, nil
	}
	if v, has := platCfg["require_mention"]; has {
		requireMention = v
	} else if v := cfg["require_mention"]; v != nil {
		requireMention = v
	}
	frc := platCfg["free_response_channels"]
	if frc == nil {
		frc = cfg["free_response_channels"]
	}
	switch v := frc.(type) {
	case []any:
		for _, c := range v {
			if s := strings.TrimSpace(anyutil.String(c)); s != "" {
				freeResponse = append(freeResponse, s)
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			for _, c := range strings.Split(v, ",") {
				if s := strings.TrimSpace(c); s != "" {
					freeResponse = append(freeResponse, s)
				}
			}
		}
	}
	return requireMention, freeResponse
}

// The top level block of a platform, else its platforms block, ok false when platforms is set but not a mapping
func relayPolicyPlatformBlock(cfg map[string]any, platform string) (map[string]any, bool) {
	if block, ok := cfg[platform].(map[string]any); ok {
		return block, true
	}
	platsRaw := cfg["platforms"]
	if !anyutil.IsTrue(platsRaw) {
		platsRaw = map[string]any{}
	}
	plats, ok := platsRaw.(map[string]any)
	if !ok {
		return nil, false
	}
	if block, ok := plats[platform].(map[string]any); ok {
		return block, true
	}
	return map[string]any{}, true
}

// HTTP status of the policy POST, RelayRuntimeError when the connector is unreachable
func relayPostPolicy(policyURL, token string, policy *jsonutil.Object) (int, error) {
	resp, err := relayJSONPost(policyURL, token, policy, relayHTTPTimeout)
	if err != nil {
		var httpErr *RelayHTTPError
		if errors.As(err, &httpErr) {
			return httpErr.Code, nil
		}
		var urlErr *RelayURLError
		if errors.As(err, &urlErr) {
			return 0, &RelayRuntimeError{Msg: "could not reach connector: " + urlErr.Reason}
		}
		return 0, err
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

// Declares the relevance policy per fronted platform, true when the connector accepted at least one
func SendRelayPolicy(ctx context.Context) (bool, error) {
	dialURL, err := RelayURLChecked(ctx)
	if err != nil {
		return false, err
	}
	if dialURL == "" {
		return false, nil
	}
	gatewayID, secret := RelayConnectionAuth(ctx)
	if gatewayID == "" || secret == "" {
		return false, nil
	}
	token := RelayMakeUpgradeToken(gatewayID, secret, relayDefaultUpgradeTTLSeconds)
	policyURL := relayConnectorURL(dialURL, "/relay/policy")
	anyDeclared := false
	for _, id := range RelayPlatformIdentities() {
		platform := id[0]
		policy := RelayRelevancePolicy(ctx, &platform)
		if policy == nil {
			continue
		}
		status, err := relayPostPolicy(policyURL, token, policy)
		if err != nil {
			pkgLog().Warn(fmt.Sprintf("relay policy declaration failed for platform=%s (%s); continuing", platform, relayExcStr(err)))
			continue
		}
		if status == 200 {
			anyDeclared = true
			scopes, _ := policy.ValueOr("freeResponseScopes", nil).([]any)
			pkgLog().Info(fmt.Sprintf("relay policy declared (platform=%s require_address=%s free_scopes=%d allow_bots=%s)",
				anyutil.String(policy.ValueOr("platform", nil)), anyutil.String(policy.ValueOr("requireAddress", nil)), len(scopes),
				anyutil.String(policy.ValueOr("allowOtherBots", nil))))
		} else {
			pkgLog().Warn(fmt.Sprintf("relay policy declaration for platform=%s returned HTTP %d; connector keeps prior/default policy", platform, status))
		}
	}
	return anyDeclared, nil
}
