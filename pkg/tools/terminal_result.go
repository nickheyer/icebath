package tools

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Signal death notes keyed by signum, used for both negative and 128 plus signum encodings
var signalExitNotes = map[int]string{
	3:  "SIGQUIT (quit from keyboard)",
	4:  "SIGILL (illegal instruction \u2014 corrupt binary or wrong architecture)",
	6:  "SIGABRT (abort \u2014 assertion failure, fatal runtime error, or glibc abort)",
	7:  "SIGBUS (bus error \u2014 misaligned or unmapped memory access)",
	8:  "SIGFPE (fatal arithmetic error, e.g. integer division by zero)",
	9:  "SIGKILL \u2014 often the kernel OOM killer on memory exhaustion, or an explicit kill -9",
	11: "SIGSEGV (segmentation fault \u2014 the program crashed)",
	13: "SIGPIPE (wrote to a closed pipe \u2014 e.g. output piped to a reader that exited)",
	15: "SIGTERM (terminated \u2014 kill/timeout or shutdown requested it to stop)",
	24: "SIGXCPU (CPU time limit exceeded)",
	25: "SIGXFSZ (file size limit exceeded)",
}

// POSIX signal names in Linux numbering
var posixSignalNames = []string{"", "SIGHUP", "SIGINT", "SIGQUIT", "SIGILL", "SIGTRAP", "SIGABRT", "SIGBUS", "SIGFPE", "SIGKILL",
	"SIGUSR1", "SIGSEGV", "SIGUSR2", "SIGPIPE", "SIGALRM", "SIGTERM", "SIGSTKFLT", "SIGCHLD", "SIGCONT", "SIGSTOP", "SIGTSTP",
	"SIGTTIN", "SIGTTOU", "SIGURG", "SIGXCPU", "SIGXFSZ", "SIGVTALRM", "SIGPROF", "SIGWINCH", "SIGIO", "SIGPWR", "SIGSYS"}

// Note for a signal termination exit code, empty otherwise
func interpretSignalExit(exitCode int) string {
	if exitCode < 0 {
		signum := -exitCode
		if signum == 2 {
			return ""
		}
		if note, ok := signalExitNotes[signum]; ok {
			return fmt.Sprintf("Command terminated by signal %d: %s", signum, note)
		}
		name := fmt.Sprintf("signal %d", signum)
		if signum < len(posixSignalNames) {
			name = posixSignalNames[signum]
		}
		return fmt.Sprintf("Command terminated by %s (signal %d)", name, signum)
	}
	if exitCode > 128 {
		if note, ok := signalExitNotes[exitCode-128]; ok {
			return fmt.Sprintf("Exit code %d usually means the command was terminated by signal %d: %s", exitCode, exitCode-128, note)
		}
	}
	return ""
}

// Informational non zero exit codes per base command
var exitCodeSemantics = func() map[string]map[int]string {
	m := map[string]map[int]string{}
	for _, k := range []string{"grep", "egrep", "fgrep", "rg", "ag", "ack"} {
		m[k] = map[int]string{1: "No matches found (not an error)"}
	}
	for _, k := range []string{"diff", "colordiff"} {
		m[k] = map[int]string{1: "Files differ (expected, not an error)"}
	}
	for _, k := range []string{"test", "["} {
		m[k] = map[int]string{1: "Condition evaluated to false (expected, not an error)"}
	}
	m["find"] = map[int]string{1: "Some directories were inaccessible (partial results may still be valid)"}
	m["curl"] = map[int]string{6: "Could not resolve host", 7: "Failed to connect to host",
		22: "HTTP response code indicated error (e.g. 404, 500)", 28: "Operation timed out"}
	m["git"] = map[int]string{1: "Non-zero exit (often normal \u2014 e.g. 'git diff' returns 1 when files differ)"}
	return m
}()

// Model facing warning attached when the backend replaced its sandbox mid command
const envRecreatedNote = "The execution environment was recreated while running this command " +
	"(the previous container/sandbox was gone). Persistent files were restored " +
	"where the backend supports it, but background processes and any files " +
	"outside persisted paths from earlier commands may be lost \u2014 verify state " +
	"before relying on prior work."

