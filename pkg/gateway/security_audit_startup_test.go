package gateway

import (
	"context"
	"strings"
	"testing"
)

// Pins the audit seams and the once per process sentinel for one case
func pinAudit(t *testing.T, root bool, sshd []string) {
	t.Helper()
	prevRoot, prevSshd, prevContainer := auditIsRoot, auditIterSshdConfigLines, auditInContainer
	auditIsRoot = func() bool { return root }
	auditIterSshdConfigLines = func() []string { return sshd }
	auditInContainer = func() bool { return false }
	auditRanMu.Lock()
	auditRan = false
	auditRanMu.Unlock()
	t.Cleanup(func() {
		auditIsRoot, auditIterSshdConfigLines, auditInContainer = prevRoot, prevSshd, prevContainer
		auditRanMu.Lock()
		auditRan = false
		auditRanMu.Unlock()
	})
}

func TestRunSecurityAuditAggregates(t *testing.T) {
	pinAudit(t, true, []string{"PasswordAuthentication yes"})
	findings := RunSecurityAudit(context.Background(), t.TempDir(), map[string]any{})
	if len(findings) != 2 {
		t.Fatalf("findings = %q, want root + ssh", findings)
	}
}

func TestRunSecurityAuditCleanPosture(t *testing.T) {
	pinAudit(t, false, []string{"PasswordAuthentication no"})
	if findings := RunSecurityAudit(context.Background(), t.TempDir(), map[string]any{}); len(findings) != 0 {
		t.Fatalf("findings = %q, want none", findings)
	}
}

func TestLogStartupSecurityWarningsEmitsAndIsIdempotent(t *testing.T) {
	pinAudit(t, true, []string{})
	logs := captureLogs(t)
	ctx := context.Background()
	first := LogStartupSecurityWarnings(ctx, t.TempDir(), map[string]any{}, false)
	if len(first) != 1 {
		t.Fatalf("first = %q", first)
	}
	if !strings.Contains(logs.String(), "ROOT") {
		t.Fatalf("no ROOT warning logged: %s", logs.String())
	}
	if second := LogStartupSecurityWarnings(ctx, t.TempDir(), map[string]any{}, false); len(second) != 0 {
		t.Fatalf("second = %q, want a no-op", second)
	}
	if forced := LogStartupSecurityWarnings(ctx, t.TempDir(), map[string]any{}, true); len(forced) != 1 {
		t.Fatalf("forced = %q", forced)
	}
}
