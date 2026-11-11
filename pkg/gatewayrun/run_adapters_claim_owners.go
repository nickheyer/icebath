package gatewayrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Exclusive resource tuple an adapter claims, a credential pair or a four field listener bind
type claimKey [4]any

// Credential claim as a resource key, nil for none
func credentialClaimKey(claim *[2]string) *claimKey {
	if claim == nil {
		return nil
	}
	return &claimKey{claim[0], claim[1]}
}

// Listener claim as a resource key, nil for none
func listenerClaimKey(claim *[4]any) *claimKey {
	if claim == nil {
		return nil
	}
	key := claimKey(*claim)
	return &key
}

// Whether the key is the four field listener claim rather than a credential pair
func (k claimKey) isListener() bool { return k[0] == "listener" }

// Held secondary claims of one profile platform and the task or adapter that owns the hold
type heldClaims struct {
	owner  any
	claims [2]*claimKey
}

// Credential and listener claims of one adapter, either nil
func (r *GatewayRunner) adapterClaims(platform gateway.Platform, adapter gateway.PlatformAdapter) [2]*claimKey {
	return [2]*claimKey{credentialClaimKey(adapterCredentialClaim(platform, adapter)), listenerClaimKey(adapterListenerClaim(platform, adapter))}
}

// Claim owner by resource for every live or queued primary adapter, a queued retryable primary owning both
func (r *GatewayRunner) primaryResourceClaims(active string) map[claimKey]string {
	claimed := map[claimKey]string{}
	for platform, adapter := range r.adapters.All() {
		var credential *claimKey
		if fp := adapterCredentialFingerprint(adapter); fp != "" {
			credential = &claimKey{string(platform), fp}
		}
		for _, claim := range []*claimKey{credential, listenerClaimKey(adapterListenerClaim(platform, adapter))} {
			if claim != nil {
				claimed[*claim] = active
			}
		}
	}
	for _, info := range r.failedPlatforms.All() {
		for _, name := range []string{"credential_claim", "listener_claim"} {
			if claim, ok := info[name].(claimKey); ok {
				claimed[claim] = active
			}
		}
	}
	return claimed
}

// Where a token was configured for a profile, its env file or the ambient env, empty when neither holds it
func credentialClaimOrigin(profileName, profileHome string, platform gateway.Platform, token any) string {
	envName := gateway.PlatformTokenEnvNames[platform]
	text, ok := token.(string)
	if envName == "" || !ok || pystr.Strip(text) == "" {
		return ""
	}
	text = pystr.Strip(text)
	if profileHome != "" && pystr.Strip(config.LoadEnvFile(filepath.Join(profileHome, ".env"))[envName]) == text {
		if profileName != "" && profileName != "default" {
			return fmt.Sprintf("profiles/%s/.env", profileName)
		}
		return ".env"
	}
	if pystr.Strip(os.Getenv(envName)) == text {
		return "env " + envName
	}
	return ""
}

// Origins of the owner and the incoming profile for a same credential refusal, either empty
func duplicateCredentialOrigins(owner, profileName, profileHome string, platform gateway.Platform, adapter gateway.PlatformAdapter) (string, string) {
	token := ""
	for _, value := range []any{adapterAttr(adapter, "token"), adapterAttr(adapter, "bot_token"), adapterConfigToken(adapter)} {
		if text, ok := value.(string); ok && pystr.Strip(text) != "" {
			token = pystr.Strip(text)
			break
		}
	}
	if token == "" {
		return "", ""
	}
	incoming := credentialClaimOrigin(profileName, profileHome, platform, token)
	ownerHome := ""
	if owner == "default" {
		func() {
			defer func() { _ = recover() }()
			ownerHome = home.Root()
		}()
	} else if dir, err := profiles.GetProfileDir(owner); err == nil {
		ownerHome = dir
	}
	ownerOrigin := ""
	if ownerHome != "" {
		ownerOrigin = credentialClaimOrigin(owner, ownerHome, platform, token)
	}
	return ownerOrigin, incoming
}

// Logs and parks a secondary adapter whose credential or listener another profile owns, true when refused
func (r *GatewayRunner) refuseDuplicateClaim(ctx context.Context, claim *claimKey, claimed map[claimKey]string, profileName string, platform gateway.Platform, kind, ownerOrigin, incomingOrigin string) bool {
	if claim == nil {
		return false
	}
	owner, ok := claimed[*claim]
	if !ok {
		return false
	}
	pv := string(platform)
	envDerived := strings.HasPrefix(ownerOrigin, "env ") || strings.HasPrefix(incomingOrigin, "env ")
	who := func(name, origin string) string {
		if envDerived && origin != "" {
			return fmt.Sprintf("%s (%s)", name, origin)
		}
		return name
	}
	head := fmt.Sprintf("Profile '%s' and '%s' both configure %s ", who(owner, ownerOrigin), who(profileName, incomingOrigin), pv)
	var message string
	if kind == "credential" {
		message = head + fmt.Sprintf("with the same credential. Give each profile its own %s credential.", pv)
		runLog().Error(fmt.Sprintf("Profile '%s' and '%s' both configure %s with the same credential — refusing to start the "+
			"duplicate (one credential cannot be consumed twice). Give each profile its own %s credential.",
			who(owner, ownerOrigin), who(profileName, incomingOrigin), pv, pv))
	} else {
		bind, port := claim[2], claim[3]
		message = head + fmt.Sprintf("sidecars on the same listener. Configure a distinct listener for profile '%s'.", profileName)
		runLog().Error(fmt.Sprintf("Profile '%s' and '%s' both configure %s sidecars on %v:%v — refusing to start the duplicate "+
			"listener. Set platforms.%s.extra.sidecar_port to a distinct port for profile '%s'.",
			owner, profileName, pv, bind, port, pv, profileName))
	}
	r.updatePlatformRuntimeStatus(ctx, fmt.Sprintf("%s:%s", profileName, platform), platformStatus{
		state: optStr("fatal"), errorCode: optStr("duplicate_" + kind), errorMessage: optStr(message)})
	return true
}

