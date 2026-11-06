package cron

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Session store a finished cron session's title is written through
type cronTitleDB interface {
	SetSessionTitle(ctx context.Context, sessionID, title string) (bool, error)
}

// Session store that can suffix a duplicate title with the next free lineage number
type cronTitleLineageDB interface {
	GetNextTitleInLineage(ctx context.Context, baseTitle string) (string, error)
}

// Persists a non blank unique title for a finished cron session, empty when nothing was written
func setCronSessionTitle(ctx context.Context, db cronTitleDB, sessionID, baseTitle string) (string, error) {
	if db == nil || sessionID == "" {
		return "", nil
	}
	title := pystr.Strip(baseTitle)
	if title == "" {
		return "", nil
	}
	_, err := db.SetSessionTitle(ctx, sessionID, title)
	if err == nil {
		return title, nil
	}
	var pe *pyint.Error
	if !errors.As(err, &pe) || pe.Kind != "ValueError" {
		return "", err
	}
	lineage, ok := db.(cronTitleLineageDB)
	if !ok {
		return "", err
	}
	deduped, lerr := lineage.GetNextTitleInLineage(ctx, title)
	if lerr != nil {
		return "", lerr
	}
	if deduped == "" || deduped == title {
		return "", err
	}
	if _, err := db.SetSessionTitle(ctx, sessionID, deduped); err != nil {
		return "", err
	}
	return deduped, nil
}

// True when the job carries its own provider, model or endpoint
func jobRoutePinned(job *Job) bool {
	for _, v := range []*string{job.Provider, job.Model, job.BaseURL} {
		if v != nil && pystr.Strip(*v) != "" {
			return true
		}
	}
	return false
}

// Fallback chain the job may walk, empty for a pinned job that never borrows the global chain
func jobFallbackChain(job *Job, cfg config.Tree) []map[string]any {
	return provider.ScopedFallbackChain(provider.GetFallbackChain(cfg), nil, false, jobRoutePinned(job), "cron job")
}

// Backup provider clause for a provider failure notice, failing open when config cannot be read
func (s *Store) fallbackChainPhrase(job *Job) string {
	if job != nil && jobRoutePinned(job) {
		return "This job is pinned to its own provider/model, so it does not fall back to " +
			fmt.Sprintf("`fallback_providers`; `icebath cron edit %s --unpin` lets it follow the ", job.ID) +
			"main model and its fallback chain."
	}
	chain, ok := func() (chain []map[string]any, ok bool) {
		defer func() {
			if recover() != nil {
				ok = false
			}
		}()
		cfg, err := s.loadConfig()
		if err != nil {
			return nil, false
		}
		return provider.GetFallbackChain(cfg), true
	}()
	if !ok || len(chain) > 0 {
		return "No backup provider succeeded either."
	}
	return "No backup provider is configured — add one with `icebath fallback add`, " +
		"or set a cron-wide default via `cron.model` + `cron.model_provider` in config.yaml."
}

// Review nudge when a recurring job keeps failing, counting this run, empty below the threshold
func (s *Store) failureStreakNudge(job *Job) string {
	if job.Schedule == nil || (job.Schedule.Kind != "cron" && job.Schedule.Kind != "interval") {
		return ""
	}
	threshold := int64(3)
	func() {
		defer func() {
			if recover() != nil {
				threshold = 3
			}
		}()
		cfg, err := s.loadConfig()
		if err != nil {
			return
		}
		var raw any = int64(3)
		if cron, ok := cfg["cron"].(map[string]any); ok {
			if v, present := cron["failure_nudge_threshold"]; present {
				raw = v
			}
		}
		n, err := pyint.Of(raw)
		if err != nil {
			return
		}
		switch {
		case n.IsInt64():
			threshold = n.Int64()
		case n.Sign() > 0:
			threshold = math.MaxInt64
		default:
			threshold = -1
		}
	}()
	if threshold <= 0 {
		return ""
	}
	streak := int64(job.FailureStreak) + 1
	if streak < threshold {
		return ""
	}
	ref := job.Name
	if ref == "" {
		ref = job.ID
	}
	if ref == "" {
		ref = "this job"
	}
	return fmt.Sprintf("\nThis job has failed %d runs in a row — worth a review. ", streak) +
		fmt.Sprintf("Fix its prompt/config, or pause it with `icebath cron pause %s` ", ref) +
		"(resume/remove also available) to stop the noise."
}

