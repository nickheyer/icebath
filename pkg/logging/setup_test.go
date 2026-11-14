package logging

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

func TestRepeatedSetupRoutesRecordsOnce(t *testing.T) {
	modes := []struct{ mode, component string }{{"cli", ""}, {"gateway", "gateway.log"}, {"gui", "gui.log"}}
	levels := []struct {
		configured, explicit string
		minimum              int
	}{{"", "", LevelInfo}, {"DEBUG", "WARNING", LevelWarning}, {"DEBUG", "", LevelDebug}}
	for _, m := range modes {
		for _, lv := range levels {
			t.Run(fmt.Sprintf("%s-%s-%s", m.mode, lv.configured, lv.explicit), func(t *testing.T) {
				h := testHome(t)
				if lv.configured != "" {
					if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("logging:\n  level: "+lv.configured+"\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				for range 2 {
					if dir := mustSetup(t, Options{Home: h, Mode: m.mode, LogLevel: lv.explicit}); dir != filepath.Join(h, "logs") {
						t.Fatalf("logs dir %q", dir)
					}
				}
				SetSessionContext("routing-session")
				sources := []string{"tools.terminal_tool", "agent.context_compressor", "gateway.run",
					"plugins.platforms.telegram.adapter", "hermes_cli.web_server", "tui_gateway.ws"}
				slogLevels := map[int]slog.Level{LevelDebug: slog.LevelDebug, LevelInfo: slog.LevelInfo, LevelWarning: slog.LevelWarn}
				for i, src := range sources {
					for _, level := range []int{LevelDebug, LevelInfo, LevelWarning} {
						slog.Default().With("logger", src).Log(context.Background(), slogLevels[level], fmt.Sprintf("routing-witness-%d-%d", i, level))
					}
				}
				FlushLogQueue()
				entries, _ := filepath.Glob(filepath.Join(h, "logs", "*.log"))
				got := map[string]string{}
				for _, e := range entries {
					got[filepath.Base(e)] = readText(t, e)
				}
				want := map[string]bool{"agent.log": true, "errors.log": true}
				if m.component != "" {
					want[m.component] = true
				}
				if len(got) != len(want) {
					t.Fatalf("log files %v, want %v", got, want)
				}
				for name, content := range got {
					if !want[name] {
						t.Fatalf("unexpected %s", name)
					}
					for i := range sources {
						for _, level := range []int{LevelDebug, LevelInfo, LevelWarning} {
							accepted := map[string]bool{
								"agent.log":   level >= lv.minimum,
								"errors.log":  level >= LevelWarning,
								"gateway.log": (i == 2 || i == 3) && level >= max(LevelInfo, lv.minimum),
								"gui.log":     (i == 4 || i == 5) && level >= max(LevelInfo, lv.minimum),
							}[name]
							w := fmt.Sprintf("routing-witness-%d-%d", i, level)
							n := 0
							if accepted {
								n = 1
							}
							if c := strings.Count(content, w); c != n {
								t.Fatalf("%s %s count %d want %d\n%s", name, w, c, n, content)
							}
						}
					}
					if !strings.Contains(content, "[routing-session]") {
						t.Fatalf("%s lacks the session tag\n%s", name, content)
					}
				}
			})
		}
	}
}

func TestProfileRoutingFollowsContextHome(t *testing.T) {
	h := testHome(t)
	profile := otherDir(t, "profile-b")
	mustSetup(t, Options{Home: h})
	mustRoute(t, h, profile)
	GetLogger("cron.scheduler.profile-routing-test").LogContext(homeCtx(profile), LevelInfo, "profile-routed cron record")
	FlushLogQueue()
	if !strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "profile-routed cron record") {
		t.Fatal("record missing from the profile log")
	}
	if strings.Contains(readText(t, filepath.Join(h, "logs", "agent.log")), "profile-routed cron record") {
		t.Fatal("record leaked into the default log")
	}
}

