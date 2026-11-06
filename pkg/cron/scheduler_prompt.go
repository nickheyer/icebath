package cron

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/skills"
)

const (
	maxContextChars     = 8000
	selfContextIntro    = "The following is this job's most recent non-silent output from a previous run. Use it for continuity: avoid repeating what was already reported, and continue where the last run left off."
	upstreamContextIntr = "The following is the most recent output from a preceding cron job. Use it as context for your analysis."
	promptFrame         = "**Prompt Characters:** "
	responseFrame       = "**Response Characters:** "
	promptHeading       = "## Prompt\n\n"
	responseHeading     = "## Response\n\n"
	promptSeparator     = "\n\n"
	responseTerminator  = "\n"
)

var (
	promptFrameRE   = mustCompileCronRE(`(?m)^\*\*Prompt\ Characters:\*\*\ (\d+)\n\#\#\ Prompt\n\n`)
	responseFrameRE = mustCompileCronRE(`(?m)^\*\*Response\ Characters:\*\*\ (\d+)\n\#\#\ Response\n\n`)
)

const cronHint = "[IMPORTANT: You are running as a scheduled cron job. " +
	"DELIVERY: Your final response will be automatically delivered " +
	"to the user \u2014 do NOT use send_message or try to deliver " +
	"the output yourself. Just produce your report/output as your " +
	"final response and the system handles the rest. " +
	"SILENT: If there is genuinely nothing new to report, respond " +
	"with exactly \"[SILENT]\" (nothing else) to suppress delivery. " +
	"[SILENT] is a literal ASCII control token \u2014 never translate or " +
	"rephrase it, whatever language the rest of your answer uses. " +
	"Never combine [SILENT] with content \u2014 either report your " +
	"findings normally, or say [SILENT] and nothing more. " +
	"FAILURE: If a delegated child fails and this cron run must be " +
	"recorded as failed, put [CRON_FAILURE] on the first line by itself, " +
	"then explain the child failure on following lines. " +
	"RECURSION: This is a run of an EXISTING scheduled job \u2014 execute " +
	"the task now. NEVER create or update a cron job because of " +
	"recurring or future-schedule language in the task prompt below; " +
	"treat phrasing like \"each Monday\" or \"every day at 9\" as " +
	"context for this run, not as a request to schedule another job.]\n\n"

// Seam over the cronjob_job_args base_url validator so tests can drive both outcomes
var cronBaseURLValidator = validateCronBaseURL

// Cached outcome of a pre run script the caller already executed for the wake gate
type scriptRun struct {
	Success bool
	Output  string
}

// Optional inputs of one prompt build beyond the stored job
type buildPromptOptions struct {
	Prerun            *scriptRun
	ExtraPrompt       string
	RuntimeDataPrompt string
}

// The four values a run returns, success flag, run document, final response and error text
type jobRunOutcome struct {
	Success       bool
	Output        string
	FinalResponse string
	Error         *string
}

// False only when the last non blank stdout line is a JSON object whose wakeAgent is exactly false
func parseWakeGate(scriptOutput string) bool {
	var lines []string
	for _, line := range pystr.Splitlines(scriptOutput, false) {
		if pystr.Strip(line) != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return true
	}
	gate, err := pyjson.Loads(pystr.Strip(lines[len(lines)-1]))
	if err != nil {
		return true
	}
	obj, ok := gate.(*pyjson.Object)
	if !ok {
		return true
	}
	v, present := obj.Get("wakeAgent")
	return !present || v != false
}

// Prefixes the prompt with a fenced data block under its own heading
func prependContextBlock(prompt, heading, intro, body string) string {
	return fmt.Sprintf("## %s\n%s\n\n```\n%s\n```\n\n%s", heading, intro, body, prompt)
}

