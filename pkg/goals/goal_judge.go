package goals

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/state"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	DefaultJudgeTimeout                    = 30.0
	DefaultJudgeMaxTokens                  = 4096
	judgeResponseSnippetChars              = 4000
	DefaultMaxConsecutiveParseFailures     = 3
	DefaultMaxConsecutiveTransportFailures = 5
	maxBarrierWaitS                        = 30 * 60
	gateOutputTailChars                    = 3000
	goalRepasteMinChars                    = 400
	goalRepasteMinShare                    = 0.8
)

var goalContractAliases = map[string]string{
	"outcome": "outcome", "goal": "outcome", "done": "outcome", "done when": "outcome",
	"verification": "verification", "verify": "verification", "verified by": "verification", "evidence": "verification", "proof": "verification",
	"constraints": "constraints", "constraint": "constraints", "preserve": "constraints", "must not": "constraints", "do not change": "constraints",
	"boundaries": "boundaries", "boundary": "boundaries", "scope": "boundaries", "allowed": "boundaries", "files": "boundaries",
	"stop when": "stop_when", "stop_when": "stop_when", "blocked": "stop_when", "stop if blocked": "stop_when", "give up when": "stop_when",
}

var (
	goalCallLLM          = provider.CallLLM
	goalNow              = time.Now
	goalJSONObjectRE     = regexp.MustCompile(`(?s)\{.*?\}`)
	goalIsSessionWaiting = func(sessionID string) bool { return tools.DefaultProcessRegistry().IsSessionWaiting(sessionID) }
	goalListSessions     = func(ctx context.Context, taskID string) []map[string]any {
		return tools.DefaultProcessRegistry().ListSessions(ctx, tools.ToolEnvFrom(ctx), taskID, "", false)
	}
	goalLiveDelegations   = tools.LiveDelegationBatches
	goalPidExists         = proc.Exists
	goalLoadConfig        = func(ctx context.Context) (config.Tree, error) { return config.Load(home.Dir(ctx), nil) }
	goalSessionMessages   = latestSessionMessages
	goalScopedSessionCwd  = tools.ScopedSessionCwd
	goalResolveAgentCwd   = tools.ResolveAgentCwd
	goalToolEnvFrom       = tools.ToolEnvFrom
	goalPyFormatTemplates = i18n.PyFormat
	goalRunGate           = RunGate
)

// Headline and contract of goal text whose field lines such as verify or done when fill the contract
func ParseContract(text string) (string, GoalContract) {
	if text == "" {
		return "", GoalContract{}
	}
	var headline []string
	fields := map[string][]string{}
	for _, rawLine := range pystr.Splitlines(text, false) {
		line := pystr.Strip(rawLine)
		if line == "" {
			continue
		}
		if prefix, value, found := strings.Cut(line, ":"); found {
			if key, known := goalContractAliases[pystr.Lower(pystr.Strip(prefix))]; known && pystr.Strip(value) != "" {
				fields[key] = append(fields[key], pystr.Strip(value))
				continue
			}
		}
		headline = append(headline, line)
	}
	joined := func(key string) string { return pystr.Strip(strings.Join(fields[key], " ")) }
	return pystr.Strip(strings.Join(headline, " ")), GoalContract{Outcome: joined("outcome"), Verification: joined("verification"),
		Constraints: joined("constraints"), Boundaries: joined("boundaries"), StopWhen: joined("stop_when")}
}

// Subgoals as numbered extra criterion lines for the contract block
func renderExtraCriteria(subgoals []string) string {
	lines := make([]string, len(subgoals))
	for i, text := range subgoals {
		lines[i] = fmt.Sprintf("- Extra criterion %d: %s", i+1, text)
	}
	return strings.Join(lines, "\n")
}

// Gate directory of this check, or the refusal when the declared session workspace is no local directory
func gateWorkspace(ctx context.Context) (cwd, refusal string) {
	env := goalToolEnvFrom(ctx)
	if declared := goalScopedSessionCwd(env); declared != "" {
		path := pypath.PathStr(pypath.Expanduser(declared))
		if info, err := os.Stat(path); err == nil && info.IsDir() {
			return path, ""
		}
		return "", "the session workspace " + declared + " is not a directory on this host, and running gates anywhere else would check a different project"
	}
	resolved, err := goalResolveAgentCwd(env)
	if err != nil {
		return "", ""
	}
	return resolved, ""
}

