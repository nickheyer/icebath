package tui

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

// Gateway refusal with a code
func refusal(msg string, code int64) error { return &rpcError{Message: msg, Code: code, HasCode: true} }

// Fails unless text contains every part
func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Fatalf("%q lacks %q", text, p)
		}
	}
}

// Fails when text contains any part
func mustLack(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(text, p) {
			t.Fatalf("%q has %q", text, p)
		}
	}
}

func TestDescribeTurnFailure(t *testing.T) {
	t.Run("turns an auth error_surface into a plain title, a Details line and the /model + /retry hint", func(t *testing.T) {
		raw := `Error code: 401 - {"error": {"message": "Incorrect API key provided", "type": "invalid_request_error"}}`
		text := describeTurnFailure(map[string]any{"error": raw, "error_surface": map[string]any{"code": "auth", "layer": "auth", "provider": "openai", "retryable": false}, "recoverable": true})
		lines := strings.Split(text, "\n")
		mustContain(t, lines[0], "openai")
		if regexp.MustCompile(`Error code|401|\{`).MatchString(lines[0]) || !strings.HasPrefix(lines[1], "Details: ") {
			t.Fatalf("lines %q", lines)
		}
		mustContain(t, lines[1], "Incorrect API key provided")
		mustContain(t, text, "/model", "/retry")
	})
	t.Run("names the local model server when the managed local runtime is down, not a custom endpoint", func(t *testing.T) {
		text := describeTurnFailure(map[string]any{"error": "The local model server isn't running. It may still be starting.", "error_surface": map[string]any{"code": "local_runtime_unavailable", "layer": "endpoint", "provider": "llamacpp", "retryable": true}})
		mustContain(t, strings.Split(text, "\n")[0], "local model server")
		mustLack(t, text, "custom model endpoint")
		mustContain(t, text, "/model")
	})
	t.Run("names where to start a switched-off local model server, and never suggests /retry", func(t *testing.T) {
		text := describeTurnFailure(map[string]any{"error": "The local model server is turned off.", "error_surface": map[string]any{"code": "local_runtime_unavailable", "layer": "endpoint", "provider": "llamacpp", "retryable": false}})
		mustLack(t, text, "/retry")
		mustContain(t, text, "icebath model")
	})
	t.Run("falls back to the layer copy, and to a generic title, when the code is unknown", func(t *testing.T) {
		mustContain(t, describeTurnFailure(map[string]any{"error": "peer closed connection", "error_surface": map[string]any{"code": "weird", "layer": "streaming", "retryable": true}}), "/retry")
		mustContain(t, describeTurnFailure(map[string]any{"error": "boom"}), "Details: boom")
	})
	t.Run("honours error_surface.retryable=false even though the backend always sets recoverable=true", func(t *testing.T) {
		no := describeTurnFailure(map[string]any{"error": "x", "error_surface": map[string]any{"code": "weird", "layer": "provider", "retryable": false}, "recoverable": true})
		mustLack(t, no, "/retry")
		mustContain(t, no, "/model")
		mustContain(t, describeTurnFailure(map[string]any{"error": "x", "error_surface": map[string]any{"code": "weird", "layer": "provider", "retryable": true}, "recoverable": true}), "/retry")
	})
}

func TestDescribeRPCError(t *testing.T) {
	t.Run("rewrites transport errors without exposing RPC method names", func(t *testing.T) {
		for _, raw := range []string{"gateway not connected: prompt.submit", "gateway not running"} {
			text := describeRPCError(errors.New(raw))
			mustLack(t, text, "prompt.submit")
			if regexp.MustCompile(`\bgateway\b`).MatchString(text) || !strings.Contains(text, "not connected") {
				t.Fatalf("%q", text)
			}
			mustContain(t, text, "/logs")
		}
	})
	t.Run("keeps the timeout seconds but drops the method", func(t *testing.T) {
		text := describeRPCError(errors.New("request timed out after 120s: slash.exec"))
		mustContain(t, text, "120s", "/logs")
		mustLack(t, text, "slash.exec")
	})
	t.Run("explains a stale session id as saved-and-reopenable, not as loss", func(t *testing.T) {
		text := describeRPCError(refusal("session not found", 4001))
		mustLack(t, text, "not found")
		mustContain(t, text, "saved", "/resume")
	})
	t.Run("explains a read-only stored chat instead of echoing its reason code", func(t *testing.T) {
		text := describeRPCError(refusal("session_read_only", 4001))
		mustLack(t, text, "session_read_only")
		mustContain(t, text, "/new")
		mustContain(t, describeRPCError(refusal("session_has_children", 4001)), "not deleted")
	})
	t.Run("passes ordinary domain errors through unchanged", func(t *testing.T) {
		if describeRPCError(refusal("hash required", 4014)) != "hash required" {
			t.Fatal("changed")
		}
	})
	t.Run("does not treat every 4001 as a stale session: the backend reuses the code for other refusals", func(t *testing.T) {
		for _, raw := range []string{"no active session to retry", "slug and api_key are required", "session ownership changed"} {
			if describeRPCError(refusal(raw, 4001)) != raw {
				t.Fatalf("%q changed", raw)
			}
		}
		mustContain(t, describeRPCError(refusal("session not found or not owned by this transport", 4001)), "/resume")
	})
	t.Run("names /new for a refusal on a chat whose launch API key changed, never the bare code", func(t *testing.T) {
		text := describeRPCError(refusal("launch_credentials_unavailable", 4001))
		mustContain(t, text, "/new")
		mustLack(t, text, "launch_credentials_unavailable")
	})
	t.Run("records the raw wire text it replaced in the log sink", func(t *testing.T) {
		var lines []string
		setRPCErrorLogSink(func(l string) { lines = append(lines, l) })
		defer setRPCErrorLogSink(nil)
		describeRPCError(refusal("session not found", 4001))
		describeRPCError(errors.New("gateway not connected: prompt.submit"))
		describeRPCError(refusal("hash required", 4014))
		if len(lines) != 2 {
			t.Fatalf("lines %q", lines)
		}
		mustContain(t, lines[0], "4001", "session not found")
		mustContain(t, lines[1], "prompt.submit")
	})
}

