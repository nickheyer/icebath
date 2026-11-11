package gatewayrun

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydict"
	"github.com/nickheyer/icebath/pkg/kanban"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Ping recorded with the home and media policy active when it was sent
type profilePing struct {
	text, home string
	strict     bool
}

// Telegram adapter recording each ping with its active home and strict media policy
type profileRecordingAdapter struct {
	*gateway.BasePlatformAdapter
	mu   sync.Mutex
	sent []profilePing
}

func (a *profileRecordingAdapter) Connect(context.Context, bool) (bool, error) { return true, nil }
func (a *profileRecordingAdapter) Disconnect(context.Context) error            { return nil }
func (a *profileRecordingAdapter) Send(ctx context.Context, _, content, _ string, _ map[string]any) (gateway.SendResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sent = append(a.sent, profilePing{content, home.Dir(ctx), gateway.MediaDeliveryStrict(ctx)})
	return gateway.SendResult{Success: true}, nil
}
func (a *profileRecordingAdapter) HandleMessage(_ context.Context, event *gateway.MessageEvent) error {
	event.SetGatewayAccepted(true)
	return nil
}
func (a *profileRecordingAdapter) GetChatInfo(_ context.Context, chatID string) (map[string]any, error) {
	return map[string]any{"id": chatID}, nil
}

// Profile recording Telegram adapter
func newProfileRecordingAdapter() *profileRecordingAdapter {
	a := &profileRecordingAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.PlatformTelegram, a)
	return a
}

func TestNotifierPingsRunUnderTheSubscribersProfile(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, ".icebath")
	alpha := filepath.Join(root, "profiles", "alpha")
	if err := os.MkdirAll(alpha, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, ".env"):         "ICEBATH_MODEL=default-model\nTERMINAL_ENV=docker\n",
		filepath.Join(root, "config.yaml"):  "gateway:\n  multiplex_profiles: true\nterminal:\n  backend: docker\n",
		filepath.Join(alpha, ".env"):        "",
		filepath.Join(alpha, "config.yaml"): "display:\n  language: zh\ngateway:\n  strict: true\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ICEBATH_HOME", root)
	t.Setenv("ICEBATH_MODEL", "default-model")
	t.Setenv("TERMINAL_ENV", "docker")
	t.Setenv("ICEBATH_KANBAN_DB", filepath.Join(tmp, "board.db"))
	provider.SetMultiplexActive(true)
	defer provider.SetMultiplexActive(false)

	ctx := context.Background()
	if _, err := kanban.InitDB(ctx, "", nil); err != nil {
		t.Fatal(err)
	}
	conn := kanbanTestConn(t)
	tid := kanbanCreate(t, conn, "notify parity", "alpha", nil)
	kanbanSubscribe(t, conn, kanban.NotifySubOptions{TaskID: tid, Platform: "telegram", ChatID: "1001", NotifierProfile: kstr("alpha")})
	kanbanComplete(t, conn, tid, "done")

	r := makeKanbanRunner(t, newProfileRecordingAdapter())
	r.config = gateway.DefaultGatewayConfig(ctx)
	on := true
	r.config.MultiplexProfiles = &on
	r.config.ProfileRoutes = nil
	alphaAdapter := newProfileRecordingAdapter()
	r.profileAdapters = pydict.New[string, *pydict.Dict[gateway.Platform, gateway.PlatformAdapter]]()
	r.profileAdapters.Set("alpha", adapterDict(gateway.PlatformTelegram, alphaAdapter))
	r.primaryProfileName = "default"
	runOneNotifierTick(t, r)
	if len(alphaAdapter.sent) != 1 || alphaAdapter.sent[0].home != alpha || !alphaAdapter.sent[0].strict {
		t.Fatalf("sent %+v", alphaAdapter.sent)
	}
}