// Boot versus disk revision skew probe a test replaces, the gateway detector by default
var detectGatewayCodeSkew = func() (bootRev, diskRev string, ok bool) {
	defer func() {
		if recover() != nil {
			bootRev, diskRev, ok = "", "", false
		}
	}()
	return gateway.DetectCodeSkew()
}

// Full revision on disk a test replaces, false when it cannot be resolved
var currentGatewayCodeSHA = func() (sha string, ok bool) {
	defer func() {
		if recover() != nil {
			sha, ok = "", false
		}
	}()
	return gateway.CurrentCodeSHA()
}

// CronTickYielded reports a stale code ticker that yielded its tick to a fresh gateway
type CronTickYielded struct {
	BootRev string
	DiskRev string
}

func (e *CronTickYielded) Error() string {
	return fmt.Sprintf("Cron tick yielded to a fresh gateway process (stale code: booted on %s, disk is at %s)", e.BootRev, e.DiskRev)
}

var staleYieldRe = mustPyRe(`stale code: booted on (\S+), disk is at (\S+)\)`)

// Boot and disk revisions from a persisted ticker error that is a tick yield, false otherwise
func staleCodeYieldLabels(recordedError string) (bootRev, diskRev string, ok bool) {
	if recordedError == "" || !strings.HasPrefix(recordedError, "CronTickYielded") {
		return "", "", false
	}
	m, err := staleYieldRe.FindStringMatch(recordedError)
	if err != nil || m == nil {
		return "", "", false
	}
	return m.GroupByNumber(1).String(), m.GroupByNumber(2).String(), true
}

// Revisions when this profile's tick must yield to a fresher gateway serving it, false on any doubt
func (s *Store) shouldYieldTickToFreshGateway() (bootRev, diskRev string, ok bool) {
	bootRev, diskRev, ok = detectGatewayCodeSkew()
	if !ok {
		return "", "", false
	}
	diskSHA, ok := currentGatewayCodeSHA()
	if !ok {
		return "", "", false
	}
	yield := func() (yield bool) {
		defer func() {
			if recover() != nil {
				yield = false
			}
		}()
		if ownsCronTickFor(s.home) {
			return false
		}
		status := liveGatewayTicking(s.home)
		if status == nil {
			return false
		}
		sha, present := status.Get("code_sha")
		got, isStr := sha.(string)
		return present && isStr && got == diskSHA
	}()
	if !yield {
		return "", "", false
	}
	return bootRev, diskRev, true
}

var (
	idleLimitRe      = mustPyRe(`idle for \d+s\s*\(limit \d+s\)`)
	leadingExcNameRe = mustPyRe(`^(RuntimeError|Exception|ValueError|HTTPStatusError):\s*`)
	whitespaceRunRe  = mustPyRe(`\s+`)
	importErrorRe    = mustPyRe(`cannot import name|modulenotfounderror|importerror`)
)

// One line failure notice for the job's chat, deterministic script shapes first then the classifier
func (s *Store) summarizeCronFailureForDelivery(job *Job, errText *string) string {
	name := job.Name
	if name == "" {
		name = job.ID
	}
	if name == "" {
		name = "cron job"
	}
	id := job.ID
	if id == "" {
		id = name
	}
	text := "unknown error"
	if errText != nil && *errText != "" {
		text = *errText
	}
	text = pystr.Strip(text)
	lower := pystr.Lower(text)
	if strings.HasPrefix(lower, "script timed out") {
		return scriptTimeoutNotice(s.home, name, id)
	}
	if ok, _ := idleLimitRe.MatchString(lower); ok {
		return inactivityNotice(s.home, name, id)
	}
	if !job.NoAgent {
		var prov any
		if job.Provider != nil {
			prov = *job.Provider
		}
		if notice, ok := providerFailureNotice(s.home, name, id, classifyCronFailureReason(text), s.fallbackChainPhrase(job), prov); ok {
			return notice
		}
	}
	runes := []rune(text)
	if len(runes) > 2000 {
		runes = runes[:2000]
	}
	cleaned, _ := leadingExcNameRe.Replace(string(runes), "", -1, -1)
	collapsed, _ := whitespaceRunRe.Replace(cleaned, " ", -1, -1)
	cleaned = strings.TrimRight(pystr.Strip(collapsed), ".")
	if cr := []rune(cleaned); len(cr) > 180 {
		cleaned = pystr.Rstrip(string(cr[:177])) + "..."
	}
	message := genericFailureNotice(s.home, name, id, cleaned)
	if isImport, _ := importErrorRe.MatchString(lower); !job.NoAgent && isImport {
		if bootRev, diskRev, ok := detectGatewayCodeSkew(); ok {
			message += fmt.Sprintf(" Likely cause: the gateway is running stale code (booted "+
				"on %s, disk is at %s) — run `icebath gateway restart` to fix it.", bootRev, diskRev)
		}
	}
	return message
}