// Stripped non empty skill names from skills or the legacy singular skill field
func jobSkillNames(job *Job) []string {
	names := job.Skills
	if names == nil {
		if job.Skill != nil && *job.Skill != "" {
			names = []string{*job.Skill}
		}
	}
	out := []string{}
	for _, name := range names {
		if v := pystr.Strip(name); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// Markdown header shared by the no agent and monitor run documents
func jobDocHeader(jobName, jobID, nowISO, mode string) string {
	return fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n**Mode:** %s\n", jobName, jobID, nowISO, mode)
}

// Code point index of the first occurrence of sub, minus one when absent
func runeIndex(s, sub string) int {
	i := strings.Index(s, sub)
	if i < 0 {
		return -1
	}
	return utf8.RuneCountInString(s[:i])
}

// Python match at a code point position, honouring a multiline caret against the preceding character
func matchAt(re *regexp2.Regexp, runes []rune, pos int) *regexp2.Match {
	if pos > len(runes) || (pos > 0 && runes[pos-1] != '\n') {
		return nil
	}
	m, err := re.FindRunesMatchStartingAt(runes, pos)
	if err != nil {
		panic(err)
	}
	if m == nil || m.RuneIndex != pos {
		return nil
	}
	return m
}

// Frame length as an int, false when it overflows every index the document could hold
func frameLength(m *regexp2.Match) (int, bool) {
	n, ok := pyint.Parse(m.GroupByNumber(1).String())
	if !ok || !n.IsInt64() || n.Int64() > int64(1<<40) {
		return 0, false
	}
	return int(n.Int64()), true
}

// Reusable answer of a stored run through its length frames, nil when blank, silent or truncated
func archiveAnswer(archive string) *string {
	runes := []rune(archive)
	var answer string
	pf, err := promptFrameRE.FindRunesMatch(runes)
	if err != nil {
		panic(err)
	}
	if pf != nil && runeIndex(archive, promptHeading) == pf.RuneIndex+pf.RuneLength-utf8.RuneCountInString(promptHeading) {
		plen, ok := frameLength(pf)
		if !ok {
			return nil
		}
		start := pf.RuneIndex + pf.RuneLength + plen + utf8.RuneCountInString(promptSeparator)
		frame := matchAt(responseFrameRE, runes, start)
		if frame == nil {
			return nil
		}
		tail := runes[frame.RuneIndex+frame.RuneLength:]
		rlen, ok := frameLength(frame)
		if !ok || len(tail) != rlen+1 || tail[len(tail)-1] != '\n' {
			return nil
		}
		answer = pystr.Strip(string(tail[:len(tail)-1]))
	} else if !strings.Contains(archive, "## Response") {
		v := pystr.Strip(archive)
		return &v
	} else {
		answer = pystr.Strip(archive[strings.LastIndex(archive, "## Response")+len("## Response"):])
	}
	if answer == "" || isCronSilenceResponse(answer) {
		return nil
	}
	return &answer
}

// Keeps the head and tail of oversized context with an omission marker between them
func clipToContextBudget(text string) string {
	runes := []rune(text)
	if len(runes) <= maxContextChars {
		return text
	}
	keep := maxContextChars / 2
	omitted := len(runes) - 2*keep
	return fmt.Sprintf("%s\n\n[... %d chars omitted ...]\n\n%s", string(runes[:keep]), omitted, string(runes[len(runes)-keep:]))
}

// Secret free origin provenance fields appended to warnings about a bad stored reference
func cronJobOriginLogSuffix(job *Job) string {
	if job.Origin == nil {
		return ""
	}
	var fields []string
	for _, key := range []string{"platform", "chat_id", "thread_id", "source_ip", "remote", "forwarded_for"} {
		value, ok := job.Origin[key]
		if !ok || value == nil {
			continue
		}
		text := pystr.Strip(strings.ReplaceAll(strings.ReplaceAll(config.PyStr(value), "\r", " "), "\n", " "))
		if text != "" {
			r := []rune(text)
			if len(r) > 200 {
				r = r[:200]
			}
			fields = append(fields, fmt.Sprintf("origin_%s=%s", key, pyrepr.StrRepr(string(r))))
		}
	}
	if len(fields) == 0 {
		return ""
	}
	return " " + strings.Join(fields, " ")
}

// Configured job workdir, nil when unset or no longer a directory
func (s *Store) resolveJobWorkdir(job *Job, jobID string) *string {
	if job.Workdir == nil {
		return nil
	}
	workdir := pystr.Strip(*job.Workdir)
	if workdir == "" {
		return nil
	}
	if st, err := os.Stat(workdir); err != nil || !st.IsDir() {
		s.log.Warn(fmt.Sprintf("Job '%s': configured workdir %s no longer exists \u2014 running without it", jobID, pyrepr.StrRepr(workdir)))
		return nil
	}
	return &workdir
}

// Whether a run document is a suppressed audit whose header records no change or a closed wake gate
func silentAuditDocument(candidate string) bool {
	if !strings.HasPrefix(candidate, "# Cron Job:") {
		return false
	}
	header, _, _ := strings.Cut(candidate, "\n---\n")
	header, _, _ = strings.Cut(header, "\n## Prompt")
	for _, line := range pystr.Splitlines(header, false) {
		if strings.HasPrefix(line, "**Status:** no_change") || strings.HasPrefix(line, "**Status:** silent") || strings.HasPrefix(line, "Script gate returned `wakeAgent=false`") {
			return true
		}
	}
	return false
}

// Newest usable answer among a job's run documents, empty when none, error only for undecodable text
func latestContextOutput(dir string) (string, error) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.md"))
	type stamped struct {
		path  string
		mtime int64
	}
	files := make([]stamped, 0, len(paths))
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return "", &os.PathError{Op: "stat", Path: p, Err: err}
		}
		files = append(files, stamped{p, st.ModTime().UnixNano()})
	}
	sort.SliceStable(files, func(i, j int) bool { return files[i].mtime > files[j].mtime })
	for _, f := range files {
		raw, err := os.ReadFile(f.path)
		if err != nil {
			return "", err
		}
		text, err := pystr.DecodeUTF8Sig(raw)
		if err != nil {
			return "", &contextDecodeError{err}
		}
		candidate := pystr.TranslateNewlines(text)
		if pystr.Strip(candidate) == "" || silentAuditDocument(candidate) {
			continue
		}
		if answer := archiveAnswer(candidate); answer != nil {
			return *answer, nil
		}
	}
	return "", nil
}

