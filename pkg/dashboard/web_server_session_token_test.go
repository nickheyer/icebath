package dashboard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

// Asserts the resolution fails with a SystemExit whose message carries want
func requireTokenExit(t *testing.T, want string) {
	t.Helper()
	_, err := gateway.ResolveSessionToken()
	var exit *gateway.DashboardExit
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(exit.Msg, want) {
		t.Fatalf("err = %#v, want SystemExit matching %q", err, want)
	}
}

func TestPlainEnvTokenIsUsedVerbatim(t *testing.T) {
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "raw-token")
	if got, err := gateway.ResolveSessionToken(); err != nil || got != "raw-token" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFilePrefixReadsAndStrips(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("  file-token  \r\n"), 0o644)
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+tokenFile)
	if got, err := gateway.ResolveSessionToken(); err != nil || got != "file-token" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestUnreadableTokenFileFailsLoudly(t *testing.T) {
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+filepath.Join(t.TempDir(), "absent"))
	requireTokenExit(t, "unreadable")
}

func TestEmptyTokenFileFailsLoudly(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("\n"), 0o644)
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+tokenFile)
	requireTokenExit(t, "empty")
}

func TestMalformedTokenFileFailsLoudly(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokenFile, []byte("\xff\xfe\x00not-utf8"), 0o644)
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "file:"+tokenFile)
	requireTokenExit(t, "unreadable")
}

func TestNoSourceMintsAToken(t *testing.T) {
	t.Setenv("ICEBATH_DASHBOARD_SESSION_TOKEN", "")
	os.Unsetenv("ICEBATH_DASHBOARD_SESSION_TOKEN")
	if got, err := gateway.ResolveSessionToken(); err != nil || len(got) == 0 {
		t.Fatalf("got %q, %v", got, err)
	}
}
