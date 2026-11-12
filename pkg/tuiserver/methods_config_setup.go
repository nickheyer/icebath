package tuiserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Readiness budgets, the overlapping poll join grace and the retryable in progress code
const (
	readinessShareWaitSeconds       = 4.0
	readinessJoinGraceSeconds       = 0.5
	readinessStatusShareWaitSeconds = 12.0
	readinessInProgressErr          = 5097
)

// Debug report bundle collected, extended with the client files, gzipped and uploaded to Nous, assigned in cmd/icebath/wiring.go
var ShareNousDiagnostics func(ctx context.Context, logLines int, clientFiles [][2]string) (*pyjson.Object, error)

// Strict debug support scrub capped at a length, negative meaning no cap, assigned in cmd/icebath/wiring.go
var RedactDebugSupportText func(value any, maxChars int) string

// One single flighted readiness probe, its answer or failure set once it settles
type readinessFuture struct {
	done      chan struct{}
	result    map[string]any
	failure   any
	cancelled bool
}

// Dedicated readiness executor and the probes in flight by kind and profile
var (
	readinessPool     = newRPCWorkerPool(max(2, min(4, envInt("ICEBATH_TUI_RPC_POOL_WORKERS", 8))))
	readinessMu       sync.Mutex
	readinessInflight = map[[2]string]*readinessFuture{}
)

var configSetupRegistry = &handlerRegistry{}

func init() {
	registerAtExit(func() { readinessPool.Shutdown(true) })
	configSetupRegistry.method("setup.status", setupStatus)
	configSetupRegistry.method("setup.runtime_check", setupRuntimeCheck)
	configSetupRegistry.method("diagnostics.share_nous", diagnosticsShareNous)
	configSetupRegistry.install()
}

// Forgets a settled probe so the next poll re-probes, logging a failure nobody waited for
func readinessCleared(key [2]string, future *readinessFuture) {
	readinessMu.Lock()
	if readinessInflight[key] == future {
		delete(readinessInflight, key)
	}
	readinessMu.Unlock()
	if !future.cancelled && future.failure != nil {
		logger().Debug(fmt.Sprintf("readiness probe %s failed after its callers returned: %s", pyrepr.Repr(key), failureText(future.failure)))
	}
}

// Single flights a probe under key on the readiness pool, answering the retryable error when it outlives the wait
func readinessShare(rid any, key [2]string, runProbe func() map[string]any, waitSeconds float64) map[string]any {
	readinessMu.Lock()
	future := readinessInflight[key]
	owner := future == nil
	if owner {
		future = &readinessFuture{done: make(chan struct{})}
		readinessInflight[key] = future
		started := false
		err := readinessPool.Submit(func() {
			started = true
			defer func() {
				if failure := recover(); failure != nil {
					future.failure = failure
				}
			}()
			future.result = runProbe()
		}, func() {
			future.cancelled = !started
			close(future.done)
			readinessCleared(key, future)
		})
		if err != nil {
			delete(readinessInflight, key)
			readinessMu.Unlock()
			panic(err)
		}
	}
	readinessMu.Unlock()
	wait := waitSeconds
	if !owner {
		wait = min(waitSeconds, readinessJoinGraceSeconds)
	}
	select {
	case <-future.done:
	case <-time.After(time.Duration(wait * float64(time.Second))):
		logger().Warn(fmt.Sprintf("readiness probe %s exceeded its budget (%.1fs); it continues in the background", pyrepr.Repr(key), waitSeconds))
		return rpcErr(rid, readinessInProgressErr, "readiness check still in progress; retrying next tick", nil)
	}
	if future.cancelled {
		panic(fmt.Errorf("CancelledError"))
	}
	if future.failure != nil {
		panic(future.failure)
	}
	return ok(rid, future.result)
}

// Probe of setup.status or setup.runtime_check under the optional profile's runtime scope, an unknown profile answering ok false
func readinessCheck(ctx context.Context, rid any, params map[string]any, probe func(ctx context.Context, profile string, scoped map[string]any) map[string]any, probeKey string, waitSeconds float64) map[string]any {
	profile := ""
	if params != nil {
		profile = strings.TrimSpace(relayString(params["profile"]))
	}
	scopeHome := ""
	if profile != "" {
		if !profiles.ProfileExists(profile) {
			return ok(rid, map[string]any{"ok": false, "profile": params["profile"],
				"error": fmt.Sprintf("Profile '%s' does not exist on this backend.", profile)})
		}
		resolved, err := profileHome(profile)
		if err != nil {
			panic(err)
		}
		scopeHome = resolved
	}
	worker := context.WithoutCancel(ctx)
	scoped := map[string]any{}
	if profile != "" {
		scoped["profile"] = profile
	}
	return readinessShare(rid, [2]string{probeKey, profile}, func() map[string]any {
		return probe(profileRuntimeScope(worker, scopeHome, true), profile, scoped)
	}, waitSeconds)
}