var exitSegmentSplitRe = regexp.MustCompile(`\s*(?:\|\||&&|[|;])\s*`)

// Note for a non zero exit code that is informational rather than an error, empty otherwise
func interpretExitCode(command string, exitCode int) string {
	if exitCode == 0 {
		return ""
	}
	if note := interpretSignalExit(exitCode); note != "" {
		return note
	}
	segments := exitSegmentSplitRe.Split(command, -1)
	last := command
	if len(segments) > 0 {
		last = segments[len(segments)-1]
	}
	base := ""
	for _, w := range strings.Fields(strings.TrimSpace(last)) {
		if !strings.Contains(w, "=") || strings.HasPrefix(w, "-") {
			parts := strings.Split(w, "/")
			base = parts[len(parts)-1]
			break
		}
	}
	return exitCodeSemantics[base][exitCode]
}

// Sudo failure handling, output, auth failed and cache cleared
func sudoAnnotations(env *Env, command, output, envType, sessionKey string) (string, bool, bool) {
	output = handleSudoFailure(env, output, envType)
	authFailed := sudoWrongPasswordFailure(output)
	cacheCleared := invalidateCachedSudoOnAuthFailure(env, sudoCacheScope(env, sessionKey), command, output)
	canReprompt := cacheCleared && ((env != nil && env.Ask != nil) || truthyEnvValue(os.Getenv("ICEBATH_INTERACTIVE"))) && !noSudoUser(env)
	if canReprompt {
		output += "\n\n\u26a0\ufe0f Sudo authentication failed \u2014 cached password " +
			"cleared. You will be prompted again on the next sudo command."
	}
	return output, authFailed, cacheCleared
}

// TerminalOutputTransformHook is the plugin transform_terminal_output seam, installed by the plugins host, first string wins
var TerminalOutputTransformHook func(payload map[string]any) []any

// Plugin output transform seam, fail open
func applyOutputTransformHook(env *Env, command, output string, returncode int, taskID, envType string) string {
	if TerminalOutputTransformHook == nil {
		return output
	}
	callID, _ := loadSessionString(env, "approval.tool_call_id")
	results := TerminalOutputTransformHook(map[string]any{
		"command": command, "output": output, "returncode": returncode, "task_id": taskID,
		"env_type": envType, "tool_call_id": callID,
	})
	for _, r := range results {
		if s, ok := r.(string); ok {
			return s
		}
	}
	return output
}

// Recovery hint for well known failure shapes, on success a masked failure warning
func failureHint(command string, returncode int, output, exitNote string) string {
	if returncode == 0 {
		return AnnotateMaskedSuccess(command, output)
	}
	if exitNote == "" {
		return AnnotateFailure(command, returncode, output)
	}
	return ""
}

// Spill handle fields after redacting the collector's raw file in place, dropped on failure
func redactSpillFile(env *Env, path string, totalChars int, command string) [][2]any {
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		text := strings.TrimPrefix(termDecodeUTF8(raw), "\ufeff")
		err = WriteTextExclusive(path, RedactTerminalCommandOutput(env, StripANSI(text), command, false), true, true)
	}
	if err != nil {
		envLog(env).Debug("spill redaction failed, dropping spill handle", "error", err)
		os.Remove(path)
		return nil
	}
	note := fmt.Sprintf("Output exceeded the capture window (head+tail shown). "+
		"Full output (%s chars) saved to %s \u2014 search it with "+
		"search_files or page it with read_file instead of re-running the command.", commaInt(totalChars), path)
	return [][2]any{{"output_total_chars", totalChars}, {"full_output_path", path}, {"truncation_note", note}}
}

// VerificationEvidenceRecorder records a terminal result as verification evidence, installed by the agent
var VerificationEvidenceRecorder func(env *Env, command, cwd, sessionID string, exitCode int, output string, pipelineStatus []int) map[string]any