// Last limit characters of text
func textTail(text string, limit int) string {
	if n := pystr.Len(text); n > limit {
		return pystr.Slice(text, n-limit, n)
	}
	return text
}

// Output a gate wrote so far, safe to read while its pipe readers still run
type gateOutput struct {
	mu  sync.Mutex
	buf []byte
}

func (g *gateOutput) Write(p []byte) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.buf = append(g.buf, p...)
	return len(p), nil
}

func (g *gateOutput) text() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return pystr.DecodeUTF8Replace(g.buf)
}

// Starts copying one child pipe into its buffer, returning the read end and the channel closed at end of file
func gatePipe(sink *gateOutput) (read, write *os.File, done chan struct{}, err error) {
	read, write, err = os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(sink, read)
	}()
	return read, write, done, nil
}

// Shell run of one gate giving pass, exit code and output tail, a timeout as minus one
func RunGate(ctx context.Context, gate GoalGate, cwd string) (passed bool, exitCode int64, outputTail string, interrupted error) {
	couldNotRun := func(err error) (bool, int64, string, error) {
		return false, -1, fmt.Sprintf("[gate could not run: %s: %s]", pypath.OSErrorClass(err), pypath.OSErrorText(err)), nil
	}
	shell, err := pysubprocess.ShellCommand(gate.Command)
	if err != nil {
		return couldNotRun(err)
	}
	var stdout, stderr gateOutput
	outRead, outWrite, outDone, err := gatePipe(&stdout)
	if err != nil {
		return couldNotRun(err)
	}
	errRead, errWrite, errDone, err := gatePipe(&stderr)
	if err != nil {
		outRead.Close()
		outWrite.Close()
		return couldNotRun(err)
	}
	defer outRead.Close()
	defer errRead.Close()
	cmd := exec.Command(shell.Path, shell.Args[1:]...)
	cmd.Dir, cmd.SysProcAttr, cmd.Stdout, cmd.Stderr = cwd, shell.SysProcAttr, outWrite, errWrite
	startErr := cmd.Start()
	outWrite.Close()
	errWrite.Close()
	if startErr != nil {
		return couldNotRun(startErr)
	}
	var waitErr error
	exited := make(chan struct{})
	go func() {
		waitErr = cmd.Wait()
		close(exited)
	}()
	finished := make(chan struct{})
	go func() {
		<-exited
		<-outDone
		<-errDone
		close(finished)
	}()
	timer := time.NewTimer(time.Duration(max(1, gate.TimeoutSeconds)) * time.Second)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
		_ = cmd.Process.Kill()
		<-exited
		return false, -1, textTail(stdout.text()+stderr.text()+fmt.Sprintf("\n[gate timed out after %ds]", gate.TimeoutSeconds), gateOutputTailChars), nil
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-exited
		return false, -1, "", ctx.Err()
	}
	code := int64(0)
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		code = int64(pysubprocess.Returncode(exitErr))
	} else if waitErr != nil {
		return couldNotRun(waitErr)
	}
	combined := stdout.text()
	if errText := stderr.text(); errText != "" {
		combined += "\n" + errText
	}
	return code == 0, code, textTail(combined, gateOutputTailChars), nil
}

// Text cut to limit characters with a truncation mark, empty for empty text
func goalTruncate(text string, limit int) string {
	if text == "" || pystr.Len(text) <= limit {
		return text
	}
	return pystr.Slice(text, 0, limit) + "… [truncated]"
}

// True when the pid names a live process, false for any pid this host cannot hold
func goalPidAlive(pid int64) (alive bool) {
	if pid <= 0 || pid != int64(int(pid)) {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			alive = false
		}
	}()
	return goalPidExists(int(pid))
}

// True while the process registry session runs and its trigger has not fired, false on any registry failure
func goalSessionWaiting(sessionID string) (waiting bool) {
	if sessionID == "" {
		return false
	}
	defer func() {
		if r := recover(); r != nil {
			waiting = false
		}
	}()
	return goalIsSessionWaiting(sessionID)
}

// Setting auxiliary.goal_judge.key passed through cast, the default for a missing, garbage or non positive value
func goalJudgeSetting(ctx context.Context, key string, def any, cast func(any) (any, bool, error)) (value any) {
	defer func() {
		if r := recover(); r != nil {
			value = def
		}
	}()
	cfg, err := goalLoadConfig(ctx)
	if err != nil {
		return def
	}
	section, isDict := pyrepr.Or(cfg["auxiliary"], map[string]any{}).(map[string]any)
	if !isDict {
		return def
	}
	judge, present := section["goal_judge"]
	if !present {
		judge = map[string]any{}
	}
	judgeSection, isDict := judge.(map[string]any)
	if !isDict {
		return def
	}
	raw, present := judgeSection[key]
	if !present {
		raw = def
	}
	converted, positive, err := cast(raw)
	if err != nil || !positive {
		return def
	}
	return converted
}