func TestRoutedRecordsFollowTheirOwnProfilesRedactionPolicy(t *testing.T) {
	cases := []struct {
		name           string
		launch         bool
		optOut         string
		routedRedacted bool
	}{{"launch-opt-out", false, "", true}, {"routed-env-opt-out", true, "env", false}, {"routed-config-opt-out", true, "config", false}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := testHome(t)
			routed := otherDir(t, "profile-b")
			switch c.optOut {
			case "config":
				_ = os.WriteFile(filepath.Join(routed, "config.yaml"), []byte("security:\n  redact_secrets: false\n"), 0o644)
			case "env":
				_ = os.WriteFile(filepath.Join(routed, ".env"), []byte("ICEBATH_REDACT_SECRETS=false\n"), 0o644)
			}
			if !c.launch {
				_ = os.WriteFile(filepath.Join(h, ".env"), []byte("ICEBATH_REDACT_SECRETS=false\n"), 0o644)
			}
			mustSetup(t, Options{Home: h})
			mustRoute(t, h, routed)
			routedSecret := "sk-proj-ROUTEDPROFILE" + strings.Repeat("b", 24)
			launchSecret := "sk-proj-LAUNCHPROFILE" + strings.Repeat("a", 24)
			lg := GetLogger("gateway.redaction-routing-test")
			lg.LogContext(homeCtx(routed), LevelWarning, "provider rejected key "+routedSecret)
			lg.Warning("launch key " + launchSecret)
			FlushLogQueue()
			if got := !strings.Contains(readText(t, filepath.Join(routed, "logs", "agent.log")), routedSecret); got != c.routedRedacted {
				t.Fatalf("routed redacted %v want %v", got, c.routedRedacted)
			}
			if got := !strings.Contains(readText(t, filepath.Join(h, "logs", "agent.log")), launchSecret); got != c.launch {
				t.Fatalf("launch redacted %v want %v", got, c.launch)
			}
		})
	}
}

func TestReleaseProfileLogHandlersClosesOnlyDeletedProfile(t *testing.T) {
	h := testHome(t)
	deleted, other := otherDir(t, "profile-deleted"), otherDir(t, "profile-other")
	mustSetup(t, Options{Home: h})
	mustRoute(t, h, deleted, other)
	lg := GetLogger("agent.profile-delete-log-release")
	lg.LogContext(homeCtx(deleted), LevelWarning, "deleted profile log handles")
	lg.LogContext(homeCtx(other), LevelWarning, "other profile log handles")
	FlushLogQueue()
	routers := queuedRouters()
	if len(routers) != 2 {
		t.Fatalf("%d routers", len(routers))
	}
	has := func(r *profileRoutingFileHandler, h string) (handler, routed bool) {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.profileHandlers[h] != nil, r.profileHomes[h]
	}
	for _, r := range routers {
		if a, _ := has(r, deleted); !a {
			t.Fatal("deleted home handler missing")
		}
		if a, _ := has(r, other); !a {
			t.Fatal("other home handler missing")
		}
	}
	if n, err := ReleaseProfileLogHandlers(deleted); err != nil || n != 2 {
		t.Fatalf("released %d, %v", n, err)
	}
	for _, r := range routers {
		if a, b := has(r, deleted); a || b {
			t.Fatal("deleted home still routed")
		}
		if a, _ := has(r, other); !a {
			t.Fatal("other home released")
		}
	}
	for _, f := range []string{"agent.log", "errors.log"} {
		if !strings.Contains(readText(t, filepath.Join(other, "logs", f)), "other profile log handles") {
			t.Fatalf("%s of the other profile lost its record", f)
		}
	}
}

func TestASecondHomeRoutesInsteadOfStackingAnUnfilteredHandler(t *testing.T) {
	h := testHome(t)
	profile := otherDir(t, "profile-b")
	mustSetup(t, Options{Home: h})
	mustSetup(t, Options{Home: profile})
	if n := len(queuedFileHandlers()); n != 0 {
		t.Fatalf("the second home added %d unfiltered file handlers", n)
	}
	lg := GetLogger("agent.conversation_loop.second-home-test")
	lg.LogContext(homeCtx(profile), LevelInfo, "turn of profile b")
	lg.Info("turn of the launch profile")
	FlushLogQueue()
	a, b := readText(t, filepath.Join(h, "logs", "agent.log")), readText(t, filepath.Join(profile, "logs", "agent.log"))
	if !strings.Contains(b, "turn of profile b") || strings.Contains(a, "turn of profile b") {
		t.Fatalf("profile b record misrouted\na=%s\nb=%s", a, b)
	}
	if !strings.Contains(a, "turn of the launch profile") || strings.Contains(b, "turn of the launch profile") {
		t.Fatalf("launch record misrouted\na=%s\nb=%s", a, b)
	}
}

