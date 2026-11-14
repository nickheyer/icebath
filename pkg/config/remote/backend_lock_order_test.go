package remote

import (
	"bytes"
	"context"
	"errors"
	"github.com/nickheyer/icebath/pkg/testfakes/configplane"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
)

// Whether a goroutine other than the caller waits for a fetch lock inside the boot fetch
func blockedInState() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*remoteBackend).bootFetch") && strings.Contains(g, "sync.(*Mutex).Lock") {
			return true
		}
	}
	return false
}

type reentry struct {
	msg  string
	took time.Duration
}

// A boot path read of its own profile fails at once while another goroutine waits for and shares the fetch
func TestSameThreadReentryRaisesWhileOtherThreadsWait(t *testing.T) {
	p := setupPlane(t)
	t.Setenv(idpEnvPrefix+"TOKEN_URL", "")
	b := testBackend()
	h := p.Home
	var mu sync.Mutex
	var reentries []reentry
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(reentries) }
	reenter := func(ctx context.Context) {
		started := time.Now()
		_, err := b.Version(ctx, h)
		var unavailable *config.ConfigBackendUnavailable
		if errors.As(err, &unavailable) {
			mu.Lock()
			reentries = append(reentries, reentry{err.Error(), time.Since(started)})
			mu.Unlock()
		}
	}
	RegisterNousAccessToken(func(ctx context.Context, _ string) (string, error) {
		if count() == 0 {
			reenter(ctx)
		}
		return configplane.NousToken, nil
	})
	t.Cleanup(func() { RegisterNousAccessToken(nil) })
	var other *profileState
	waited := make(chan struct{})
	original := request
	request = func(ctx context.Context, method, rh, profile, etag string, body any, token string) (Response, error) {
		if method == "GET" && count() == 1 {
			reenter(ctx)
			go func() {
				defer close(waited)
				other, _ = b.state(context.Background(), h)
			}()
			deadline := time.Now().Add(5 * time.Second)
			for !blockedInState() {
				if time.Now().After(deadline) {
					t.Error("the other goroutine never waited")
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
		return original(ctx, method, rh, profile, etag, body, token)
	}
	t.Cleanup(func() { request = original })
	p.Upper = map[string]any{"display": map[string]any{"skin": "ares"}}
	doc, err := b.ReadUserDoc(context.Background(), h)
	if err != nil || doc.(map[string]any)["display"].(map[string]any)["skin"] != "ares" {
		t.Fatalf("read %v %v", doc, err)
	}
	if len(reentries) != 2 {
		t.Fatalf("reentries %v", reentries)
	}
	for _, r := range reentries {
		if !strings.Contains(r.msg, "re-entered") || r.took >= time.Second {
			t.Fatalf("reentry %v", r)
		}
	}
	waitOrFail(t, waited)
	st, _ := b.state(context.Background(), h)
	if other != st || len(p.Gets()) != 1 {
		t.Fatalf("shared state %v gets %d", other == st, len(p.Gets()))
	}
	if _, err := b.Version(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

type seenLog struct {
	msg  string
	held bool
}

// Records each warning with whether a fetch lock or profile state lock was held when it was emitted
type lockProbe struct {
	b    *remoteBackend
	mu   *sync.Mutex
	seen *[]seenLog
}

func (h lockProbe) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelWarn }
func (h lockProbe) WithAttrs([]slog.Attr) slog.Handler               { return h }
func (h lockProbe) WithGroup(string) slog.Handler                    { return h }

func (h lockProbe) Handle(_ context.Context, r slog.Record) error {
	held := false
	h.b.mu.Lock()
	locks := make([]*sync.Mutex, 0, len(h.b.fetchLocks)+len(h.b.states))
	for _, l := range h.b.fetchLocks {
		locks = append(locks, l)
	}
	for _, st := range h.b.states {
		locks = append(locks, &st.mu)
	}
	h.b.mu.Unlock()
	for _, l := range locks {
		if l.TryLock() {
			l.Unlock()
		} else {
			held = true
		}
	}
	h.mu.Lock()
	*h.seen = append(*h.seen, seenLog{r.Message, held})
	h.mu.Unlock()
	return nil
}

// The boot retry warning and the inherited key warning of a write are emitted with no backend lock held
func TestNothingIsLoggedUnderABackendLock(t *testing.T) {
	p := setupPlane(t)
	b := testBackend()
	var mu sync.Mutex
	var seen []seenLog
	prev := slog.Default()
	slog.SetDefault(slog.New(lockProbe{b: b, mu: &mu, seen: &seen}))
	t.Cleanup(func() { slog.SetDefault(prev) })
	original := request
	request = func(ctx context.Context, method, h, profile, etag string, body any, token string) (Response, error) {
		resp, err := original(ctx, method, h, profile, etag, body, token)
		p.Mu.Lock()
		p.FailStatus = 0
		p.Mu.Unlock()
		return resp, err
	}
	t.Cleanup(func() { request = original })
	p.Mu.Lock()
	p.Upper = map[string]any{"display": map[string]any{"personality": "concise"}}
	p.Profile("default").Values = map[string]any{"display": map[string]any{"personality": "pirate"}}
	p.FailStatus = 503
	p.Mu.Unlock()
	if err := writeKey(p, "display.personality", nil); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	retrying, inherited := false, false
	for _, s := range seen {
		retrying = retrying || strings.Contains(s.msg, "retrying")
		inherited = inherited || strings.Contains(s.msg, "still set by an upper level")
		if s.held {
			t.Fatalf("logged under a lock %q", s.msg)
		}
	}
	if !retrying || !inherited {
		t.Fatalf("seen %v", seen)
	}
}

// A set equal to the inherited value is stored at the profile level and outlives an upper change
func TestSetEqualToInheritedValueIsStoredAndSurvivesAnUpperChange(t *testing.T) {
	p := setupPlane(t)
	p.Upper = map[string]any{"display": map[string]any{"skin": "ares"}}
	if err := setValue(p, "display.skin", "ares"); err != nil {
		t.Fatal(err)
	}
	mustEqualJSON(t, "values", profileValues(p, "default"), map[string]any{"display": map[string]any{"skin": "ares"}})
	p.Mu.Lock()
	p.Upper = map[string]any{"display": map[string]any{"skin": "slate"}}
	p.Mu.Unlock()
	ResetForTests()
	doc, err := config.ReadConfigDoc(context.Background(), filepath.Join(p.Home, "config.yaml"))
	if err != nil || doc.(map[string]any)["display"].(map[string]any)["skin"] != "ares" {
		t.Fatalf("read %v %v", doc, err)
	}
}

// Remote set and unset name the Remote Config profile level and never a local config.yaml
func TestRemoteSetAndUnsetNameRemoteConfigNotALocalFile(t *testing.T) {
	p := setupPlane(t)
	var out bytes.Buffer
	prev := config.FacadeStdout
	config.FacadeStdout = &out
	t.Cleanup(func() { config.FacadeStdout = prev })
	ctx := context.Background()
	if err := setValue(p, "agent.max_turns", "7"); err != nil {
		t.Fatal(err)
	}
	if err := config.UnsetConfigValue(ctx, p.Home, "agent.max_turns", cmdSources()); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "✓ Set agent.max_turns = 7 in Remote Config (profile 'default' level)") ||
		!strings.Contains(text, "✓ Unset agent.max_turns from Remote Config (profile 'default' level)") || strings.Contains(text, "config.yaml") {
		t.Fatalf("out %q", text)
	}
	if _, err := os.Stat(filepath.Join(p.Home, "config.yaml")); err == nil {
		t.Fatal("config.yaml written")
	}
}

// Selected from the env alone, a remote backend without its instance id fails the read and never reads config.yaml
func TestUnconfiguredRemoteBackendFailsClosedOnRead(t *testing.T) {
	h := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	if err := os.WriteFile(filepath.Join(h, "config.yaml"), []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(config.BackendEnv, "remote")
	t.Setenv(InstanceEnv, "")
	os.Unsetenv(InstanceEnv)
	ResetForTests()
	t.Cleanup(ResetForTests)
	ctx := context.Background()
	backend, err := config.GetConfigBackend(ctx)
	if err != nil || backend.Name() != "remote" {
		t.Fatalf("backend %v %v", backend, err)
	}
	var unavailable *config.ConfigBackendUnavailable
	if _, err := config.ReadConfigDoc(ctx, filepath.Join(h, "config.yaml")); !errors.As(err, &unavailable) || !strings.Contains(err.Error(), "Remote Config") {
		t.Fatalf("read %v", err)
	}
}

// The remote backend is never a file backend and serves every read and write member of the protocol
func TestRemoteBackendIsNotAFileBackend(t *testing.T) {
	backend := newRemoteBackend()
	if _, isFile := backend.(config.FileBackend); isFile {
		t.Fatal("remote backend is a file backend")
	}
	rb := backend.(*remoteBackend)
	_ = []any{rb.ReadUserDoc, rb.ReadUserDocReadonly, rb.Version, rb.Exists, rb.ProbeReadable, rb.WriteChanges}
}