// Python int cast of a setting and whether it is above zero
func castJudgeInt(raw any) (any, bool, error) {
	n, err := pyint.Of(raw)
	if err != nil {
		return nil, false, err
	}
	return pyint.Value(n), n.Sign() > 0, nil
}

// Python float cast of a setting and whether it is above zero
func castJudgeFloat(raw any) (any, bool, error) {
	f, err := pyint.Float(raw)
	return f, err == nil && f > 0, err
}

// Output token budget of the judge from auxiliary.goal_judge.max_tokens
func goalJudgeMaxTokens(ctx context.Context) any {
	return goalJudgeSetting(ctx, "max_tokens", int64(DefaultJudgeMaxTokens), castJudgeInt)
}

// Timeout of the judge call from auxiliary.goal_judge.timeout
func goalJudgeTimeout(ctx context.Context) float64 {
	return goalJudgeSetting(ctx, "timeout", DefaultJudgeTimeout, castJudgeFloat).(float64)
}

// AttributeError a str method call on a value of another type raises
func strMethodError(v any, method string) error {
	return &pyint.Error{Kind: "AttributeError", Msg: fmt.Sprintf("'%s' object has no attribute '%s'", pyrepr.TypeName(v), method)}
}

// JSON object of a fence stripped reply or its first parsable braced span, nil otherwise
func extractJSONObject(raw any) (*pyjson.Object, error) {
	if !pyrepr.Truthy(raw) {
		return nil, nil
	}
	rawText, isText := raw.(string)
	if !isText {
		return nil, strMethodError(raw, "strip")
	}
	text := pystr.Strip(rawText)
	if strings.HasPrefix(text, "```") {
		text = pystr.StripChars(text, "`")
		if nl := strings.Index(text, "\n"); nl != -1 {
			text = text[nl+1:]
		}
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		match := goalJSONObjectRE.FindString(text)
		if match == "" {
			return nil, nil
		}
		if data, err = pyjson.Loads(match); err != nil {
			return nil, nil
		}
	}
	obj, _ := data.(*pyjson.Object)
	return obj, nil
}

// Verdict, reason, unusable reply flag and wait target of a judge reply, failing open to continue
func parseJudgeResponse(raw any) (verdict, reason string, parseFailed bool, wait map[string]any, err error) {
	if !pyrepr.Truthy(raw) {
		return "continue", "judge returned empty response", true, nil, nil
	}
	data, err := extractJSONObject(raw)
	if err != nil {
		return "", "", false, nil, err
	}
	if data == nil {
		return "continue", "judge reply was not JSON: " + pyrepr.Repr(goalTruncate(raw.(string), 200)), true, nil, nil
	}
	get := func(key string) any { return data.GetOr(key, nil) }
	reason = pystr.Strip(pyrepr.Str(pyrepr.Or(get("reason"), "")))
	if reason == "" {
		reason = "no reason provided"
	}
	if verdictRaw, isStr := get("verdict").(string); isStr {
		verdict = pystr.Lower(pystr.Strip(verdictRaw))
	} else {
		doneVal := get("done")
		done := pyrepr.Truthy(doneVal)
		if text, isStr := doneVal.(string); isStr {
			switch pystr.Lower(pystr.Strip(text)) {
			case "true", "yes", "1", "done":
				done = true
			default:
				done = false
			}
		}
		verdict = "continue"
		if done {
			verdict = "done"
		}
	}
	switch verdict {
	case "done", "blocked", "continue", "wait":
	default:
		verdict = "continue"
	}
	if verdict != "wait" {
		return verdict, reason, false, nil, nil
	}
	firstInt := func(keys ...string) any {
		for _, key := range keys {
			value := get(key)
			if value == nil {
				continue
			}
			iv, convErr := pyint.Of(value)
			if convErr != nil {
				if kind := convErr.(*pyint.Error).Kind; kind == "TypeError" || kind == "ValueError" {
					continue
				}
				panic(convErr)
			}
			if iv.Sign() > 0 {
				return pyint.Value(iv)
			}
		}
		return nil
	}
	sess := get("wait_on_session")
	for _, key := range []string{"session_id", "wait_session"} {
		if !pyrepr.Truthy(sess) {
			sess = get(key)
		}
	}
	if text, isStr := sess.(string); isStr && pystr.Strip(text) != "" {
		return "wait", reason, false, map[string]any{"session_id": pystr.Strip(text)}, nil
	}
	if pid := firstInt("wait_on_pid", "pid", "wait_pid"); pid != nil {
		return "wait", reason, false, map[string]any{"pid": pid}, nil
	}
	if seconds := firstInt("wait_for_seconds", "seconds", "wait_seconds"); seconds != nil {
		return "wait", reason, false, map[string]any{"seconds": seconds}, nil
	}
	return "continue", reason + " (wait verdict had no target — continuing)", false, nil, nil
}

