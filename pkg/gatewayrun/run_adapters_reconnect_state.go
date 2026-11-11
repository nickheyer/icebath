package gatewayrun

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/internal/pythread"
	"github.com/nickheyer/icebath/pkg/plugins"
)

// Plugin load re-arms allowed per queued platform before a broken plugin stops re-importing
const maxPluginLoadRearms = 3

// Actionable adapter unavailable status text shared by startup and the reconnect watchers
func adapterUnavailableMessage(platform gateway.Platform, retrying bool) string {
	message := fmt.Sprintf("No adapter available for enabled %s; check the plugin, dependencies, and credentials.", platform)
	if retrying {
		return message + " Retrying in the background."
	}
	return message
}

// Optional value of a claim key as the Python tuple or None stored in a queue entry
func claimValue(claim *claimKey) any {
	if claim == nil {
		return nil
	}
	return *claim
}

// Reconnect queue entry shared by startup failures and runtime fatals
func (r *GatewayRunner) reconnectQueueEntry(platform gateway.Platform, adapter gateway.PlatformAdapter, platformConfig *gateway.PlatformConfig, attempts int, delay float64, queued bool) map[string]any {
	now := pythread.Monotonic()
	claims := r.adapterClaims(platform, adapter)
	entry := map[string]any{"config": platformConfig, "attempts": attempts, "next_retry": now + delay}
	if queued {
		entry["queued_at"] = now
	}
	entry["credential_claim"] = claimValue(claims[0])
	entry["listener_claim"] = claimValue(claims[1])
	entry["predecessor"] = adapter
	return entry
}

// Flags a long failing reconnect as needing attention once, true when it was flagged now
func (r *GatewayRunner) flagReconnectNeedsAttention(ctx context.Context, platform gateway.Platform, info map[string]any, now float64, statusKey string) bool {
	if flagged, _ := info["attention_flagged"].(bool); flagged || !reconnectNeedsAttention(ctx, info, now) {
		return false
	}
	info["attention_flagged"] = true
	queuedAt, ok := info["queued_at"].(float64)
	if !ok {
		queuedAt = now
	}
	queuedFor := now - queuedAt
	key := statusKey
	if key == "" {
		key = string(platform)
	}
	attempts, _ := info["attempts"].(int)
	runLog().Warn(fmt.Sprintf("%s has been failing/reconnecting continuously for %.1f hours (%d attempts) — flagging "+
		"NEEDS_ATTENTION. Retries continue, but this usually means a permanent problem (revoked "+
		"credentials, missing intents, broken sidecar). Check `icebath status` / `/platform list`.",
		key, queuedFor/3600.0, attempts))
	since := time.Now().UTC().Add(-time.Duration(queuedFor * float64(time.Second)))
	r.updatePlatformRuntimeStatus(ctx, key, platformStatus{state: optStr("retrying"), needsAttention: optBool(true),
		retryingSince: optSince(pydatetime.ISOFormat(since))})
	return true
}

// Records an adapter's fatal error code and message as fatal runtime status
func (r *GatewayRunner) markPlatformFatal(ctx context.Context, statusKey string, adapter gateway.PlatformAdapter) {
	base := adapter.Base()
	r.updatePlatformRuntimeStatus(ctx, statusKey, platformStatus{state: optStr("fatal"), errorCode: base.FatalErrorCode(),
		errorMessage: base.FatalErrorMessage()})
}

// Marks the platform retrying and records the failed attempt, returning the backoff applied
func (r *GatewayRunner) bumpReconnectBackoff(ctx context.Context, platform gateway.Platform, info map[string]any, attempt int, errorCode *string, errorMessage string) float64 {
	r.updatePlatformRuntimeStatus(ctx, string(platform), platformStatus{state: optStr("retrying"), errorCode: errorCode,
		errorMessage: optStr(errorMessage)})
	retryNow, _ := info["retry_now"].(bool)
	delete(info, "retry_now")
	backoff := reconnectBackoff(attempt)
	if retryNow {
		backoff = 0
	}
	info["attempts"] = attempt
	info["next_retry"] = pythread.Monotonic() + backoff
	return backoff
}