// Undecodable run document text, which propagates out of the prompt build like UnicodeDecodeError
type contextDecodeError struct{ err error }

func (e *contextDecodeError) Error() string { return e.err.Error() }
func (e *contextDecodeError) Unwrap() error { return e.err }

// Prepends the latest output of every context_from job, reporting whether anything was injected
func (s *Store) injectContextFrom(job *Job, prompt string) (string, bool, error) {
	if len(job.ContextFrom) == 0 {
		return prompt, false, nil
	}
	injected := false
	for _, sourceID := range job.ContextFrom {
		if pystr.Lower(pystr.Strip(sourceID)) == "self" {
			sourceID = job.ID
		}
		isSelf := sourceID == job.ID
		if sourceID == "" || strings.Trim(sourceID, "0123456789abcdef") != "" {
			s.log.Warn(fmt.Sprintf("context_from: skipping invalid job_id %s for job_id=%s name=%s%s", pyrepr.StrRepr(sourceID), pyrepr.StrRepr(job.ID), pyrepr.StrRepr(job.Name), cronJobOriginLogSuffix(job)))
			continue
		}
		latest, err := latestContextOutput(filepath.Join(s.outputDir, sourceID))
		if err != nil {
			var dec *contextDecodeError
			if errors.As(err, &dec) {
				return prompt, injected, dec.err
			}
			s.log.Warn(fmt.Sprintf("context_from: failed to read output for job %s: %v", pyrepr.StrRepr(sourceID), err))
			continue
		}
		if latest == "" {
			continue
		}
		latest = clipToContextBudget(latest)
		if isSelf {
			prompt = prependContextBlock(prompt, "Your previous run's output", selfContextIntro, latest)
		} else {
			prompt = prependContextBlock(prompt, fmt.Sprintf("Output from job '%s'", sourceID), upstreamContextIntr, latest)
		}
		injected = true
	}
	return prompt, injected, nil
}