func TestSetupForAnAlreadyRoutedHomeAddsNoDuplicateWriter(t *testing.T) {
	h := testHome(t)
	profile := otherDir(t, "profile-b")
	mustSetup(t, Options{Home: h})
	mustRoute(t, h, profile)
	mustSetup(t, Options{Home: profile})
	if n := len(queuedFileHandlers()); n != 0 {
		t.Fatalf("%d bare file handlers", n)
	}
	GetLogger("agent.conversation_loop.routed-home-test").LogContext(homeCtx(profile), LevelInfo, "once please")
	FlushLogQueue()
	if c := strings.Count(readText(t, filepath.Join(profile, "logs", "agent.log")), "once please"); c != 1 {
		t.Fatalf("written %d times", c)
	}
	if strings.Contains(readText(t, filepath.Join(h, "logs", "agent.log")), "once please") {
		t.Fatal("leaked into the launch log")
	}
}

func TestAComponentLogAddedAfterRoutingIsRoutedToo(t *testing.T) {
	h := testHome(t)
	profile := otherDir(t, "profile-b")
	mustSetup(t, Options{Home: h})
	mustSetup(t, Options{Home: profile})
	mustSetup(t, Options{Home: h, Mode: "gateway"})
	lg := GetLogger("gateway.run.routed-component-test")
	lg.LogContext(homeCtx(profile), LevelInfo, "gw-b")
	lg.Info("gw-a")
	FlushLogQueue()
	a := readText(t, filepath.Join(h, "logs", "gateway.log"))
	if !strings.Contains(a, "gw-a") || strings.Contains(a, "gw-b") {
		t.Fatalf("launch gateway.log %q", a)
	}
	if !strings.Contains(readText(t, filepath.Join(profile, "logs", "gateway.log")), "gw-b") {
		t.Fatal("profile gateway.log lacks its record")
	}
}

func TestProfileRoutingFallsBackWhenProfileHomeDeleted(t *testing.T) {
	h := testHome(t)
	profile := otherDir(t, "profile-deleted")
	mustSetup(t, Options{Home: h})
	mustRoute(t, h, profile)
	lg := GetLogger("cron.scheduler.profile-routing-deleted-test")
	defaultLog := filepath.Join(h, "logs", "agent.log")
	errText := captureStderr(t, func() {
		lg.Info("routing fallback default-home probe record")
		FlushLogQueue()
		if !strings.Contains(readText(t, defaultLog), "default-home probe record") {
			t.Fatal("default record missing")
		}
		lg.LogContext(homeCtx(profile), LevelInfo, "profile-routed record before deletion")
		FlushLogQueue()
		if !strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "record before deletion") {
			t.Fatal("profile record missing")
		}
		if err := os.RemoveAll(profile); err != nil {
			t.Fatal(err)
		}
		lg.LogContext(homeCtx(profile), LevelInfo, "profile-routed record after deletion")
		FlushLogQueue()
	})
	if !strings.Contains(readText(t, defaultLog), "record after deletion") {
		t.Fatal("post deletion record did not fall back")
	}
	if _, err := os.Stat(profile); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the deleted profile was recreated")
	}
	if strings.Contains(errText, "FileNotFoundError") {
		t.Fatalf("stderr %q", errText)
	}
}

