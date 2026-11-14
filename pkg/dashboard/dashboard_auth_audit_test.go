package dashboard

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Value whose repr stands in for a non JSON object such as a Path
type reprValue string

func (r reprValue) PyRepr() string { return "PosixPath('" + string(r) + "')" }

func auditProfileHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", home)
	return home
}

func auditLines(t *testing.T, home string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "logs", "dashboard-auth.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestAuditBoundsStringValues(t *testing.T) {
	for _, nonNative := range []bool{false, true} {
		home := auditProfileHome(t)
		oversized := strings.Repeat("value\"\n", 1000)
		var extra any = oversized
		if nonNative {
			extra = reprValue(oversized)
		}
		auditLog(context.Background(), AuditLoginFailure, "provider", oversized,
			"details", pyjson.ObjectOf("values", []any{oversized}), "extra", extra,
			"reason", "invalid_credentials", "attempts", 3, "allowed", false, "access_token", oversized)
		lines := auditLines(t, home)
		if len(lines) != 1 {
			t.Fatalf("got %d lines", len(lines))
		}
		entry, err := pyjson.LoadsDict(lines[0])
		if err != nil {
			t.Fatal(err)
		}
		details := entry.Vals["details"].(*pyjson.Object)
		for _, v := range []any{entry.Vals["provider"], details.Vals["values"].([]any)[0], entry.Vals["extra"]} {
			s := v.(string)
			if len([]rune(s)) > 256 || !strings.HasSuffix(s, "...[truncated]") {
				t.Fatalf("unbounded value %q", s)
			}
		}
		if !strings.HasPrefix(oversized, strings.TrimSuffix(entry.Vals["provider"].(string), "...[truncated]")) {
			t.Fatal("bounded provider must be a prefix of the original")
		}
		if entry.Vals["reason"] != "invalid_credentials" || entry.Vals["attempts"] != int64(3) || entry.Vals["allowed"] != false {
			t.Fatalf("plain fields changed: %v", entry.Vals)
		}
		if _, ok := entry.Vals["access_token"]; ok {
			t.Fatal("access_token must be dropped")
		}
	}
}

func TestAuditWritesJsonlines(t *testing.T) {
	home := auditProfileHome(t)
	ctx := context.Background()
	auditLog(ctx, AuditLoginStart, "provider", "nous", "ip", "1.2.3.4")
	auditLog(ctx, AuditLoginSuccess, "provider", "nous", "user_id", "u1", "email", "a@b.com", "ip", "1.2.3.4")
	lines := auditLines(t, home)
	if len(lines) != 2 {
		t.Fatalf("got %d lines", len(lines))
	}
	second, err := pyjson.LoadsDict(lines[1])
	if err != nil {
		t.Fatal(err)
	}
	if second.Vals["event"] != "login_success" || second.Vals["provider"] != "nous" || second.Vals["user_id"] != "u1" ||
		second.Vals["email"] != "a@b.com" {
		t.Fatalf("entry %v", second.Vals)
	}
	if _, ok := second.Vals["ts"]; !ok {
		t.Fatal("missing ts")
	}
}

func patchAuditCaps(t *testing.T, maxBytes int64, backups int) {
	savedMax, savedBackups := auditMaxBytes, auditBackupCount
	auditMaxBytes, auditBackupCount = maxBytes, backups
	t.Cleanup(func() { auditMaxBytes, auditBackupCount = savedMax, savedBackups })
}

func TestAuditRotatesWhenOverMaxBytes(t *testing.T) {
	home := auditProfileHome(t)
	patchAuditCaps(t, 2000, 3)
	for i := 0; i < 500; i++ {
		auditLog(context.Background(), AuditLoginStart, "provider", strings.Repeat("x", 50), "i", i)
	}
	logs := filepath.Join(home, "logs")
	for _, name := range []string{"dashboard-auth.log", "dashboard-auth.log.1", "dashboard-auth.log.2", "dashboard-auth.log.3"} {
		if _, err := os.Stat(filepath.Join(logs, name)); err != nil {
			t.Fatalf("%s missing", name)
		}
	}
	if _, err := os.Stat(filepath.Join(logs, "dashboard-auth.log.4")); err == nil {
		t.Fatal("rotation kept a fourth backup")
	}
	if info, _ := os.Stat(filepath.Join(logs, "dashboard-auth.log")); info.Size() >= 2000 {
		t.Fatalf("current log %d bytes", info.Size())
	}
}

func TestAuditRotatesOnCrossingWrite(t *testing.T) {
	home := auditProfileHome(t)
	patchAuditCaps(t, 200, 3)
	logs := filepath.Join(home, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	prior := filepath.Join(logs, "dashboard-auth.log")
	if err := os.WriteFile(prior, bytes.Repeat([]byte("x"), 190), 0o644); err != nil {
		t.Fatal(err)
	}
	auditLog(context.Background(), AuditLoginStart, "provider", "nous")
	if raw, _ := os.ReadFile(prior + ".1"); !bytes.Equal(raw, bytes.Repeat([]byte("x"), 190)) {
		t.Fatal("the prior log must rotate before the crossing append")
	}
	if info, _ := os.Stat(prior); info.Size() >= 200 {
		t.Fatalf("current log %d bytes", info.Size())
	}
}

func TestAuditRedactsTokenLikeFields(t *testing.T) {
	home := auditProfileHome(t)
	auditLog(context.Background(), AuditLoginSuccess, "provider", "nous", "access_token", "should-not-appear",
		"refresh_token", "also-not", "code", "not-this", "state", "nope")
	raw, _ := os.ReadFile(filepath.Join(home, "logs", "dashboard-auth.log"))
	for _, forbidden := range []string{"should-not-appear", "also-not", "not-this", "nope"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("token-like value leaked into audit log: %s", forbidden)
		}
	}
}