// Loads each named skill or bundle into prompt parts, unknown ones skipped behind a leading notice
func (s *Store) loadCronSkillParts(ctx context.Context, job *Job, skillNames []string) ([]string, error) {
	cfg, _ := s.loadConfig()
	scope := skills.NewScope(s.home, cfg)
	jobLabel := job.Name
	taskID := job.ID
	parts := []string{}
	var skipped []string
	skip := func(label, msg string) {
		s.log.Warn(fmt.Sprintf("Cron job '%s': %s", jobLabel, msg))
		skipped = append(skipped, label)
	}
	for _, name := range skillNames {
		bundleKey, isBundle, err := scope.ResolveBundleCommandKey(strings.TrimLeft(name, "/"))
		if err != nil {
			return nil, err
		}
		if isBundle && bundleKey != "" {
			msg, _, _, ok, err := scope.BuildBundleInvocationMessage(ctx, bundleKey, "", taskID, "")
			if err != nil {
				return nil, err
			}
			if ok {
				if len(parts) > 0 {
					parts = append(parts, "")
				}
				parts = append(parts, msg)
			} else {
				skip(name, fmt.Sprintf("bundle '%s' could not load any skills, skipping", name))
			}
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(scope.SkillView(ctx, scope.NormalizeSkillLookupName(name), "", "", true)), &decoded); err != nil {
			skip(name, fmt.Sprintf("skill '%s' returned invalid JSON, skipping", name))
			continue
		}
		loaded, ok := decoded.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(decoded))
		}
		if ambiguous, ok := skills.AmbiguousSkillLabel(name, loaded); ok {
			skip(ambiguous, ambiguous+" \u2014 skipping")
			continue
		}
		if !config.PyTruthy(loaded["success"]) {
			reason := fmt.Sprintf("Failed to load skill '%s'", name)
			if config.PyTruthy(loaded["error"]) {
				reason = config.PyStr(loaded["error"])
			}
			skip(name, "skill not found, skipping \u2014 "+reason)
			continue
		}
		scope.BumpUse(ctx, name, taskID, "")
		if len(parts) > 0 {
			parts = append(parts, "")
		}
		content := ""
		if config.PyTruthy(loaded["content"]) {
			content = config.PyStr(loaded["content"])
		}
		parts = append(parts, fmt.Sprintf("[IMPORTANT: The user has invoked the \"%s\" skill, indicating they want you to follow its instructions. The full skill content is loaded below.]", name), "", pystr.Strip(content))
		scope.InjectSkillConfig(loaded, &parts)
	}
	if len(skipped) > 0 {
		joined := strings.Join(skipped, ", ")
		notice := fmt.Sprintf("[IMPORTANT: The following skill(s) were listed for this job but could not be found and were skipped: %s. Start your response with a brief notice so the user is aware, e.g.: '\u26a0\ufe0f Skill(s) not found and skipped: %s']", joined, joined)
		parts = append([]string{notice}, parts...)
	}
	return parts, nil
}

// Python type name of a decoded JSON value for the AttributeError text
func pyTypeName(v any) string {
	switch v.(type) {
	case []any:
		return "list"
	case string:
		return "str"
	case float64:
		return "float"
	case bool:
		return "bool"
	case nil:
		return "NoneType"
	}
	return "dict"
}

// Wall clock line of this fire in the configured zone with its UTC offset
func (s *Store) runTimeLine() string {
	now := s.clk.now()
	offset := config.SafeStrftime(now, "%z")
	zone := ""
	if offset != "" {
		zone = fmt.Sprintf(" (UTC%s:%s)", offset[:3], offset[3:])
	}
	return fmt.Sprintf("[CURRENT RUN TIME: %s%s]\n\n", pystr.Strip(config.SafeStrftime(now, "%A, %Y-%m-%d %H:%M %Z")), zone)
}

