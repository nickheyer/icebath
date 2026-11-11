package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/cron"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/profiles"
)

var cronListSplitRe = regexp.MustCompile(`[\n,]`)

// Stripped text of a value, nil for None or blank, trailing slashes cut when asked
func cronOptionalText(value any, stripTrailingSlash bool) *string {
	if value == nil {
		return nil
	}
	text := pystr.Strip(pyrepr.Str(value))
	if stripTrailingSlash {
		text = pystr.RstripChars(text, "/")
	}
	if text == "" {
		return nil
	}
	return &text
}

// Optional text as a value, None when absent
func optAny(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Non blank stripped items of a comma or newline list or a sequence, nil when none remain
func cronStringList(value any) []any {
	var raw []any
	switch v := value.(type) {
	case string:
		for _, part := range cronListSplitRe.Split(v, -1) {
			raw = append(raw, part)
		}
	case []any:
		raw = v
	case []string:
		for _, s := range v {
			raw = append(raw, s)
		}
	default:
		return nil
	}
	var items []any
	for _, item := range raw {
		if text := pystr.Strip(pyrepr.Str(item)); text != "" {
			items = append(items, text)
		}
	}
	return items
}

// Script path relative to the profile scripts dir, 400 when outside it, missing or not a file
func normalizeDashboardCronScript(value any, profileHome string) (any, error) {
	text := cronOptionalText(value, false)
	if text == nil {
		return nil, nil
	}
	scriptsRoot, err := pypath.Resolve(filepath.Join(profileHome, "scripts"))
	if err != nil {
		return nil, err
	}
	rawPath, err := pypath.PathExpanduser(*text)
	if err != nil {
		return nil, err
	}
	target := rawPath
	if !filepath.IsAbs(rawPath) {
		target = filepath.Join(scriptsRoot, rawPath)
	}
	candidate, err := pypath.Resolve(target)
	if err != nil {
		return nil, err
	}
	relative, ok := pypath.PathRelativeTo(candidate, scriptsRoot)
	if !ok {
		return nil, NewHTTPException(400, fmt.Sprintf("script must be inside %s", scriptsRoot))
	}
	st, statErr := os.Stat(candidate)
	if statErr != nil {
		return nil, NewHTTPException(400, fmt.Sprintf("script does not exist: %s", candidate))
	}
	if !st.Mode().IsRegular() {
		return nil, NewHTTPException(400, fmt.Sprintf("script is not a file: %s", candidate))
	}
	return relative, nil
}

// 400 unless a no agent job has a script and an agent job has a prompt, skill or script
func validateDashboardCronEffectiveJob(job map[string]any) error {
	prompt := cronOptionalText(job["prompt"], false)
	script := cronOptionalText(job["script"], false)
	skills := cronStringList(job["skills"])
	if len(skills) == 0 {
		skills = cronStringList(job["skill"])
	}
	if pyrepr.Truthy(job["no_agent"]) {
		if script == nil {
			return NewHTTPException(400, "no_agent=True requires a script")
		}
		return nil
	}
	if prompt == nil && len(skills) == 0 && script == nil {
		return NewHTTPException(400, "agent cron jobs require a prompt, skill, or script")
	}
	return nil
}

// 400 for a context_from reference that names no job of the profile, the self toggle exempt
func validateDashboardCronContextFrom(ctx context.Context, refs []any, profileName string) error {
	for _, ref := range refs {
		if s, ok := ref.(string); ok && pystr.Lower(pystr.Strip(s)) == "self" {
			continue
		}
		refText := pyrepr.Str(ref)
		found, err := callCronForProfile(ctx, profileName, func(ctx context.Context, h string) (any, error) {
			return cron.GetJob(ctx, h, refText)
		})
		if err != nil {
			return err
		}
		if !pyrepr.Truthy(found) {
			return NewHTTPException(400, fmt.Sprintf("context_from job '%s' not found in profile '%s'", refText, profileName))
		}
	}
	return nil
}

// Name, path and default flag of every served profile, the directory scan when listing fails
func cronProfileDicts(ctx context.Context) []*pyjson.Object {
	served, err := profiles.ProfilesToServe(ctx, true, true, true)
	if err != nil {
		webServerLog().Error("Failed to list profiles for cron dashboard; falling back to directory scan", "exc", err)
		return fallbackProfileDicts(ctx)
	}
	rows := make([]*pyjson.Object, 0, len(served))
	for _, p := range served {
		rows = append(rows, pyjson.ObjectOf("name", p.Name, "path", p.Home, "is_default", p.Name == "default"))
	}
	return rows
}

// Job dict with its profile, home, default flag and scheduler heartbeat age added
func annotateCronJob(job *pyjson.Object, profile, profileHome string, heartbeatAge any) *pyjson.Object {
	out := job.Copy()
	out.Set("profile", profile)
	out.Set("profile_name", profile)
	out.Set("hermes_home", profileHome)
	out.Set("is_default_profile", profile == "default")
	out.Set("scheduler_heartbeat_age_s", heartbeatAge)
	return out
}

// Ordered dict of a stored job as its JSON record holds it
func cronJobDict(job *cron.Job) (*pyjson.Object, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, err
	}
	return pyjson.LoadsDict(string(raw))
}