func TestIsVersionSkewError(t *testing.T) {
	t.Run("recognises both skew shapes and points at /update", func(t *testing.T) {
		extra := refusal("invalid params for prompt.submit: turn_author: Extra inputs are not permitted", 4000)
		unknown := refusal("unknown method: session.control.read", -32601)
		if !isVersionSkewError(extra) || !isVersionSkewError(unknown) || isVersionSkewError(refusal("session_id required", 4000)) {
			t.Fatal("skew shapes")
		}
		mustContain(t, describeRPCError(extra), "/update")
		mustLack(t, describeRPCError(extra), "turn_author")
	})
}

func TestSlashExecFallbackPolicy(t *testing.T) {
	t.Run(`falls back to command.dispatch only for the two "slash.exec does not own this" 4018 refusals`, func(t *testing.T) {
		if !shouldFallbackToDispatch(refusal("skill command: use command.dispatch for /x", 4018)) ||
			!shouldFallbackToDispatch(refusal("snapshot restore mutates live config/state; use command.dispatch for /snapshot restore", 4018)) ||
			shouldFallbackToDispatch(refusal("slash worker timed out", 5030)) || shouldFallbackToDispatch(refusal("session not found", 4001)) {
			t.Fatal("policy")
		}
	})
	t.Run("does not re-dispatch a 4018 that command.dispatch itself already returned (would re-run /retry, /undo, bundles)", func(t *testing.T) {
		for _, raw := range []string{"retry cannot safely reconstruct or combine attached media", "bundle dispatch failed: boom", "not a quick/plugin/bundle/skill command: zzz", "quick command failed with exit code 1"} {
			if shouldFallbackToDispatch(refusal(raw, 4018)) {
				t.Fatalf("%q falls back", raw)
			}
		}
		if shouldFallbackToDispatch(refusal("unknown command: zzz", 4011)) {
			t.Fatal("4011 falls back")
		}
	})
	t.Run("names the command and the helper failure instead of the fallback refusal", func(t *testing.T) {
		timeout := describeSlashExecError("status", refusal("slash worker timed out", 5030))
		if !strings.HasPrefix(timeout, "/status did not finish") {
			t.Fatalf("%q", timeout)
		}
		mustContain(t, timeout, "timed out", "/logs")
		mustLack(t, timeout, "quick/plugin/bundle/skill")
		crash := describeSlashExecError("journey", refusal("slash worker closed pipe: ValueError: bad", 5030))
		if !strings.HasPrefix(crash, "/journey did not finish") {
			t.Fatalf("%q", crash)
		}
		mustContain(t, crash, "Details: ValueError: bad")
	})
}

func TestBackendLifecycleCopy(t *testing.T) {
	t.Run("names the exit code, the last real stderr line, /logs and icebath doctor", func(t *testing.T) {
		tail := "[lifecycle] child exit code=1\nModuleNotFoundError: No module named pydantic\n[lifecycle] scheduling gateway reconnect in 1000ms (attempt 1)"
		last, _ := lastStderrLine(tail)
		code := 1
		text := backendGaveUp(&code, last)
		mustContain(t, text, "exit code 1", "Details: ModuleNotFoundError: No module named pydantic", "/logs", "icebath doctor", "/resume")
		mustLack(t, text, "[lifecycle]")
		if regexp.MustCompile(`\bgateway\b`).MatchString(text) {
			t.Fatalf("%q", text)
		}
	})
	t.Run("only failure-looking stderr earns an activity row", func(t *testing.T) {
		for line, want := range map[string]bool{
			`  File "/x/run_agent.py", line 812, in _call_model`: false,
			"Traceback (most recent call last):":                 true,
			"[gateway-turn] ValueError: nope":                    true,
			"ValueError: nope":                                   true,
			"INFO hermes.mcp: discovered 3 servers":              false,
			"gateway incompatible: runtime_protocol":             true,
			"gateway inaccessible: profile_parked":               true,
			"the gateway incompatible: x":                        false,
		} {
			if stderrLooksLikeProblem(line) != want {
				t.Fatalf("%q", line)
			}
		}
	})
	t.Run(`ignores dependency warning lines and does not call the failure a "backend" problem`, func(t *testing.T) {
		if stderrLooksLikeProblem("/x/site-packages/foo.py:12: DeprecationWarning: use bar instead") || stderrLooksLikeProblem("UserWarning: something is odd") {
			t.Fatal("warning flagged")
		}
		row := stderrProblemActivity("ValueError: nope")
		mustContain(t, row, "(ValueError)", "/logs")
		if regexp.MustCompile(`\bbackend\b`).MatchString(row) {
			t.Fatalf("%q", row)
		}
	})
}