// Merges scoped into out
func withScoped(out, scoped map[string]any) map[string]any {
	for key, value := range scoped {
		out[key] = value
	}
	return out
}

// Loose provider check answered from the boot setup record for the launch profile, else a live probe
func setupStatus(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = rpcErr(rid, 5016, failureText(failure), nil)
		}
	}()
	probe := func(scoped context.Context, profile string, stamp map[string]any) map[string]any {
		var record *provider.SetupRecord
		if profile == "" {
			record = provider.WaitForSetupRecord(scopedPoolEnv(scoped), provider.SetupReadyWait)
		}
		if record == nil {
			var launch *provider.SetupRecord
			if profile != "" {
				launch = provider.WaitForSetupRecord(scopedPoolEnv(scoped), provider.SetupReadyWait)
			}
			out := map[string]any{"provider_configured": gateway.HasAnyProviderConfigured(scoped, profile != "")}
			if launch != nil {
				out["ready"], out["free_tier_account"], out["free_tier_route"] = true, launch.FreeTierAccount, provider.FreeTierRoute(scoped)
			}
			return withScoped(out, stamp)
		}
		out := map[string]any{"provider_configured": record.ProviderConfigured, "ready": true, "free_tier_account": record.FreeTierAccount,
			"free_tier_route": record.FreeTierRoute(), "other_providers": record.OtherProviders, "inference_provider": record.InferenceProvider}
		for key, value := range record.FailureFields() {
			out[key] = value
		}
		return withScoped(out, stamp)
	}
	return readinessCheck(ctx, rid, params, probe, "status", readinessStatusShareWaitSeconds)
}

// Strict readiness of the runtime a session would build with, a failure naming the provider the user pinned
func setupRuntimeCheck(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			err, isError := failure.(error)
			if !isError {
				err = errors.New(failureText(failure))
			}
			response = ok(rid, pyjson.Plain(gateway.ReadinessFailure(err)))
		}
	}()
	requested := strings.TrimSpace(relayString(params["provider"]))
	probe := func(scoped context.Context, profile string, stamp map[string]any) map[string]any {
		startupModel, startupProvider := resolveStartupRuntime(scoped)
		var model any
		var runtime *provider.Runtime
		if requested != "" {
			resolved, err := provider.ResolveRuntimeProvider(scoped, home.Dir(scoped), requested, startupModel, "", "")
			if err != nil {
				panic(err)
			}
			model, runtime = startupModel, resolved
		} else {
			resolvedModel, resolved, _, err := resolveAgentModelRuntime(scoped, nil, "")
			if err != nil {
				panic(err)
			}
			model, runtime = resolvedModel, resolved
		}
		providerConfigured := gateway.HasAnyProviderConfigured(scoped, profile != "")
		providerName := runtime.Provider
		if providerName == "" {
			providerName = "provider"
		}
		source := runtime.Source
		pinned := requested
		if pinned == "" {
			pinned = startupProvider
		}
		if pinned == "" {
			if cfgModel, isDict := loadCfg(scoped)["model"].(map[string]any); isDict {
				pinned = strings.TrimSpace(relayString(cfgModel["provider"]))
			}
		}
		blamed := pinned
		if blamed == "" {
			blamed = providerName
		}
		fail := func(message string, src any) map[string]any {
			return withScoped(map[string]any{"ok": false, "provider": blamed, "model": model, "source": src, "error": message}, stamp)
		}
		if !providerConfigured && providerName == "bedrock" && (source == "iam-role" || source == "aws-sdk-default-chain") {
			return fail("No Icebath provider is configured.", source)
		}
		callable := runtime.TokenSource != nil
		apiKeyText := ""
		if !callable {
			apiKeyText = strings.TrimSpace(runtime.APIKey)
		}
		if !(callable || apiKeyText == "aws-sdk" || apiKeyText == "no-key-required" || provider.HasUsableSecret(apiKeyText, 4) || runtime.Command != "") {
			var src any
			if runtime.Source != "" {
				src = runtime.Source
			}
			return fail("No usable credentials found for "+blamed+"."+provider.MissingCredentialsSuffix(scoped, blamed), src)
		}
		var resolvedProvider, resolvedSource any
		if runtime.Provider != "" {
			resolvedProvider = runtime.Provider
		}
		if runtime.Source != "" {
			resolvedSource = runtime.Source
		}
		return withScoped(map[string]any{"ok": true, "provider": resolvedProvider, "model": model, "source": resolvedSource,
			"free_tier_route": providerName == "nous" && provider.RouteIsWelcomeHost(runtime.BaseURL)}, stamp)
	}
	return readinessCheck(ctx, rid, params, probe, "runtime:"+requested, readinessShareWaitSeconds)
}