// Context with the home override pointed at one profile home for its cron store
func cronStoreScope(ctx context.Context, profileHome string) context.Context {
	return home.WithOverride(ctx, profileHome)
}

// Result of a cron store call against a profile, jobs annotated with the profile they came from
func callCronForProfile(ctx context.Context, targetProfile string, fn func(ctx context.Context, h string) (any, error)) (any, error) {
	profileName, profileHome, err := cronProfileHome(ctx, targetProfile)
	if err != nil {
		return nil, err
	}
	result, err := fn(cronStoreScope(ctx, profileHome), profileHome)
	if err != nil {
		return nil, err
	}
	var heartbeatAge any
	if age, ok := cron.TickerHeartbeatAge(profileHome); ok {
		heartbeatAge = age
	}
	switch r := result.(type) {
	case []*cron.Job:
		out := make([]*pyjson.Object, 0, len(r))
		for _, j := range r {
			d, err := cronJobDict(j)
			if err != nil {
				return nil, err
			}
			out = append(out, annotateCronJob(d, profileName, profileHome, heartbeatAge))
		}
		return out, nil
	case *cron.Job:
		if r == nil {
			return nil, nil
		}
		d, err := cronJobDict(r)
		if err != nil {
			return nil, err
		}
		return annotateCronJob(d, profileName, profileHome, heartbeatAge), nil
	case *pyjson.Object:
		if r == nil {
			return nil, nil
		}
		return annotateCronJob(r, profileName, profileHome, heartbeatAge), nil
	}
	return result, nil
}

// Jobs of one profile store with disabled ones included
func cronListJobs(ctx context.Context, profile string) ([]*pyjson.Object, error) {
	result, err := callCronForProfile(ctx, profile, func(ctx context.Context, h string) (any, error) {
		return cron.ListJobs(ctx, h, true)
	})
	if err != nil {
		return nil, err
	}
	return result.([]*pyjson.Object), nil
}

// Reconciles the profile scheduler provider, skipping an external one while several profiles are served
func notifyCronProviderForProfile(ctx context.Context, targetProfile string) {
	_, profileHome, err := cronProfileHome(ctx, targetProfile)
	if err != nil {
		webServerLog().Debug(fmt.Sprintf("Cron provider reconciliation failed for profile %s", targetProfile), "exc", err)
		return
	}
	scoped := cronStoreScope(ctx, profileHome)
	provider := cron.ResolveCronScheduler(profileHome)
	_, builtin := provider.(*cron.InProcessCronScheduler)
	if !builtin {
		named := 0
		for _, p := range cronProfileDicts(scoped) {
			if pyrepr.Truthy(p.GetOr("name", nil)) {
				named++
			}
		}
		if named > 1 {
			webServerLog().Warn(fmt.Sprintf("Skipping cron provider reconcile for profile %s: external provider '%s' reconcile is not "+
				"profile-scoped and would disarm other profiles' armed one-shots. The mutated profile re-arms "+
				"idempotently on its next fire/start.", targetProfile, provider.Name()))
			return
		}
	}
	if err := cron.SchedulerOnJobsChanged(provider); err != nil {
		webServerLog().Debug(fmt.Sprintf("Cron provider reconciliation failed for profile %s", targetProfile), "exc", err)
	}
}

