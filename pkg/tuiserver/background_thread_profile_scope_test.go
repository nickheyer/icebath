package tuiserver

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Home and secret a callback saw, the error text when the secret read failed closed
type scopeSeen struct{ home, key string }

func observeScope(ctx context.Context) scopeSeen {
	value, _, err := provider.GetSecret(ctx, "TITLE_KEY")
	if err != nil {
		value = err.Error()
	}
	return scopeSeen{home.Dir(ctx), value}
}

// Memory provider recording the scope of the session end commit and of the close
type scopeRecorder struct {
	memory.BaseProvider
	mu             sync.Mutex
	commit, closed *scopeSeen
	done           chan struct{}
	once           sync.Once
}

func (p *scopeRecorder) Name() string                                          { return "scope-recorder" }
func (p *scopeRecorder) Available(context.Context) bool                        { return true }
func (p *scopeRecorder) ToolSchemas(context.Context) ([]map[string]any, error) { return nil, nil }
func (p *scopeRecorder) Initialize(context.Context, string, memory.InitOptions) error {
	return nil
}
func (p *scopeRecorder) OnSessionEnd(ctx context.Context, _ []map[string]any) error {
	seen := observeScope(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.commit == nil {
		p.commit = &seen
	}
	return nil
}
func (p *scopeRecorder) Shutdown(ctx context.Context) error {
	seen := observeScope(ctx)
	p.mu.Lock()
	if p.closed == nil {
		p.closed = &seen
	}
	p.mu.Unlock()
	p.once.Do(func() { close(p.done) })
	return nil
}

func TestWsOrphanReapTearsDownUnderTheSessionsProfile(t *testing.T) {
	a := filepath.Join(t.TempDir(), ".icebath")
	b := filepath.Join(a, "profiles", "b")
	if err := os.MkdirAll(b, 0o755); err != nil {
		t.Fatal(err)
	}
	for dir, key := range map[string]string{a: "a-key", b: "b-key"} {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("TITLE_KEY="+key+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", a)
	t.Setenv("TITLE_KEY", "a-key")
	recorder := &scopeRecorder{done: make(chan struct{})}
	mm := memory.NewMemoryManager(context.Background(), nil, nil)
	if err := mm.AddProvider(context.Background(), recorder); err != nil {
		t.Fatal(err)
	}
	mm.InitializeAll(home.WithOverride(context.Background(), b), "sess-b", memory.InitOptions{Home: b})
	held, err := agent.NewAIAgent(context.Background(), agent.AIAgentOptions{Model: "test-model", Provider: "custom", BaseURL: "http://127.0.0.1:9/v1",
		APIKey: "k", APIMode: "chat_completions", SessionID: "sess-b", Platform: "tui", QuietMode: true, SkipContextFiles: true, MemoryManager: mm})
	if err != nil {
		t.Fatal(err)
	}
	isolateProfileHosting(t)
	provider.SetMultiplexActive(true)
	sid := "ws-b"
	session := tuigateway.NewSession(map[string]any{"agent": held, "history": []any{map[string]any{"role": "user", "content": "x"}},
		"history_lock": &sync.Mutex{}, "session_key": "sess-b", "profile_home": b, "transport": tuigateway.DetachedWSTransport(), "last_active": 0.0})
	swapSessions(t, map[string]*tuigateway.Session{sid: session})
	swapVar(t, &wsOrphanReapGraceS, func() float64 { return 0.01 })
	scheduleWSOrphanReap(context.Background(), sid, nil, nil)
	select {
	case <-recorder.done:
	case <-time.After(10 * time.Second):
		t.Fatal("reap timer never tore the session down")
	}
	recorder.mu.Lock()
	commit, closed := recorder.commit, recorder.closed
	recorder.mu.Unlock()
	want := scopeSeen{b, "b-key"}
	if commit == nil || *commit != want || closed == nil || *closed != want {
		t.Fatalf("commit %v close %v", commit, closed)
	}
	if got := home.Dir(context.Background()); got != a {
		t.Fatalf("test context home %s", got)
	}
}