// Verification evidence summary for the result, nil when nothing was recorded
func verificationEvidence(env *Env, command, cwd, sessionID string, returncode int, output string, pipelineStatus []int) map[string]any {
	if VerificationEvidenceRecorder == nil {
		return nil
	}
	evidence := VerificationEvidenceRecorder(env, command, cwd, sessionID, returncode, output, pipelineStatus)
	if len(evidence) == 0 {
		return nil
	}
	out := map[string]any{}
	for _, k := range []string{"status", "kind", "scope", "canonical_command"} {
		out[k] = evidence[k]
	}
	return out
}

// Turns a raw execute result into the tool's JSON result string
func finalizeForegroundResult(env *Env, command string, result *ExecResult, e Environment, envType, effectiveTaskID, taskID, sessionID, sessionKey, workdir, commandCwd, approvalNote string) string {
	observedCwd := ""
	if result.CwdObserved {
		observedCwd = result.Cwd
		if observedCwd == "" && e != nil {
			observedCwd = e.Cwd()
		}
	}
	if workdir == "" && observedCwd != "" {
		RecordSessionCwd(env, sessionKey, observedCwd)
	}
	output := result.Output
	returncode := result.ReturnCode
	output, sudoAuthFailed, sudoCacheCleared := sudoAnnotations(env, command, output, envType, sessionKey)
	output = applyOutputTransformHook(env, command, output, returncode, effectiveTaskID, envType)
	output = TruncateHeadTail(output, ToolOutputLimits(env).MaxBytes, "")
	output = StripANSI(output)
	if output != "" {
		output = RedactTerminalCommandOutput(env, strings.TrimSpace(output), command, false)
	}
	exitNote := interpretExitCode(command, returncode)
	hint := failureHint(command, returncode, output, exitNote)
	changedCwd := ""
	if observedCwd != "" && commandCwd != "" && realPathOf(observedCwd) != realPathOf(commandCwd) {
		changedCwd = observedCwd
	}
	if approvalNote != "" && returncode == 130 && strings.Contains(output, "[Command interrupted]") {
		approvalNote = strings.TrimRight(approvalNote, ".") + ", then interrupted."
	}
	evidenceSession := sessionID
	if evidenceSession == "" {
		evidenceSession = taskID
	}
	if evidenceSession == "" {
		evidenceSession = effectiveTaskID
	}
	if evidenceSession == "" {
		evidenceSession = "default"
	}
	fields := [][2]any{{"output", output}, {"exit_code", returncode}, {"error", nil}}
	optional := [][2]any{{"cwd", changedCwd}}
	if result.EnvironmentRecreated {
		optional = append(optional, [2]any{"environment_recreated", envRecreatedNote})
	}
	optional = append(optional, redactSpillFile(env, result.FullOutputPath, result.OutputTotalChars, command)...)
	if evidence := verificationEvidence(env, command, commandCwd, evidenceSession, returncode, output, result.PipelineStatus); evidence != nil {
		optional = append(optional, [2]any{"verification_evidence", evidence})
	}
	optional = append(optional, [2]any{"approval", approvalNote}, [2]any{"exit_code_meaning", exitNote}, [2]any{"hint", hint})
	if sudoAuthFailed {
		optional = append(optional, [2]any{"sudo_auth_failed", true})
	}
	if sudoCacheCleared {
		optional = append(optional, [2]any{"sudo_cache_cleared", true})
	}
	for _, kv := range optional {
		if s, ok := kv[1].(string); ok && s == "" {
			continue
		}
		fields = append(fields, kv)
	}
	return orderedJSON(fields)
}

// Encodes key value pairs as a JSON object keeping their order
func orderedJSON(fields [][2]any) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, kv := range fields {
		if i > 0 {
			b.WriteByte(',')
		}
		key, _ := jsonEncodeWTF8(kv[0])
		val, err := jsonEncodeWTF8(kv[1])
		if err != nil {
			val, _ = jsonEncodeWTF8(fmt.Sprint(kv[1]))
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(val)
	}
	b.WriteByte('}')
	return b.String()
}
