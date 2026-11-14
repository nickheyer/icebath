package manage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
)

// Victim profile dir under a temp home with service facts and supervisor calls faked for host
func victimService(t *testing.T, host string) (string, string, *[][]string) {
	t.Helper()
	tmp := manageEnv(t)
	victim := filepath.Join(tmp, ".icebath", "profiles", "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "xdg"))
	savedFacts, savedHost, savedRun := serviceFacts, hostSystem, runQuiet
	t.Cleanup(func() { serviceFacts, hostSystem, runQuiet = savedFacts, savedHost, savedRun })
	hostSystem = func() string { return host }
	calls := &[][]string{}
	runQuiet = func(_ time.Duration, args ...string) (int, error) {
		*calls = append(*calls, append([]string(nil), args...))
		return 0, nil
	}
	serviceFacts.ServiceName = func(ctx context.Context) string { return "icebath-gateway-" + filepath.Base(home.Dir(ctx)) }
	serviceFacts.UserSystemdUnitDir = func(context.Context) string { return filepath.Join(tmp, "xdg", "systemd", "user") }
	serviceFacts.PinnedHome = func(string) (string, bool) { return "", false }
	serviceFacts.Resolve = func(p string) string { return p }
	return tmp, victim, calls
}

func TestCleanupHandlesTheSystemScopeUnit(t *testing.T) {
	for _, euid := range []int{0, 1000} {
		t.Run(map[int]string{0: "root", 1000: "user"}[euid], func(t *testing.T) {
			tmp, victim, calls := victimService(t, "Linux")
			systemDir := filepath.Join(tmp, "etc-systemd-system")
			unit := filepath.Join(systemDir, "icebath-gateway-victim.service")
			writeFile(t, unit, "[Service]\n")
			serviceFacts.SystemUnitDir = func() string { return systemDir }
			serviceFacts.Geteuid = func() int { return euid }
			out := captureOut(t)
			removed := cleanupGatewayService(context.Background(), "victim", victim)
			_, statErr := os.Stat(unit)
			if euid == 0 {
				if !removed || statErr == nil {
					t.Fatalf("removed %v unit stat %v", removed, statErr)
				}
				found := false
				for _, c := range *calls {
					found = found || reflect.DeepEqual(c, []string{"systemctl", "disable", "icebath-gateway-victim"})
				}
				if !found {
					t.Fatalf("calls %v", *calls)
				}
				return
			}
			if removed || statErr != nil {
				t.Fatalf("removed %v unit stat %v", removed, statErr)
			}
			text := out.String()
			if !strings.Contains(text, unit) || !strings.Contains(text, "sudo systemctl disable --now icebath-gateway-victim") {
				t.Fatalf("output %q", text)
			}
		})
	}
}

func TestCleanupDeletesTheWindowsScheduledTask(t *testing.T) {
	tmp, victim, _ := victimService(t, "Windows")
	registered := map[string]bool{"IcebathGateway_victim": true}
	startup := filepath.Join(tmp, "Startup", "IcebathGateway_victim.vbs")
	writeFile(t, startup, "")
	serviceFacts.WindowsTaskName = func(context.Context) string { return "IcebathGateway_victim" }
	serviceFacts.WindowsTaskRegistered = func(context.Context) bool { return len(registered) > 0 }
	serviceFacts.ExecSchtasks = func(args []string) (int, string, string) {
		delete(registered, args[len(args)-1])
		return 0, "", ""
	}
	serviceFacts.WindowsStartupEntryPath = func(context.Context) string { return startup }
	serviceFacts.WindowsLegacyStartupEntryPath = func(context.Context) string { return filepath.Join(tmp, "Startup", "legacy.vbs") }
	captureOut(t)
	if !cleanupGatewayService(context.Background(), "victim", victim) {
		t.Fatal("cleanup reported nothing removed")
	}
	if len(registered) != 0 {
		t.Fatalf("task still registered %v", registered)
	}
	if _, err := os.Stat(startup); !os.IsNotExist(err) {
		t.Fatalf("startup entry %v", err)
	}
}