const defaultFailureRepeatAlertHours = 6.0

// Hours an alerted incident stays quiet before a reminder ping, zero or less re alerts every run
func (s *Store) failureRepeatAlertHours() float64 {
	return s.cronConfigFloat("failure_repeat_alert_hours", defaultFailureRepeatAlertHours)
}

// True when an alerted incident pinged inside the repeat alert cooldown
func (s *Store) repeatAlertWithheld(incident *Incident) bool {
	hours := s.failureRepeatAlertHours()
	if hours <= 0 || incident.AlertedAt == nil || *incident.AlertedAt == "" {
		return false
	}
	parsed, err := ParseISO(*incident.AlertedAt)
	if err != nil {
		return false
	}
	last := s.clk.ensureAware(parsed, nil)
	return elapsedSeconds(s.clk.now(), last) < hours*3600
}

// Upserts the failure incident and reports whether the per run ping is withheld
func (s *Store) upsertIncidentForFailure(job *Job, errText string, outputFile *string) (withheld bool, incidentID *string) {
	var name *string
	if job.Name != "" {
		name = &job.Name
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		store := openIncidents(s)
		id, _, err := store.UpsertIncident(job.ID, errText, name, outputFile)
		if err != nil {
			return err
		}
		incident, err := store.GetIncident(id)
		if err != nil {
			return err
		}
		state := ""
		if incident != nil {
			state = incident.State
		}
		withheld = state == "closed" || (state == "alerted" && s.repeatAlertWithheld(incident))
		incidentID = &id
		return nil
	}()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Incident store unavailable for job %s (delivery unaffected): %v", job.ID, err))
		return false, nil
	}
	return withheld, incidentID
}

// Closes the job's open incidents after a clean run, delivery unaffected by store errors
func (s *Store) resolveIncidentsForRecoveredJob(job *Job) {
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		_, err = openIncidents(s).CloseIncidentsForRecoveredJob(job.ID)
		return err
	}()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Incident store unavailable for job %s (delivery unaffected): %v", job.ID, err))
	}
}

// Marks the incident alerted after its ping went out
func (s *Store) markIncidentAlerted(incidentID *string) {
	if incidentID == nil || *incidentID == "" {
		return
	}
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		_, err = openIncidents(s).SetIncidentState(*incidentID, "alerted")
		return err
	}()
	if err != nil {
		s.log.Debug(fmt.Sprintf("Failed marking incident %s alerted: %v", *incidentID, err))
	}
}

// CronPromptInjectionBlocked reports an assembled prompt that tripped the injection scanner at run time
type CronPromptInjectionBlocked struct{ Msg string }

func (e *CronPromptInjectionBlocked) Error() string { return e.Msg }

// Toolsets a cron agent never gets plus the user's agent.disabled_toolsets
func resolveCronDisabledToolsets(cfg config.Tree) []string {
	cronCfg, _ := cfg["cron"].(map[string]any)
	disabled := []string{"cronjob", "messaging", "clarify"}
	if pyrepr.Truthy(cronCfg["allow_agent_scheduling"]) {
		disabled = []string{"messaging", "clarify"}
	}
	agentCfg, _ := cfg["agent"].(map[string]any)
	for _, name := range tools.ParseConfigStringList(agentCfg["disabled_toolsets"]) {
		name = pystr.Strip(name)
		if name != "" && !containsString(disabled, name) {
			disabled = append(disabled, name)
		}
	}
	return disabled
}