// Store mutation against a profile, reconciling its provider when the result is truthy
func mutateCronForProfile(ctx context.Context, targetProfile string, fn func(ctx context.Context, h string) (any, error)) (any, error) {
	result, err := callCronForProfile(ctx, targetProfile, fn)
	if err != nil {
		return nil, err
	}
	if cronResultTruthy(result) {
		notifyCronProviderForProfile(ctx, targetProfile)
	}
	return result, nil
}

// Python truth of an annotated cron call result
func cronResultTruthy(result any) bool {
	switch r := result.(type) {
	case []*pyjson.Object:
		return len(r) > 0
	case *pyjson.Object:
		return r != nil
	}
	return pyrepr.Truthy(result)
}

// First served profile whose store holds the job by id or name, empty when none
func findCronJobProfile(ctx context.Context, jobID string) (string, error) {
	for _, p := range cronProfileDicts(ctx) {
		name := pyrepr.Str(pyrepr.Or(p.GetOr("name", nil), ""))
		if name == "" {
			continue
		}
		jobs, err := cronListJobs(ctx, name)
		if err != nil {
			return "", err
		}
		if cronJobsHold(jobs, jobID) {
			return name, nil
		}
	}
	return "", nil
}

// Whether any job carries the id or name
func cronJobsHold(jobs []*pyjson.Object, jobID string) bool {
	for _, j := range jobs {
		if j.GetOr("id", nil) == jobID || j.GetOr("name", nil) == jobID {
			return true
		}
	}
	return false
}

// 424 with the structured envelope for a saved job whose scheduler registration failed
func raiseIfCronRegistrationError(err error) error {
	var reg *cron.CronSchedulerRegistrationError
	if errors.As(err, &reg) {
		return NewHTTPException(424, reg.ToDict())
	}
	return nil
}

// Creates a cron job in a profile from a validated dashboard body
func createCronJobSync(ctx context.Context, body *pyjson.Object, fieldsSet map[string]bool, profile string) (any, error) {
	result, err := createCronJobChecked(ctx, body, fieldsSet, profile)
	if err == nil {
		return result, nil
	}
	if _, ok := asHTTPException(err); ok {
		return nil, err
	}
	if regErr := raiseIfCronRegistrationError(err); regErr != nil {
		return nil, regErr
	}
	webServerLog().Error("POST /api/cron/jobs failed", "exc", err)
	return nil, NewHTTPException(400, pyrepr.ExceptionMessage(err))
}

