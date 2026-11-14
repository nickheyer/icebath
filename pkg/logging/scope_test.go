package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupLoggingRoutesAgentRecordsToProfileScope(t *testing.T) {
	testHome(t)
	a, b := otherDir(t, "home-a"), otherDir(t, "home-b")
	if _, err := SetupLogging(homeCtx(a), Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := SetupLogging(homeCtx(b), Options{}); err != nil {
		t.Fatal(err)
	}
	GetLogger("agent.test_profile_scope_125974").LogContext(homeCtx(b), LevelInfo, "scoped-witness-b")
	FlushLogQueue()
	if !strings.Contains(readText(t, filepath.Join(b, "logs", "agent.log")), "scoped-witness-b") {
		t.Fatal("the profile scoped record missed its own logs")
	}
	if strings.Contains(readText(t, filepath.Join(a, "logs", "agent.log")), "scoped-witness-b") {
		t.Fatal("record leaked into the launch home")
	}
}

func TestSetupLoggingUnderRoutingAddsNoBareHandler(t *testing.T) {
	testHome(t)
	a, b := otherDir(t, "home-a"), otherDir(t, "home-b")
	mustSetup(t, Options{Home: a})
	mustRoute(t, a, b)
	if _, err := SetupLogging(homeCtx(b), Options{}); err != nil {
		t.Fatal(err)
	}
	GetLogger("agent.test_routing_guard_125974").LogContext(homeCtx(b), LevelInfo, "routing-guard-witness-b")
	FlushLogQueue()
	if n := len(queuedFileHandlers()); n != 0 {
		t.Fatalf("%d bare file handlers beside the routers", n)
	}
	if !strings.Contains(readText(t, filepath.Join(b, "logs", "agent.log")), "routing-guard-witness-b") {
		t.Fatal("record missing from the profile log")
	}
}

func TestReleaseProfileLogHandlersFreesEveryRoutedHandlerForThatHome(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit-routing", true: "setup_logging-adoption"}[adopt], func(t *testing.T) {
			h := testHome(t)
			profile := filepath.Join(h, "profiles", "routed-log-delete")
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			mustSetup(t, Options{Home: h, Force: true})
			if adopt {
				if _, err := SetupLogging(homeCtx(profile), Options{Home: profile}); err != nil {
					t.Fatal(err)
				}
			} else {
				mustRoute(t, h, profile)
			}
			GetLogger("agent.tests.profile-delete").LogContext(homeCtx(profile), LevelError, "routed record before deletion")
			FlushLogQueue()
			if !strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "routed record before deletion") {
				t.Fatal("record missing from the profile log")
			}
			routers := queuedRouters()
			held := map[string]*managedRotatingFileHandler{}
			for _, r := range routers {
				r.mu.Lock()
				held[r.filename] = r.profileHandlers[profile]
				r.mu.Unlock()
			}
			if len(held) != 2 || held["agent.log"] == nil || held["errors.log"] == nil {
				t.Fatalf("held %v", held)
			}
			if n, err := ReleaseProfileLogHandlers(profile); err != nil || n != 2 {
				t.Fatalf("released %d, %v", n, err)
			}
			for _, r := range routers {
				r.mu.Lock()
				handler, routed := r.profileHandlers[profile], r.profileHomes[profile]
				r.mu.Unlock()
				if handler != nil || routed {
					t.Fatal("the deleted profile is still routed")
				}
			}
			for _, fh := range held {
				if fh.currentStream() != nil {
					t.Fatal("streams still open into the deleted profile")
				}
			}
			if err := os.RemoveAll(profile); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(profile); !os.IsNotExist(err) {
				t.Fatal("profile directory survived")
			}
		})
	}
}