// Per job toolsets with every enabled MCP server added unless the job names one or opts out
func mergeMCPIntoPerJobToolsets(ctx context.Context, perJob []string, cfg config.Tree) []string {
	result := []string{}
	for _, t := range perJob {
		if t != "no_mcp" {
			result = append(result, t)
		}
	}
	if containsString(perJob, "no_mcp") {
		return result
	}
	enabled := tools.EnabledMCPServerNames(cfg, plugins.GetPortableMCPServerNamesNowait(ctx))
	known := map[string]bool{}
	for n := range enabled {
		known[n] = true
	}
	if configured, ok := cfg["mcp_servers"].(map[string]any); ok {
		for n := range configured {
			known[n] = true
		}
	}
	for _, t := range result {
		if known[t] || (strings.HasPrefix(t, "mcp-") && known[strings.TrimPrefix(t, "mcp-")]) {
			return result
		}
	}
	names := make([]string, 0, len(enabled))
	for n := range enabled {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !containsString(result, n) {
			result = append(result, n)
		}
	}
	return result
}

// Toolsets a cron run gets, refusing the run when platform resolution fails rather than granting everything
func resolveCronEnabledToolsets(ctx context.Context, job *Job, cfg config.Tree) (out []string, err error) {
	if job.EnabledToolsets != nil {
		if len(job.EnabledToolsets) == 0 {
			return []string{}, nil
		}
		return mergeMCPIntoPerJobToolsets(ctx, append([]string(nil), job.EnabledToolsets...), cfg), nil
	}
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, fmt.Errorf("Cron toolset resolution failed, so this run was refused rather than given every "+
				"tool. Check `platform_toolsets.cron` in config.yaml (`icebath cron doctor`): %v", r)
		}
	}()
	if cfg == nil {
		cfg = config.Tree{}
	}
	if pt := cfg["platform_toolsets"]; pyrepr.Truthy(pt) {
		if _, ok := pt.(map[string]any); !ok {
			return nil, fmt.Errorf("Cron toolset resolution failed, so this run was refused rather than given every "+
				"tool. Check `platform_toolsets.cron` in config.yaml (`icebath cron doctor`): '%s' object has no attribute 'get'", pyrepr.TypeName(pt))
		}
	}
	enabled := tools.Default.PlatformTools(ctx, "", cfg, "cron", tools.PlatformToolsOptions{
		PluginToolsetKeys:        plugins.GetPluginToolsetKeysNowait(ctx),
		PortableMCPServers:       plugins.GetPortableMCPServerNamesNowait(ctx),
		IncludeDefaultMCPServers: true,
	})
	out = make([]string, 0, len(enabled))
	for n := range enabled {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// Reasoning config for the run, a valid per job pin first then config resolution
func (s *Store) resolveJobReasoningConfig(job *Job, cfg config.Tree, model string) map[string]any {
	if job.ReasoningEffort != nil {
		pinned := *job.ReasoningEffort
		if parsed := config.ParseReasoningEffort(pinned); parsed != nil {
			s.log.Info(fmt.Sprintf("Job '%s': using per-job reasoning_effort '%s'", jobIDOrUnknown(job), pinned))
			return parsed
		}
		s.log.Warn(fmt.Sprintf("Job '%s': invalid stored reasoning_effort %s — ignoring the pin "+
			"and falling back to config resolution. Fix with `cronjob "+
			"action=update job_id=%s reasoning_effort=<level>` (valid: none, "+
			"minimal, low, medium, high, xhigh, max, ultra).", jobIDOrUnknown(job), pyrepr.StrRepr(pinned), jobIDOrUnknown(job)))
	}
	var tree any = map[string]any(cfg)
	if cfg == nil {
		tree = map[string]any{}
	}
	return config.ResolveReasoningConfig(tree, model)
}

// Job id or a question mark for log lines
func jobIDOrUnknown(job *Job) string {
	if job.ID == "" {
		return "?"
	}
	return job.ID
}

const (
	SilentMarker      = "[SILENT]"
	CronFailureMarker = "[CRON_FAILURE]"
)

// Evidence of a failure the agent reported with the first line marker, false for any other response
func cronFailureMarkerError(text string) (string, bool) {
	lines := pystr.Splitlines(text, false)
	if len(lines) == 0 || pystr.Rstrip(lines[0]) != CronFailureMarker {
		return "", false
	}
	evidence := pystr.Strip(strings.Join(lines[1:], "\n"))
	if evidence == "" {
		evidence = "Cron agent reported failure."
	}
	return evidence, true
}

// True when the response is an autonomous lane silence marker
func isCronSilenceResponse(text string) bool {
	return gateway.IsAutonomousSilenceResponse(text)
}