// Strict scrubbed client text capped, the scrub seeing at most three caps so an unbounded param stays bounded
func redactClientText(text string, limit int) string {
	runes := []rune(text)
	cut := -1
	if 2*limit < len(runes) {
		for i := 2 * limit; i < min(3*limit, len(runes)); i++ {
			if runes[i] == '\n' {
				cut = i
				break
			}
		}
	}
	window := runes
	if cut >= 0 {
		window = runes[:cut+1]
	} else if len(runes) > 2*limit {
		window = runes[:2*limit]
	}
	return RedactDebugSupportText(string(window), limit)
}

// Client file label kept to letters, digits and . _ - space and parentheses, capped and stripped of traversal shapes
func safeClientLabel(label string) string {
	var kept []rune
	for _, ch := range label {
		if unicode.IsLetter(ch) || unicode.IsDigit(ch) || unicode.Is(unicode.Nl, ch) || unicode.Is(unicode.No, ch) || strings.ContainsRune("._- ()", ch) {
			kept = append(kept, ch)
		}
	}
	safe := []rune(strings.TrimSpace(string(kept)))
	if len(safe) > 64 {
		safe = safe[:64]
	}
	text := string(safe)
	for strings.Contains(text, "..") {
		text = strings.ReplaceAll(text, "..", ".")
	}
	return strings.TrimSpace(strings.TrimLeft(text, "."))
}

// Uploads a force redacted debug bundle with the client's error context and files to Nous diagnostics storage
func diagnosticsShareNous(ctx context.Context, rid any, params map[string]any) (response map[string]any) {
	defer func() {
		if failure := recover(); failure != nil {
			if configOutage(failure) {
				panic(failure)
			}
			response = ok(rid, map[string]any{"ok": false, "error": RedactDebugSupportText(failureText(failure), -1)})
		}
	}()
	logLines, isInt := params["log_lines"].(int)
	if !isInt || logLines < 10 || logLines > 2000 {
		logLines = 200
	}
	var clientFiles [][2]string
	if errorContext, isText := params["error_context"].(string); isText && strings.TrimSpace(errorContext) != "" {
		clientFiles = append(clientFiles, [2]string{"error-context.txt", redactClientText(strings.TrimSpace(errorContext), 8_000)})
	}
	if extra, isDict := params["extra_files"].(map[string]any); isDict {
		labels := sortedKeys(extra)
		for _, label := range labels[:min(4, len(labels))] {
			text, isText := extra[label].(string)
			safe := safeClientLabel(label)
			if safe != "" && isText && strings.TrimSpace(text) != "" {
				clientFiles = append(clientFiles, [2]string{"client/" + safe, redactClientText(text, 524_288)})
			}
		}
	}
	res, err := ShareNousDiagnostics(ctx, logLines, clientFiles)
	if err != nil {
		return ok(rid, map[string]any{"ok": false, "error": RedactDebugSupportText(err.Error(), -1)})
	}
	viewURL := firstTruthy(res.GetOr("viewUrl", nil), res.GetOr("view_url", nil))
	uploadID := res.GetOr("id", nil)
	if !truthy(viewURL) && !truthy(uploadID) {
		return ok(rid, map[string]any{"ok": false, "error": "upload succeeded but returned no view URL or id"})
	}
	return ok(rid, map[string]any{"ok": true, "view_url": viewURL, "upload_id": uploadID,
		"expires_at": firstTruthy(res.GetOr("expiresAt", nil), res.GetOr("expires_at", nil))})
}

// First truthy value, else the last one as Python or does
func firstTruthy(values ...any) any {
	for _, value := range values {
		if truthy(value) {
			return value
		}
	}
	return values[len(values)-1]
}