// Validation and store create of one dashboard cron job, raising every failure
func createCronJobChecked(ctx context.Context, body *pyjson.Object, fieldsSet map[string]bool, profile string) (any, error) {
	profileName, profileHome, err := cronProfileHome(ctx, profile)
	if err != nil {
		return nil, err
	}
	script, err := normalizeDashboardCronScript(body.GetOr("script", nil), profileHome)
	if err != nil {
		return nil, err
	}
	skills := cronStringList(body.GetOr("skills", nil))
	contextFrom := cronStringList(body.GetOr("context_from", nil))
	if err := validateDashboardCronContextFrom(ctx, contextFrom, profileName); err != nil {
		return nil, err
	}
	noAgent := pyrepr.Truthy(body.GetOr("no_agent", false))
	if repeat := body.GetOr("repeat", nil); repeat != nil {
		if n, isInt := repeat.(int64); isInt && n < 1 {
			return nil, NewHTTPException(400, "repeat must be a positive integer")
		}
		if _, err := cron.NormalizeRepeatValue(repeat); err != nil {
			return nil, err
		}
	}
	if err := validateDashboardCronEffectiveJob(map[string]any{
		"prompt": body.GetOr("prompt", nil), "skills": listOrNone(skills), "script": script, "no_agent": noAgent,
	}); err != nil {
		return nil, err
	}
	prompt := body.GetOr("prompt", nil)
	if !pyrepr.Truthy(prompt) {
		prompt = ""
	}
	deliver := cronOptionalText(body.GetOr("deliver", nil), false)
	if deliver == nil {
		local := "local"
		deliver = &local
	}
	spec := pyjson.ObjectOf(
		"prompt", prompt,
		"schedule", body.GetOr("schedule", nil),
		"name", body.GetOr("name", nil),
		"repeat", body.GetOr("repeat", nil),
		"deliver", *deliver,
		"skills", listOrNone(skills),
		"model", optAny(cronOptionalText(body.GetOr("model", nil), false)),
		"provider", optAny(cronOptionalText(body.GetOr("provider", nil), false)),
		"base_url", optAny(cronOptionalText(body.GetOr("base_url", nil), true)),
		"script", script,
		"context_from", listOrNone(contextFrom),
		"enabled_toolsets", listOrNone(cronStringList(body.GetOr("enabled_toolsets", nil))),
		"workdir", optAny(cronOptionalText(body.GetOr("workdir", nil), false)),
		"no_agent", noAgent,
		"timezone", optAny(cronOptionalText(body.GetOr("timezone", nil), false)),
		"grace_minutes", body.GetOr("grace_minutes", nil),
	)
	for _, key := range []string{"paused", "paused_reason", "catch_up"} {
		if fieldsSet[key] {
			spec.Set(key, body.GetOr(key, nil))
		}
	}
	return mutateCronForProfile(ctx, profileName, func(ctx context.Context, h string) (any, error) {
		return cron.CreateJobWithSchedulerRegistration(ctx, h, spec)
	})
}

// A string list as a value, None when it is empty
func listOrNone(items []any) any {
	if len(items) == 0 {
		return nil
	}
	return items
}

// Fires one due job of a profile through its scheduler provider, forced fires need provider support
func fireCronJobForProfile(ctx context.Context, profile, jobID string, force bool) (bool, error) {
	_, profileHome, err := cronProfileHome(ctx, profile)
	if err != nil {
		return false, err
	}
	scoped := cronStoreScope(ctx, profileHome)
	provider := cron.ResolveCronScheduler(profileHome)
	if force {
		if !cron.ProviderSupportsForceFire(provider) {
			return false, NewHTTPException(409, fmt.Sprintf("Cron provider '%s' does not support atomic forced firing of paused jobs", provider.Name()))
		}
		return cron.SchedulerFireDue(scoped, provider, jobID, cron.FireOptions{Force: true})
	}
	if cron.ProviderFireDueAccepts(provider, "manual") {
		return cron.SchedulerFireDue(scoped, provider, jobID, cron.FireOptions{Manual: true})
	}
	return cron.SchedulerFireDue(scoped, provider, jobID, cron.FireOptions{})
}

// One value of a profile .env, empty when absent or unreadable
func profileEnvValue(profileHome, key string) string {
	return config.LoadEnvFile(filepath.Join(profileHome, ".env"))[key]
}