// Single line text of a process field cut to 120 characters
func backgroundField(p map[string]any, key string) string {
	text := pyrepr.Str(pyrepr.Or(p[key], ""))
	return goalTruncate(pystr.Strip(strings.ReplaceAll(text, "\n", " ")), 120)
}

// Running process registry entries as judge prompt lines, empty when nothing runs
func renderBackgroundBlock(backgroundProcesses []map[string]any) string {
	var lines []string
	for _, p := range backgroundProcesses {
		if p == nil || p["status"] == "exited" || !pyrepr.Truthy(p["pid"]) {
			continue
		}
		cmd, tail := backgroundField(p, "command"), backgroundField(p, "output_preview")
		line := "- pid " + pyrepr.Str(p["pid"])
		if pyrepr.Truthy(p["session_id"]) {
			line += " / session " + pyrepr.Str(p["session_id"])
		}
		line += ": " + cmd
		if p["uptime_seconds"] != nil {
			line += " (running " + pyrepr.Str(p["uptime_seconds"]) + "s)"
		}
		if wps := p["watch_patterns"]; pyrepr.Truthy(wps) {
			hit := ""
			if pyrepr.Truthy(p["watch_hit"]) {
				hit = " [already matched]"
			}
			line += " | watch_patterns=" + pyrepr.Str(wps) + hit
		} else if pyrepr.Truthy(p["notify_on_complete"]) {
			line += " | notify_on_complete"
		}
		if tail != "" {
			line += " | recent output: " + tail
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return ""
	}
	return mustGoalFormat(judgeBackgroundBlockTemplate, map[string]any{"background_lines": strings.Join(lines, "\n")})
}

// Template filled like str.format, panicking on a template and arguments that do not fit
func mustGoalFormat(template string, kwargs map[string]any) string {
	out, err := goalPyFormatTemplates(template, kwargs)
	if err != nil {
		panic(err)
	}
	return out
}

// Raw reply content of the goal_judge auxiliary task for a system and user prompt, empty when the reply has none
func callGoalJudgeLLM(ctx context.Context, systemPrompt, userPrompt string, timeout float64) (any, error) {
	resp, err := goalCallLLM(ctx, map[string]any{
		"task":        "goal_judge",
		"messages":    []any{map[string]any{"role": "system", "content": systemPrompt}, map[string]any{"role": "user", "content": userPrompt}},
		"temperature": int64(0), "max_tokens": goalJudgeMaxTokens(ctx), "timeout": timeout,
	})
	if err != nil {
		return nil, err
	}
	message, err := provider.FirstChoiceMessage(resp)
	if err != nil {
		return "", nil
	}
	return pyrepr.Or(provider.MessageField(message, "content"), ""), nil
}

// Optional inputs of one judge call
type JudgeOptions struct {
	Timeout             *float64
	Subgoals            []string
	BackgroundProcesses []map[string]any
	Contract            *GoalContract
	ActiveDelegations   int
}

// Outcome of one judge call, the verdict being done, blocked, continue, wait or skipped
type JudgeResult struct {
	Verdict         string
	Reason          string
	ParseFailed     bool
	Wait            map[string]any
	TransportFailed bool
}

// Asks the auxiliary model whether the goal is satisfied, failing open to continue on any transport error
func JudgeGoal(ctx context.Context, goal, lastResponse string, o JudgeOptions) JudgeResult {
	if pystr.Strip(goal) == "" {
		return JudgeResult{Verdict: "skipped", Reason: "empty goal"}
	}
	if pystr.Strip(lastResponse) == "" {
		return JudgeResult{Verdict: "continue", Reason: "empty response (nothing to evaluate)"}
	}
	timeout := goalJudgeTimeout(ctx)
	if o.Timeout != nil {
		timeout = *o.Timeout
	}
	var cleanSubgoals []string
	for _, s := range o.Subgoals {
		if pystr.Strip(s) != "" {
			cleanSubgoals = append(cleanSubgoals, pystr.Strip(s))
		}
	}
	backgroundBlock := renderBackgroundBlock(o.BackgroundProcesses)
	if o.ActiveDelegations > 0 {
		backgroundBlock += mustGoalFormat(judgeDelegationsBlockTemplate, map[string]any{"count": int64(o.ActiveDelegations)})
	}
	common := map[string]any{"goal": goalTruncate(goal, 2000), "response": goalTruncate(lastResponse, judgeResponseSnippetChars),
		"background_block": backgroundBlock, "current_time": config.SafeStrftime(goalNow().Local(), "%Y-%m-%d %H:%M:%S %Z")}
	prompt := ""
	switch {
	case o.Contract != nil && !o.Contract.IsEmpty():
		contractBlock := o.Contract.RenderBlock()
		if len(cleanSubgoals) > 0 {
			contractBlock += "\n" + renderExtraCriteria(cleanSubgoals)
		}
		common["contract_block"] = goalTruncate(contractBlock, 2500)
		prompt = mustGoalFormat(judgeUserPromptWithContractTemplate, common)
	case len(cleanSubgoals) > 0:
		lines := make([]string, len(cleanSubgoals))
		for i, text := range cleanSubgoals {
			lines[i] = fmt.Sprintf("- %d. %s", i+1, text)
		}
		common["subgoals_block"] = goalTruncate(strings.Join(lines, "\n"), 2000)
		prompt = mustGoalFormat(judgeUserPromptWithSubgoalsTemplate, common)
	default:
		prompt = mustGoalFormat(judgeUserPromptTemplate, common)
	}
	raw, err := callGoalJudgeLLM(ctx, judgeSystemPrompt, prompt, timeout)
	if err != nil {
		var unavailable *provider.AuxiliaryClientUnavailable
		if errors.As(err, &unavailable) {
			goalsLogger().Info(fmt.Sprintf("goal judge: auxiliary client unavailable (%s) — falling through to continue", pyrepr.ExceptionMessage(err)))
			return JudgeResult{Verdict: "continue", Reason: "goal_judge auxiliary client unavailable: " + pyrepr.ExceptionMessage(err), TransportFailed: true}
		}
		goalsLogger().Info(fmt.Sprintf("goal judge: API call failed (%s) — falling through to continue", pyrepr.ExceptionMessage(err)))
		return JudgeResult{Verdict: "continue", Reason: "judge error: " + pyrepr.ExceptionName(err), TransportFailed: true}
	}
	verdict, reason, parseFailed, wait, err := parseJudgeResponse(raw)
	if err != nil {
		panic(err)
	}
	waitText := ""
	if len(wait) > 0 {
		waitText = " wait=" + pyrepr.Str(wait)
	}
	goalsLogger().Info(fmt.Sprintf("goal judge: verdict=%s reason=%s%s", verdict, goalTruncate(reason, 120), waitText))
	return JudgeResult{Verdict: verdict, Reason: reason, ParseFailed: parseFailed, Wait: wait}
}

// Result of the judge hook with any exception the judge raised returned as the error
func judgeRaising(ctx context.Context, goal, lastResponse string, o JudgeOptions) (result JudgeResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			raised, isPy := r.(*pyint.Error)
			if !isPy {
				panic(r)
			}
			err = raised
		}
	}()
	return goalJudgeFn(ctx, goal, lastResponse, o), nil
}