// Replaces the router clock with a fixed time and returns the step past one recheck interval
func livenessClock(t *testing.T) func() {
	now := 1000.0
	var mu sync.Mutex
	prev := monotonic
	monotonic = func() float64 {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	t.Cleanup(func() { monotonic = prev })
	return func() {
		mu.Lock()
		now += profileLivenessRecheckS
		mu.Unlock()
	}
}

func TestOutOfBandProfileDeleteReleasesRoutedHandler(t *testing.T) {
	for _, removal := range []string{"tombstone", "rmtree", "logs-only"} {
		t.Run(removal, func(t *testing.T) {
			h := testHome(t)
			advance := livenessClock(t)
			profile := filepath.Join(h, "profiles", "worker")
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			mustSetup(t, Options{Home: h})
			mustRoute(t, h, profile)
			lg := GetLogger("cron.scheduler.out-of-band-delete-test")
			emit := func(msg string) {
				lg.LogContext(homeCtx(profile), LevelWarning, msg)
				FlushLogQueue()
			}
			emit("before delete")
			var routed []*managedRotatingFileHandler
			for _, r := range queuedRouters() {
				r.mu.Lock()
				if ph := r.profileHandlers[profile]; ph != nil {
					routed = append(routed, ph)
				}
				r.mu.Unlock()
			}
			if len(routed) == 0 {
				t.Fatal("no routed handler")
			}
			switch removal {
			case "tombstone":
				if err := home.MarkProfileDeleted(profile); err != nil {
					t.Fatal(err)
				}
				advance()
			case "rmtree":
				_ = os.RemoveAll(profile)
			default:
				_ = os.RemoveAll(filepath.Join(profile, "logs"))
			}
			errText := captureStderr(t, func() {
				for i := range 3 {
					emit(fmt.Sprintf("after delete %d", i))
				}
			})
			if strings.Contains(errText, "Logging error") {
				t.Fatalf("stderr %q", errText)
			}
			for _, r := range routed {
				if r.currentStream() != nil {
					t.Fatal("stale profile log fd kept open")
				}
			}
			defaultLog := readText(t, filepath.Join(h, "logs", "agent.log"))
			for i := range 3 {
				if !strings.Contains(defaultLog, fmt.Sprintf("after delete %d", i)) {
					t.Fatalf("record %d did not fall back\n%s", i, defaultLog)
				}
			}
			if strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "after delete") {
				t.Fatal("record written into the removed profile")
			}
		})
	}
}

func TestLiveProfileLivenessCheckedAtMostOncePerInterval(t *testing.T) {
	for _, closedPerWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept-open", true: "closed-per-write"}[closedPerWrite], func(t *testing.T) {
			h := testHome(t)
			advance := livenessClock(t)
			if closedPerWrite {
				clhMu.Lock()
				prev := useCLH
				useCLH = true
				clhMu.Unlock()
				t.Cleanup(func() {
					clhMu.Lock()
					useCLH = prev
					clhMu.Unlock()
				})
			}
			profile := filepath.Join(h, "profiles", "worker")
			if err := os.MkdirAll(profile, 0o755); err != nil {
				t.Fatal(err)
			}
			mustSetup(t, Options{Home: h})
			mustRoute(t, h, profile)
			var mu sync.Mutex
			calls := 0
			prev := namedProfileIsDeleted
			namedProfileIsDeleted = func(p string) bool {
				mu.Lock()
				calls++
				mu.Unlock()
				return prev(p)
			}
			t.Cleanup(func() { namedProfileIsDeleted = prev })
			lg := GetLogger("cron.scheduler.liveness-rate-limit-test")
			emitMany := func(n int) {
				for i := range n {
					lg.LogContext(homeCtx(profile), LevelWarning, fmt.Sprintf("live %d", i))
				}
				FlushLogQueue()
			}
			emitMany(5)
			mu.Lock()
			first := calls
			mu.Unlock()
			advance()
			emitMany(5)
			routers := len(queuedRouters())
			if routers == 0 || first != routers {
				t.Fatalf("first sighting checked %d times for %d routers", first, routers)
			}
			if calls != 2*routers {
				t.Fatalf("%d checks, want %d", calls, 2*routers)
			}
			if !strings.Contains(readText(t, filepath.Join(profile, "logs", "agent.log")), "live 4") {
				t.Fatal("profile log lacks the records")
			}
		})
	}
}

func TestExplicitParamsOverrideConfig(t *testing.T) {
	h := testHome(t)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("logging:\n  level: DEBUG\n"), 0o644)
	mustSetup(t, Options{Home: h, LogLevel: "WARNING"})
	for _, fh := range queuedFileHandlers() {
		if strings.Contains(fh.baseFilename, "agent.log") {
			if fh.Level() != LevelWarning {
				t.Fatalf("agent.log level %d", fh.Level())
			}
			return
		}
	}
	t.Fatal("no agent.log handler")
}