// Loopback URL of the api_server cron fire route of the gateway that listens for the profile
func gatewayFireEndpoint(ctx context.Context, profile, profileHome string) string {
	multiplex, err := gateway.DefaultGatewayMultiplexes(ctx, "")
	if err != nil {
		webServerLog().Debug("cron fire: multiplex detection failed; assuming single-profile", "exc", err)
		multiplex = false
	}
	listenerProfile, listenerHome := profile, profileHome
	if multiplex && profile != "default" {
		listenerProfile, listenerHome = "default", home.Root()
		webServerLog().Info(fmt.Sprintf("cron fire: multiplex gateway — resolving api_server port for %s from the default profile's listener (%s)", profile, listenerHome))
	}
	port := gatewayFireConfigPort(ctx, listenerHome)
	if port == 0 {
		raw := ""
		if listenerProfile == cronDefaultProfile(ctx) {
			raw = os.Getenv("API_SERVER_PORT")
		} else {
			raw = profileEnvValue(listenerHome, "API_SERVER_PORT")
		}
		if raw != "" {
			if n, ok := pyIntText(raw); ok {
				port = int(n)
			}
		}
	}
	if port == 0 {
		port = 8642
	}
	if multiplex && profile != "default" {
		return fmt.Sprintf("http://127.0.0.1:%d/p/%s/api/cron/fire", port, profile)
	}
	return fmt.Sprintf("http://127.0.0.1:%d/api/cron/fire", port)
}

// platforms.api_server.extra.port of a home config, zero when unset or unreadable
func gatewayFireConfigPort(ctx context.Context, listenerHome string) int {
	cfg, err := webLoadConfig(cronStoreScope(ctx, listenerHome))
	if err != nil {
		return 0
	}
	raw, _ := config.CfgGet(cfg, "platforms", "api_server", "extra", "port")
	if !pyrepr.Truthy(raw) {
		return 0
	}
	n, err := pyint.Of(raw)
	if err != nil || !n.IsInt64() {
		return 0
	}
	return int(n.Int64())
}

// Status and JSON body of the fire forwarded to the gateway api_server, nil when it is unreachable
func forwardCronFireToGateway(ctx context.Context, profile, jobID, authorization string) (*int, *pyjson.Object, error) {
	profileName, profileHome, err := cronProfileHome(ctx, profile)
	if err != nil {
		return nil, nil, err
	}
	url := gatewayFireEndpoint(ctx, profileName, profileHome)
	payload, _ := json.Marshal(map[string]string{"job_id": jobID})
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("cron fire forward to %s failed (%s: %s); returning 503 for NAS retry", url, pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)))
		return nil, nil, nil
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", authorization)
	resp, err := cronFireHTTPClient.Do(req)
	if err != nil {
		webServerLog().Warn(fmt.Sprintf("cron fire forward to %s failed (%s: %s); returning 503 for NAS retry", url, pyrepr.ExceptionName(err), pyrepr.ExceptionMessage(err)))
		return nil, nil, nil
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		webServerLog().Warn(fmt.Sprintf("cron fire forward to %s failed (%s: %s); returning 503 for NAS retry", url, pyrepr.ExceptionName(readErr), pyrepr.ExceptionMessage(readErr)))
		return nil, nil, nil
	}
	status := resp.StatusCode
	decoded, decodeErr := pyjson.LoadsBytes(raw)
	var body *pyjson.Object
	switch {
	case decodeErr != nil:
		body = pyjson.ObjectOf("raw", pystr.Slice(pystr.DecodeUTF8Replace(raw), 0, 500))
	default:
		if obj, ok := decoded.(*pyjson.Object); ok {
			body = obj
		} else {
			body = pyjson.ObjectOf("raw", decoded)
		}
	}
	return &status, body, nil
}

// Client of the loopback cron fire forward
var cronFireHTTPClient = &http.Client{}

// Whether the operator stopped the profile gateway on purpose, false on any read or parse failure
func gatewayIntentionallyStopped(ctx context.Context, profile string) bool {
	_, profileHome, err := cronProfileHome(ctx, profile)
	if err != nil {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(profileHome, "gateway_state.json"))
	if err != nil {
		return false
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return false
	}
	data, err := pyjson.LoadsBytes([]byte(text))
	if err != nil {
		return false
	}
	obj, ok := data.(*pyjson.Object)
	return ok && obj.GetOr("desired_state", nil) == "stopped"
}