// Live async delegation batches the session spawned, zero for no session or a registry failure
func CountActiveDelegations(sessionID string) (count int) {
	if sessionID == "" {
		return 0
	}
	defer func() {
		if r := recover(); r != nil {
			count = 0
		}
	}()
	return goalLiveDelegations(sessionID)
}

// Mapping view of a message or content block, not ok for a value that is not a dict
func dictOf(v any) (func(string) (any, bool), bool) {
	switch m := v.(type) {
	case map[string]any:
		if m == nil {
			return nil, false
		}
		return func(k string) (any, bool) { x, ok := m[k]; return x, ok }, true
	case *pyjson.Object:
		if m == nil {
			return nil, false
		}
		return m.Get, true
	}
	return nil, false
}

// Elements of a Python list value, not ok for any other value
func listOf(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	case []*pyjson.Object:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// Goal text, or the already seen pointer when the last user message is essentially that text
func GoalKickPrompt(goal string, lastUserMessage any) string {
	content := lastUserMessage
	if blocks, isList := listOf(content); isList {
		var texts []string
		for _, block := range blocks {
			if get, isDict := dictOf(block); isDict {
				text, present := get("text")
				if !present {
					text = ""
				}
				texts = append(texts, pyrepr.Str(text))
			}
		}
		content = strings.Join(texts, " ")
	}
	normalize := func(v any) string {
		return strings.Join(pystr.SplitWhitespace(pyrepr.Str(pyrepr.Or(v, "")), -1), " ")
	}
	goalNorm, lastNorm := normalize(goal), normalize(content)
	if pystr.Len(goalNorm) >= goalRepasteMinChars && strings.Contains(lastNorm, goalNorm) &&
		float64(pystr.Len(goalNorm)) >= goalRepasteMinShare*float64(pystr.Len(lastNorm)) {
		return GoalAlreadySeenKick
	}
	return goal
}

// Content of the newest user message of a chat history, empty text when there is none
func LastUserMessageContent(history any) any {
	items, _ := listOf(history)
	for i := len(items) - 1; i >= 0; i-- {
		if get, isDict := dictOf(items[i]); isDict {
			if role, _ := get("role"); role == "user" {
				content, _ := get("content")
				return content
			}
		}
	}
	return ""
}

// Newest twenty messages of a session from the session store, not ok when the store cannot be read
func latestSessionMessages(ctx context.Context, sessionID string) ([]*pyjson.Object, bool) {
	db := SessionDB(ctx)
	if db == nil {
		return nil, false
	}
	limit := 20
	rows, err := db.GetMessages(ctx, sessionID, state.GetMessagesOptions{Limit: &limit, Latest: true})
	if err != nil {
		return nil, false
	}
	return rows, true
}

// Newest user message of a session from the session store, empty text on any failure
func LastUserMessageFromDB(ctx context.Context, sessionID string) (content any) {
	if sessionID == "" {
		return ""
	}
	defer func() {
		if r := recover(); r != nil {
			content = ""
		}
	}()
	rows, ok := goalSessionMessages(ctx, sessionID)
	if !ok {
		return ""
	}
	return LastUserMessageContent(rows)
}

// Running process registry sessions for the judge, only those the owner task spawned when one is named
func GatherBackgroundProcesses(ctx context.Context, taskID, ownerTaskID string) (running []map[string]any) {
	sessions, failed := func() (sessions []map[string]any, failed bool) {
		defer func() {
			if r := recover(); r != nil {
				goalsLogger().Debug(fmt.Sprintf("gather_background_processes failed: %v", r))
				sessions, failed = nil, true
			}
		}()
		return goalListSessions(ctx, taskID), false
	}()
	running = []map[string]any{}
	if failed {
		return running
	}
	for _, session := range sessions {
		if session == nil || session["status"] == "exited" {
			continue
		}
		running = append(running, session)
	}
	if ownerTaskID != "" {
		owned := []map[string]any{}
		for _, session := range running {
			owner := pyrepr.Or(pyrepr.Or(session["owner_task_id"], session["task_id"]), "")
			if pyrepr.Str(owner) == ownerTaskID {
				owned = append(owned, session)
			}
		}
		running = owned
	}
	return running
}

// Completion contract drafted from a plain objective through the goal_judge task, nil when unavailable or unparseable
func DraftContract(ctx context.Context, objective string, timeout *float64) *GoalContract {
	objective = pystr.Strip(objective)
	if objective == "" {
		return nil
	}
	budget := goalJudgeTimeout(ctx)
	if timeout != nil {
		budget = *timeout
	}
	raw, err := callGoalJudgeLLM(ctx, draftContractSystemPrompt, "Objective:\n"+goalTruncate(objective, 4000), budget)
	if err != nil {
		goalsLogger().Info(fmt.Sprintf("goal draft: API call failed (%s)", pyrepr.ExceptionMessage(err)))
		return nil
	}
	data, err := extractJSONObject(raw)
	if err != nil {
		panic(err)
	}
	if data == nil {
		goalsLogger().Debug("goal draft: reply was not JSON: " + pyrepr.Repr(goalTruncate(raw.(string), 200)))
		return nil
	}
	contract := GoalContractFromDict(data)
	if contract.IsEmpty() {
		return nil
	}
	return &contract
}