// Refuses a secondary adapter whose credential or listener another profile owns, else records the bot owner
func (r *GatewayRunner) claimSecondaryPlatform(ctx context.Context, profileName, profileHome string, platform gateway.Platform, adapter gateway.PlatformAdapter, claimed map[claimKey]string) ([2]*claimKey, bool) {
	claims := r.adapterClaims(platform, adapter)
	ownerOrigin, incomingOrigin := "", ""
	if claims[0] != nil {
		if owner := claimed[*claims[0]]; owner != "" {
			ownerOrigin, incomingOrigin = duplicateCredentialOrigins(owner, profileName, profileHome, platform, adapter)
		}
	}
	if r.refuseDuplicateClaim(ctx, claims[0], claimed, profileName, platform, "credential", ownerOrigin, incomingOrigin) ||
		r.refuseDuplicateClaim(ctx, claims[1], claimed, profileName, platform, "listener", "", "") {
		return claims, false
	}
	r.adapterClaimsMu.Lock()
	if r.profileBotPlatforms == nil {
		r.profileBotPlatforms = map[string]map[gateway.Platform]struct{}{}
	}
	if r.profileBotPlatforms[profileName] == nil {
		r.profileBotPlatforms[profileName] = map[gateway.Platform]struct{}{}
	}
	r.profileBotPlatforms[profileName][platform] = struct{}{}
	r.adapterClaimsMu.Unlock()
	return claims, true
}

// Drops the ownership record of a never built platform that gave up for good
func (r *GatewayRunner) disownSecondaryPlatform(profileName string, platform gateway.Platform) {
	r.adapterClaimsMu.Lock()
	defer r.adapterClaimsMu.Unlock()
	delete(r.profileBotPlatforms[profileName], platform)
}

// Holds a secondary platform's claims while it is outside the live map, a task owner releasing on completion
func (r *GatewayRunner) holdSecondaryClaims(profileName string, platform gateway.Platform, owner any, claims [2]*claimKey) {
	r.adapterClaimsMu.Lock()
	if r.secondaryHeldClaims == nil {
		r.secondaryHeldClaims = pydict.New[secondaryKey, heldClaims]()
	}
	key := secondaryKey{profileName, platform}
	previous, had := r.secondaryHeldClaims.Get(key)
	r.secondaryHeldClaims.Set(key, heldClaims{owner: owner, claims: claims})
	r.adapterClaimsMu.Unlock()
	if task, ok := owner.(*gatewayTask); ok && (!had || previous.owner != owner) {
		task.addDoneCallback(func(*gatewayFuture) { r.releaseSecondaryClaims(profileName, platform, task) })
	}
}

// Drops the hold on a profile platform when the owner still owns it
func (r *GatewayRunner) releaseSecondaryClaims(profileName string, platform gateway.Platform, owner any) {
	r.adapterClaimsMu.Lock()
	defer r.adapterClaimsMu.Unlock()
	key := secondaryKey{profileName, platform}
	if entry, ok := r.secondaryHeldClaims.Get(key); ok && entry.owner == owner {
		r.secondaryHeldClaims.Delete(key)
	}
}

// Claim owner for every held secondary claim but the excluded profile platform hold
func (r *GatewayRunner) heldClaimOwners(exclude *secondaryKey) map[claimKey]string {
	r.adapterClaimsMu.Lock()
	defer r.adapterClaimsMu.Unlock()
	out := map[claimKey]string{}
	for key, held := range r.secondaryHeldClaims.All() {
		if exclude != nil && key == *exclude {
			continue
		}
		for _, claim := range held.claims {
			if claim != nil {
				out[*claim] = key.profile
			}
		}
	}
	return out
}

// Current owner of each bot and listener, held claims but the excluded one overlaid by everything live
func (r *GatewayRunner) currentClaimOwners(exclude *secondaryKey) map[claimKey]string {
	active := r.primaryProfileName
	if active == "" {
		active = "default"
	}
	out := r.heldClaimOwners(exclude)
	for claim, owner := range r.liveResourceClaims(active) {
		out[claim] = owner
	}
	return out
}