// Effective prompt of a run with script output, context, notepad and skills, nil when the script printed nothing
func (s *Store) buildJobPrompt(ctx context.Context, job *Job, opts buildPromptOptions) (*string, error) {
	userPrompt := job.Prompt
	if opts.ExtraPrompt != "" {
		userPrompt = fmt.Sprintf("%s\n\n## Run Context\n%s", userPrompt, opts.ExtraPrompt)
	}
	prompt := userPrompt
	hasInjectedData := false
	if opts.RuntimeDataPrompt != "" {
		prompt = fmt.Sprintf("%s\n\n## Run Context\n%s", prompt, opts.RuntimeDataPrompt)
		hasInjectedData = true
	}
	if job.Script != nil && *job.Script != "" {
		run := opts.Prerun
		if run == nil {
			workdir := ""
			if w := s.resolveJobWorkdir(job, job.ID); w != nil {
				workdir = *w
			}
			ok, out, err := s.runJobScript(ctx, *job.Script, workdir, nil, job.Interpreter)
			if err != nil {
				return nil, err
			}
			run = &scriptRun{Success: ok, Output: out}
		}
		if run.Success && run.Output == "" {
			return nil, nil
		}
		heading, intro := "Script Output", "The following data was collected by a pre-run script. Use it as context for your analysis."
		if !run.Success {
			heading, intro = "Script Error", "The data-collection script failed. Report this to the user."
		}
		prompt = prependContextBlock(prompt, heading, intro, run.Output)
		hasInjectedData = true
	}
	prompt, ctxInjected, err := s.injectContextFrom(job, prompt)
	if err != nil {
		return nil, err
	}
	hasInjectedData = hasInjectedData || ctxInjected
	if section := openNotepad(s).RenderSection(job.ID); section != "" {
		prompt = section + prompt
		hasInjectedData = true
	}
	prompt = cronHint + s.runTimeLine() + prompt
	names := jobSkillNames(job)
	if len(names) == 0 {
		out, err := s.scanAssembledCronPrompt(prompt, job, false, hasInjectedData, userPrompt)
		if err != nil {
			return nil, err
		}
		return &out, nil
	}
	parts, err := s.loadCronSkillParts(ctx, job, names)
	if err != nil {
		return nil, err
	}
	stablePrefix := ""
	if prompt != "" {
		parts = append(parts, "")
		stablePrefix = skills.AppendUserInstruction(&parts, prompt)
	}
	assembled, err := s.scanAssembledCronPrompt(strings.Join(parts, "\n"), job, true, false, "")
	if err != nil {
		return nil, err
	}
	if stablePrefix != "" && len(assembled) > len(stablePrefix) && strings.HasPrefix(assembled, stablePrefix) {
		provider.RegisterStablePrefix(stablePrefix)
	}
	return &assembled, nil
}

// Injection scan of the assembled prompt, strict for user text and loose once skills or data are present
func (s *Store) scanAssembledCronPrompt(assembled string, job *Job, hasSkills, hasInjectedData bool, userPrompt string) (string, error) {
	var scanErr string
	if hasSkills || hasInjectedData {
		assembled, scanErr = scanCronSkillAssembled(assembled)
		if scanErr == "" && !hasSkills && userPrompt != "" {
			scanErr = scanCronPrompt(userPrompt)
		}
	} else {
		scanErr = scanCronPrompt(assembled)
	}
	if scanErr != "" {
		label := job.Name
		if label == "" {
			label = job.ID
		}
		if label == "" {
			label = "<unknown>"
		}
		s.log.Warn(fmt.Sprintf("Cron job '%s': assembled prompt blocked by injection scanner \u2014 %s", label, scanErr))
		return "", &CronPromptInjectionBlocked{Msg: scanErr}
	}
	return assembled, nil
}

// Refuses a job whose stored provider and base_url pair could send a stored key off origin
func (s *Store) guardJobCredentialExfil(job *Job) error {
	msg, err := cronBaseURLValidator(job.Provider, job.BaseURL)
	if err != nil {
		msg = ""
		if job.BaseURL != nil && *job.BaseURL != "" {
			msg = fmt.Sprintf("could not validate provider/base_url pair (%s: %v); refusing to run a job with an unverified base_url override", cronExceptionName(err), err)
		}
	}
	if msg == "" {
		return nil
	}
	s.log.Error(fmt.Sprintf("Job '%s': refusing to run \u2014 unsafe provider/base_url pair could exfiltrate a stored credential: %s", job.ID, msg))
	return fmt.Errorf("Cron job '%s' blocked for safety: %s", job.ID, msg)
}

// Fails a run closed and pauses the job so an unrunnable job cannot refire every tick
func (s *Store) blockAndPauseJob(jobID, jobName, reason string) jobRunOutcome {
	s.log.Error(fmt.Sprintf("Job '%s': %s", jobID, reason))
	pauseReason := "Auto-paused by scheduler: " + reason
	if _, err := s.pauseJob(jobID, &pauseReason); err != nil {
		s.log.Error(fmt.Sprintf("Job '%s': failed to auto-pause unrunnable job: %v", jobID, err))
	}
	nowISO := config.SafeStrftime(s.clk.now(), "%Y-%m-%d %H:%M:%S")
	doc := fmt.Sprintf("# Cron Job: %s\n\n**Job ID:** %s\n**Run Time:** %s\n**Status:** blocked (unrunnable job) \u2014 auto-paused\n\n%s\n", jobName, jobID, nowISO, reason)
	alert := fmt.Sprintf("\u26a0 Cron job '%s' was auto-paused\n\n%s", jobName, reason)
	return jobRunOutcome{Success: false, Output: doc, FinalResponse: alert, Error: &reason}
}