func TestPromptTimeoutNotice(t *testing.T) {
	t.Run("explains a timed-out password/vault prompt and stays silent for other reasons", func(t *testing.T) {
		minutes := regexp.MustCompile(`\d+ minutes?`)
		sudo, ok := promptTimeoutNotice("sudo", "timeout")
		code, _ := promptTimeoutNotice("vault.code", "timeout")
		if !ok || sudo == "" || minutes.MatchString(sudo) || minutes.MatchString(code) {
			t.Fatalf("%q %q", sudo, code)
		}
		if _, ok := promptTimeoutNotice("sudo", "interrupted"); ok {
			t.Fatal("interrupted")
		}
		if _, ok := promptTimeoutNotice("approval", "timeout"); ok {
			t.Fatal("approval")
		}
	})
}

func TestDescribeCredentialWarning(t *testing.T) {
	t.Run("adds the /model fix to the backend missing-key warning", func(t *testing.T) {
		mustContain(t, describeCredentialWarning("No API key configured for provider 'openai'. First message will fail."), "openai", "/model")
	})
}

func TestAMethodTheBackendSaysItDoesNotProvide(t *testing.T) {
	unavailable := refusal("unknown method: groups.no_such_verb \u2014 not available on this Icebath backend", -32601)
	t.Run("is not read as version skew and names the method without update advice", func(t *testing.T) {
		if isVersionSkewError(unavailable) {
			t.Fatal("skew")
		}
		text := describeRPCError(unavailable)
		mustContain(t, text, "groups.no_such_verb")
		if regexp.MustCompile(`out of sync|different versions|/update|icebath update`).MatchString(text) {
			t.Fatalf("%q", text)
		}
	})
	t.Run("keeps the skew warning for a schema the backend rejects", func(t *testing.T) {
		mustContain(t, describeRPCError(refusal("invalid params for prompt.submit: turn_author: Extra inputs are not permitted", 4000)), "/update")
	})
}

func TestDescribeCredentialWarningPassesOtherWarnings(t *testing.T) {
	t.Run("adds the /model fix to the backend missing-key warning", func(t *testing.T) {
		mustContain(t, describeCredentialWarning("No API key configured for provider 'openai'. First message will fail."), "/setup")
		if describeCredentialWarning("something else") != "something else" {
			t.Fatal("changed")
		}
	})
}

func TestDescribeShellExecError(t *testing.T) {
	for _, backend := range []string{"docker", "ssh"} {
		t.Run("names the "+backend+" backend and points at the agent (#134889 TUI-16)", func(t *testing.T) {
			err := &rpcError{Message: "unsupported_terminal_backend", Code: 4001, HasCode: true, Data: map[string]any{"reason": "unsupported_terminal_backend", "field": "terminal.backend", "detail": backend}}
			text := describeShellExecError(err)
			mustContain(t, text, "terminal backend is "+backend, "Ask the agent to run it instead")
			mustLack(t, text, "unsupported_terminal_backend")
		})
	}
	t.Run("still names the refusal when an older gateway sends no backend, and passes other errors through", func(t *testing.T) {
		text := describeShellExecError(&rpcError{Message: "unsupported_terminal_backend", Code: 4001, HasCode: true, Data: map[string]any{"reason": "unsupported_terminal_backend"}})
		mustContain(t, text, "this session's terminal backend isn't local", "Ask the agent to run it instead.")
		mustLack(t, text, "not local", "agent's terminal runs inside")
		if describeShellExecError(refusal("dangerous_command", 4001)) != "dangerous_command" {
			t.Fatal("changed")
		}
	})
}

func TestLocaleAwareResolution(t *testing.T) {
	t.Run("resolves the turn-failure and prompt-timeout tables at call time so a pack swap is observed", func(t *testing.T) {
		resetLocale(t)
		applyLocale("xx", map[string]string{"userMessages.backend.restarting": "RESTART-XX", "userMessages.promptTimeout.sudo": "SUDO-XX",
			"userMessages.turn.code.auth.title": "AUTH-XX", "userMessages.turn.notAnswered": "{0} // XX"})
		sudo, _ := promptTimeoutNotice("sudo", "timeout")
		if tr("userMessages.backend.restarting") != "RESTART-XX" || sudo != "SUDO-XX" {
			t.Fatal("pack not observed")
		}
		if got := strings.Split(describeTurnFailure(map[string]any{"error_surface": map[string]any{"code": "auth", "layer": "auth"}}), "\n")[0]; got != "AUTH-XX // XX" {
			t.Fatalf("got %q", got)
		}
	})
}