func TestSetupVerboseLoggingAddsStreamHandler(t *testing.T) {
	h := testHome(t)
	mustSetup(t, Options{Home: h})
	for range 2 {
		if err := SetupVerboseLogging(); err != nil {
			t.Fatal(err)
		}
	}
	var verbose []*streamHandler
	for _, hd := range root.Handlers() {
		if sh, ok := hd.(*streamHandler); ok && sh.verbose {
			verbose = append(verbose, sh)
		}
	}
	if len(verbose) != 1 || verbose[0].Level() != LevelDebug {
		t.Fatalf("verbose handlers %v", verbose)
	}
}

func TestReadLoggingConfig(t *testing.T) {
	t.Run("returns none when no config", func(t *testing.T) {
		testHome(t)
		l, s, b := readLoggingConfig(context.Background())
		if l != nil || s != nil || b != nil {
			t.Fatalf("%v %v %v", l, s, b)
		}
	})
	t.Run("reads logging section", func(t *testing.T) {
		h := testHome(t)
		_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("logging:\n  backup_count: 5\n  level: DEBUG\n  max_size_mb: 10\n"), 0o644)
		l, s, b := readLoggingConfig(context.Background())
		if l != "DEBUG" || s != 10 || b != 5 {
			t.Fatalf("%#v %#v %#v", l, s, b)
		}
	})
}

func TestGatewayLogAttachedAfterExternalRotationThenReSetup(t *testing.T) {
	h := testHome(t)
	mustSetup(t, Options{Home: h, Mode: "gateway"})
	gw, rotated := filepath.Join(h, "logs", "gateway.log"), filepath.Join(h, "logs", "gateway.log.1")
	GetLogger("gateway.run").Info("line BEFORE rotation")
	FlushLogQueue()
	if !strings.Contains(readText(t, gw), "BEFORE rotation") {
		t.Fatal("first line missing")
	}
	if err := os.Rename(gw, rotated); err != nil {
		t.Fatal(err)
	}
	mustSetup(t, Options{Home: h, Mode: "gateway"})
	GetLogger("gateway.run").Info("line AFTER rotation")
	FlushLogQueue()
	if !strings.Contains(readText(t, gw), "AFTER rotation") {
		t.Fatal("gateway.log was never recreated")
	}
	if strings.Contains(readText(t, rotated), "AFTER rotation") {
		t.Fatal("written into the rotated file")
	}
}

func TestSessionContextIsolatedPerGoroutine(t *testing.T) {
	h := testHome(t)
	mustSetup(t, Options{Home: h})
	var wg sync.WaitGroup
	ready := make(chan struct{})
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			SetSessionContext(fmt.Sprintf("sess-%d", i))
			<-ready
			slog.Default().With("logger", "agent.isolation").Info(fmt.Sprintf("record-of-%d", i))
			ClearSessionContext()
			slog.Default().With("logger", "agent.isolation").Info(fmt.Sprintf("cleared-of-%d", i))
		}()
	}
	close(ready)
	wg.Wait()
	slog.Default().With("logger", "agent.isolation").Info("main-untagged")
	FlushLogQueue()
	for _, line := range strings.Split(readText(t, filepath.Join(h, "logs", "agent.log")), "\n") {
		for i := range 8 {
			if strings.Contains(line, fmt.Sprintf("record-of-%d", i)) && !strings.Contains(line, fmt.Sprintf(" INFO [sess-%d] agent.isolation: ", i)) {
				t.Fatalf("record %d tagged wrong: %q", i, line)
			}
			if strings.Contains(line, fmt.Sprintf("cleared-of-%d", i)) && strings.Contains(line, "[sess-") {
				t.Fatalf("cleared record still tagged: %q", line)
			}
		}
		if strings.Contains(line, "main-untagged") && !strings.Contains(line, " INFO agent.isolation: main-untagged") {
			t.Fatalf("main record %q", line)
		}
	}
}