// Whether a platform with no adapter can heal without a config change, only an unregistered plugin can
func (r *GatewayRunner) adapterMayHeal(ctx context.Context, platform gateway.Platform, platformConfig *gateway.PlatformConfig) bool {
	_, builtin := gateway.BuiltinAdapters[platform]
	return !builtin && !gateway.Registry.IsRegistered(ctx, string(platform)) && platformHasBotCredential(ctx, platform, platformConfig)
}

// Drops a platform from the reconnect queue with the reason logged
func (r *GatewayRunner) dropFromReconnectQueue(platform gateway.Platform, reason string) {
	runLog().Warn(fmt.Sprintf("Reconnect %s: %s, removing from retry queue", platform, reason))
	r.failedPlatforms.Delete(platform)
}

// Cancels profile scoped reconnects before their registry is torn down, waiting a bounded time
func (r *GatewayRunner) cancelSecondaryProfileReconnectTasks(ctx context.Context) error {
	pending := r.profileFailedPlatforms
	if pending == nil {
		return nil
	}
	current := currentGatewayTask(ctx)
	var tasks []*gatewayTask
	for _, profilePending := range pending {
		for _, task := range profilePending {
			if task != nil && task != current && !task.isDone() {
				tasks = append(tasks, task)
			}
		}
	}
	for _, task := range tasks {
		task.Cancel()
	}
	timeout := r.adapterDisconnectTimeoutSecs()
	if len(tasks) > 0 && timeout > 0 {
		deadline := time.NewTimer(time.Duration(timeout * float64(time.Second)))
		defer deadline.Stop()
	wait:
		for _, task := range tasks {
			select {
			case <-task.done:
			case <-deadline.C:
				break wait
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		unfinished := 0
		for _, task := range tasks {
			if !task.isDone() {
				unfinished++
			}
		}
		if unfinished > 0 {
			runLog().Warn(fmt.Sprintf("Timed out waiting for %d secondary profile reconnect task(s) during shutdown", unfinished))
		}
	}
	clear(pending)
	return nil
}

// Reports an unpaired WhatsApp or a secondary only shared ingress platform once with its status entry
func (r *GatewayRunner) noteUnservedSecondaryPlatform(ctx context.Context, profileName string, platform gateway.Platform) {
	r.adapterClaimsMu.Lock()
	if r.unservedSecondaryPlatforms == nil {
		r.unservedSecondaryPlatforms = pydict.New[secondaryKey, struct{}]()
	}
	key := secondaryKey{profileName, platform}
	if r.unservedSecondaryPlatforms.Has(key) {
		r.adapterClaimsMu.Unlock()
		return
	}
	r.unservedSecondaryPlatforms.Set(key, struct{}{})
	r.adapterClaimsMu.Unlock()
	pv := string(platform)
	statusKey := fmt.Sprintf("%s:%s", profileName, pv)
	if platform == gateway.PlatformWhatsApp {
		message := fmt.Sprintf("WhatsApp is not paired; pair it: icebath -p %s whatsapp", profileName)
		runLog().Info(fmt.Sprintf("[MULTIPLEX] Profile '%s': %s", profileName, message))
		r.updatePlatformRuntimeStatus(ctx, statusKey, platformStatus{state: optStr("disabled"),
			errorCode: optStr("whatsapp_unpaired"), errorMessage: optStr(message)})
		return
	}
	runLog().Info(fmt.Sprintf("[MULTIPLEX] Profile '%s': %s is enabled but not served — %s is process-level shared ingress "+
		"owned by the default profile under multiplex. Enable and configure %s on the default profile "+
		"(it serves every profile), or disable it in profile '%s'.", profileName, pv, pv, pv, profileName))
	r.updatePlatformRuntimeStatus(ctx, statusKey, platformStatus{state: optStr("disabled"),
		errorCode: optStr("multiplex_shared_ingress"), errorMessage: optStr("not served under multiplex (shared ingress owned by default)")})
}

// Loud not being served lines for shared ingress platforms secondaries enabled while no profile runs them
func (r *GatewayRunner) unservedSharedIngressWarnings() []string {
	r.adapterClaimsMu.Lock()
	noted := r.unservedSecondaryPlatforms.Keys()
	r.adapterClaimsMu.Unlock()
	var platforms []gateway.Platform
	for _, key := range noted {
		if !slices.Contains(platforms, key.platform) {
			platforms = append(platforms, key.platform)
		}
	}
	slices.Sort(platforms)
	var lines []string
	for _, platform := range platforms {
		if platform == gateway.PlatformWhatsApp || r.adapters.Has(platform) || r.failedPlatforms.Has(platform) {
			continue
		}
		var names []string
		for _, key := range noted {
			if key.platform == platform {
				names = append(names, key.profile)
			}
		}
		slices.Sort(names)
		lines = append(lines, fmt.Sprintf("%s is enabled in profile(s) %s but not on the default "+
			"profile — the platform is not being served. Under multiplex %s is shared "+
			"ingress: enable and configure it on the default profile, or disable it in those profiles.",
			platform, strings.Join(names, ", "), platform))
	}
	return lines
}

// Stamps adapter unavailable runtime status, retrying or fatal, for an enabled platform with no adapter
func (r *GatewayRunner) markAdapterUnavailable(ctx context.Context, statusKey string, platform gateway.Platform, retrying bool, needsAttention *bool) {
	state := "fatal"
	if retrying {
		state = "retrying"
	}
	r.updatePlatformRuntimeStatus(ctx, statusKey, platformStatus{state: optStr(state), errorCode: optStr("adapter_unavailable"),
		errorMessage: optStr(adapterUnavailableMessage(platform, retrying)), needsAttention: needsAttention})
}

// Replaces the retrying stamp of a never built platform whose retry gave up for a config reason
func (r *GatewayRunner) standDownUnbuiltSecondary(ctx context.Context, profileName string, platform gateway.Platform, reason string) {
	r.updatePlatformRuntimeStatus(ctx, fmt.Sprintf("%s:%s", profileName, platform), platformStatus{state: optStr("disabled"),
		errorMessage: optStr(reason), needsAttention: optBool(false)})
}

// Re-arms a failed plugin load for a queued platform off the loop, capped per queue entry
func (r *GatewayRunner) rearmFailedPluginLoad(ctx context.Context, platform gateway.Platform, info map[string]any) error {
	rearms, _ := info["load_rearms"].(int)
	if rearms >= maxPluginLoadRearms {
		return nil
	}
	manager := plugins.GetPluginManager(ctx)
	rearmed, err := r.runInExecutorWithContext(ctx, func(execCtx context.Context) (any, error) {
		return manager.RearmFailedPlatform(execCtx, string(platform)), nil
	})
	if err != nil {
		return err
	}
	if ok, _ := rearmed.(bool); ok {
		info["load_rearms"] = rearms + 1
	}
	return nil
}

// Platform bound authorization callback for adapters, scoped to the owning profile per call
func (r *GatewayRunner) makeAdapterAuthCheck(ctx context.Context, platform gateway.Platform, profileName string) gateway.AuthorizationCheck {
	transportHome := ""
	if r.multiplexOn() && profileName == "" {
		transportHome = home.Dir(ctx)
	}
	var profileHome *routedHome
	if profileName != "" {
		profileHome = routedProfileHome(profileName)
	}
	return func(userID string, chatType, chatID *string, opts gateway.AuthorizationOpts) (any, error) {
		if userID == "" {
			return false, nil
		}
		source := &gateway.SessionSource{Platform: platform, ChatType: "group", UserID: &userID, ThreadID: opts.ThreadID, IsBot: opts.IsBot}
		if chatID != nil {
			source.ChatID = *chatID
		}
		if chatType != nil && *chatType != "" {
			source.ChatType = *chatType
		}
		if profileName != "" {
			name := profileName
			source.Profile = &name
		}
		registry := r.adapters
		if profileName != "" {
			registry, _ = r.profileAdapters.Get(profileName)
		}
		if adapter, _ := registry.Get(platform); adapter != nil {
			gateway.SetTransportAdapterRef(source, adapter)
		}
		ctx := context.Background()
		if transportHome == "" {
			scoped := scopeOrNull(ctx, func(c context.Context, h string) context.Context {
				return gateway.ProfileRuntimeScope(c, h, false)
			}, profileHome)
			return r.isUserAuthorized(scoped, source, true)
		}
		if r.canonicalize(ctx, source, "", transportHome) == nil {
			return false, nil
		}
		return r.isUserAuthorizedForSource(ctx, source, true)
	}
}
