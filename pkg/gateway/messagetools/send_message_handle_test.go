package messagetools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
)

type sendCall struct {
	platform gateway.Platform
	pconfig  *gateway.PlatformConfig
	chatID   string
	message  string
	opts     SendOptions
}

// Installs the config loader, platform send and mirror doubles the hermes tests patch in
func installSendSeams(t *testing.T, cfg *gateway.GatewayConfig, send func(sendCall) (map[string]any, error)) *[]sendCall {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	patchDiscovery(t, nil)
	gateway.SetGatewayRunnerRef(nil)
	var calls []sendCall
	prevLoad, prevSend, prevMirror := loadGatewayConfig, sendToPlatformFn, mirrorToSession
	loadGatewayConfig = func(context.Context) (*gateway.GatewayConfig, error) { return cfg, nil }
	sendToPlatformFn = func(_ context.Context, p gateway.Platform, pc *gateway.PlatformConfig, chatID, message string, opts SendOptions) (map[string]any, error) {
		c := sendCall{p, pc, chatID, message, opts}
		calls = append(calls, c)
		if send != nil {
			return send(c)
		}
		return map[string]any{"success": true}, nil
	}
	mirrorToSession = func(context.Context, string, string, string, gateway.MirrorOptions) bool { return true }
	t.Cleanup(func() { loadGatewayConfig, sendToPlatformFn, mirrorToSession = prevLoad, prevSend, prevMirror })
	return &calls
}

func telegramConfig() (*gateway.GatewayConfig, *gateway.PlatformConfig) {
	tg := &gateway.PlatformConfig{Enabled: true, Token: "***", Extra: map[string]any{}}
	return &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformTelegram: tg}}, tg
}

func runTool(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	raw, err := SendMessageTool(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("%v in %s", err, raw)
	}
	return out
}

func assertOneSend(t *testing.T, calls []sendCall, want sendCall) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("calls=%v", calls)
	}
	c := calls[0]
	if c.platform != want.platform || c.pconfig != want.pconfig || c.chatID != want.chatID || c.message != want.message ||
		c.opts.ThreadID != nil || c.opts.ForceDocument || !reflect.DeepEqual(c.opts.MediaFiles, want.opts.MediaFiles) {
		t.Fatalf("call=%+v want=%+v", c, want)
	}
}

// Registers the ntfy platform with the parse_nonempty target syntax its plugin declares
func registerNtfy(t *testing.T) {
	e := gateway.NewPlatformEntry("ntfy", "ntfy", func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil }, func() bool { return true })
	e.TargetRefSyntaxFn = gateway.ParseNonempty
	registerEntry(t, e)
}

func TestSendMessageToolNtfyTopicTargetIsExplicit(t *testing.T) {
	patchDiscovery(t, nil)
	registerNtfy(t)
	chat, thread, explicit := gateway.ParseTargetRef(context.Background(), "ntfy", "alerts-channel")
	if chat == nil || *chat != "alerts-channel" || thread != nil || !explicit {
		t.Fatalf("chat=%v thread=%v explicit=%v", chat, thread, explicit)
	}
}

func TestSendMessageToolNtfyTopicTargetBypassesChannelDirectory(t *testing.T) {
	registerNtfy(t)
	ntfy, err := gateway.ParsePlatform(context.Background(), "ntfy")
	if err != nil {
		t.Fatal(err)
	}
	ntfyCfg := &gateway.PlatformConfig{Enabled: true, Extra: map[string]any{"topic": "hermes-in"}}
	calls := installSendSeams(t, &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{ntfy: ntfyCfg}}, nil)
	prev := gateway.SetResolveChannelNameFn(func(context.Context, string, string) (string, error) {
		panic("should not resolve ntfy topics")
	})
	t.Cleanup(func() { gateway.SetResolveChannelNameFn(prev) })
	res := runTool(t, map[string]any{"action": "send", "target": "ntfy:alerts-channel", "message": "done"})
	if res["success"] != true {
		t.Fatalf("res=%v", res)
	}
	assertOneSend(t, *calls, sendCall{platform: ntfy, pconfig: ntfyCfg, chatID: "alerts-channel", message: "done", opts: SendOptions{MediaFiles: []gateway.MediaRef{}}})
}

func TestSendMessageToolMediaTagOutsideAllowedRootsIsNotSent(t *testing.T) {
	cfg, tg := telegramConfig()
	calls := installSendSeams(t, cfg, nil)
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "1")
	t.Setenv("ICEBATH_MEDIA_TRUST_RECENT_FILES", "0")
	secret := writeTemp(t, "secret.pdf", []byte("%PDF secret"))
	res := runTool(t, map[string]any{"action": "send", "target": "telegram:12345", "message": "hello\nMEDIA:" + secret})
	if res["success"] != false || res["partial_success"] != true {
		t.Fatalf("res=%v", res)
	}
	want := []any{map[string]any{"path": secret, "reason": "denied by the delivery policy"}}
	if !reflect.DeepEqual(res["media_dropped"], want) {
		t.Fatalf("media_dropped=%v", res["media_dropped"])
	}
	assertOneSend(t, *calls, sendCall{platform: gateway.PlatformTelegram, pconfig: tg, chatID: "12345", message: "hello", opts: SendOptions{MediaFiles: []gateway.MediaRef{}}})
}

func TestSendMessageToolMissingMediaIsReportedToTheCallerAndHermesSendExitsNonzero(t *testing.T) {
	cfg, tg := telegramConfig()
	calls := installSendSeams(t, cfg, nil)
	t.Setenv("ICEBATH_MEDIA_DELIVERY_STRICT", "0")
	dir := t.TempDir()
	t.Setenv("ICEBATH_MEDIA_ALLOW_DIRS", dir)
	report := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(report, []byte("%PDF report"), 0o644); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.pdf")
	raw, err := SendMessageTool(context.Background(), map[string]any{"action": "send", "target": "telegram:12345",
		"message": fmt.Sprintf("report\nMEDIA:%s\nMEDIA:%s", report, missing)})
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res["success"] != false || res["partial_success"] != true || res["error"] == "" || res["error"] == nil {
		t.Fatalf("res=%v", res)
	}
	want := []any{map[string]any{"path": missing, "reason": "not found on this host"}}
	if !reflect.DeepEqual(res["media_dropped"], want) {
		t.Fatalf("media_dropped=%v", res["media_dropped"])
	}
	resolved, _ := filepath.EvalSymlinks(report)
	resolved, _ = filepath.Abs(resolved)
	assertOneSend(t, *calls, sendCall{platform: gateway.PlatformTelegram, pconfig: tg, chatID: "12345", message: "report", opts: SendOptions{MediaFiles: []gateway.MediaRef{{Path: resolved}}}})
	var out, errw bytes.Buffer
	if code, err := EmitSendResult(&out, &errw, raw, true, true); err != nil || code == 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestSendMessageToolTopLevelSendFailureRedactsQueryToken(t *testing.T) {
	cfg, _ := telegramConfig()
	leaked := "very-secret-query-token-123456"
	installSendSeams(t, cfg, func(sendCall) (map[string]any, error) {
		return nil, errors.New("transport error: https://api.example.com/send?access_token=" + leaked)
	})
	res := runTool(t, map[string]any{"action": "send", "target": "telegram:-1001", "message": "hello"})
	msg, _ := res["error"].(string)
	if msg == "" || strings.Contains(msg, leaked) || !strings.Contains(msg, "access_token=***") {
		t.Fatalf("res=%v", res)
	}
}

func TestEmailHomeChannelErrorHintEmailErrorNamesEmailHomeAddress(t *testing.T) {
	email := &gateway.PlatformConfig{Enabled: true, Token: "", Extra: map[string]any{}}
	installSendSeams(t, &gateway.GatewayConfig{Platforms: map[gateway.Platform]*gateway.PlatformConfig{gateway.PlatformEmail: email}}, nil)
	res := runTool(t, map[string]any{"action": "send", "target": "email", "message": "hi"})
	msg, _ := res["error"].(string)
	if !strings.Contains(msg, "EMAIL_HOME_ADDRESS") || strings.Contains(msg, "EMAIL_HOME_CHANNEL") {
		t.Fatalf("res=%v", res)
	}
}

func TestResolveSlackUserTargetsConversationIdsPassThroughWithoutApiCalls(t *testing.T) {
	prev := slackAPIBase
	slackAPIBase = "http://127.0.0.1:1/unreachable/"
	t.Cleanup(func() { slackAPIBase = prev })
	for _, cid := range []string{"C0B0QV5434G", "G123ABCDEF", "D123ABCDEF"} {
		chat, errPayload := resolveSlackUserTarget(context.Background(), "tok", cid)
		if chat != cid || errPayload != nil {
			t.Fatalf("cid=%s chat=%q err=%v", cid, chat, errPayload)
		}
	}
}

func TestResolveSlackUserTargetsConversationsOpenFailureSurfacesError(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": false, "error": "missing_scope"}`))
	}))
	t.Cleanup(srv.Close)
	prev := slackAPIBase
	slackAPIBase = srv.URL + "/api/"
	t.Cleanup(func() { slackAPIBase = prev })
	t.Setenv("ICEBATH_HOME", t.TempDir())
	chat, errPayload := resolveSlackUserTarget(context.Background(), "tok", "user:U123ABCDEF")
	msg, _ := errPayload["error"].(string)
	if chat != "" || !strings.Contains(msg, "missing_scope") || !strings.Contains(msg, "im:write") {
		t.Fatalf("chat=%q err=%v", chat, errPayload)
	}
	if !reflect.DeepEqual(paths, []string{"/api/conversations.open"}) {
		t.Fatalf("paths=%v", paths)
	}
}

func TestNotConfiguredErrorNamesResolvedHomeAndConsultedSources(t *testing.T) {
	h := filepath.Join(t.TempDir(), "AppData", "Local", "icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(h, ".env"), []byte("FIRECRAWL_API_KEY=x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(h, "config.yaml"), []byte("platforms:\n  discord:\n    enabled: false\n"), 0o644)
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	os.Unsetenv("DISCORD_BOT_TOKEN")
	patchDiscovery(t, nil)
	_, _, _, msg, err := resolvePlatformConfig(context.Background(), "discord", &gateway.GatewayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msg, "~/.icebath") || !strings.Contains(msg, filepath.Join(h, ".env")) || !strings.Contains(msg, filepath.Join(h, "config.yaml")) {
		t.Fatalf("msg=%s", msg)
	}
}

func TestNotConfiguredErrorNamesDefaultRootGatewayAndSecretSources(t *testing.T) {
	root := filepath.Join(t.TempDir(), "icebath")
	profile := filepath.Join(root, "profiles", "coder")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf(`{"pid": %d, "platforms": {"discord": {"state": "connected"}}}`, os.Getpid())
	_ = os.WriteFile(filepath.Join(root, "gateway_state.json"), []byte(state), 0o644)
	_ = os.WriteFile(filepath.Join(profile, ".env"), []byte("FIRECRAWL_API_KEY=x\n"), 0o644)
	_ = os.WriteFile(filepath.Join(profile, "config.yaml"), []byte("secrets:\n  bitwarden:\n    enabled: false\n    session_token: SECRET-VALUE\n"), 0o644)
	t.Setenv("ICEBATH_HOME", profile)
	t.Setenv("DISCORD_BOT_TOKEN", "")
	os.Unsetenv("DISCORD_BOT_TOKEN")
	patchDiscovery(t, nil)
	_, _, _, msg, err := resolvePlatformConfig(context.Background(), "discord", &gateway.GatewayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, fmt.Sprint(os.Getpid())) || !strings.Contains(msg, profile) || !strings.Contains(msg, "bitwarden") || strings.Contains(msg, "SECRET-VALUE") {
		t.Fatalf("msg=%s", msg)
	}
}

type fakePhotonAdapter struct {
	*gateway.BasePlatformAdapter
	calls [][]any
}

var errPhotonDoubleTransport = errors.New("the photon double carries reactions only")

func (a *fakePhotonAdapter) Connect(context.Context, bool) (bool, error) {
	return false, errPhotonDoubleTransport
}
func (a *fakePhotonAdapter) Disconnect(context.Context) error { return errPhotonDoubleTransport }
func (a *fakePhotonAdapter) Send(context.Context, string, string, string, map[string]any) (gateway.SendResult, error) {
	return gateway.SendResult{}, errPhotonDoubleTransport
}
func (a *fakePhotonAdapter) GetChatInfo(context.Context, string) (map[string]any, error) {
	return nil, errPhotonDoubleTransport
}

// Photon double registered as a live adapter of the runner's default profile map
func newFakePhotonAdapter() *fakePhotonAdapter {
	a := &fakePhotonAdapter{}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(gateway.NewPlatformConfig(), gateway.Platform("photon"), a)
	return a
}

func (a *fakePhotonAdapter) AddReaction(_ context.Context, chatID, messageID, emoji string) (any, error) {
	a.calls = append(a.calls, []any{"add", chatID, emoji, messageID})
	return map[string]any{"success": true, "emoji": emoji}, nil
}

func (a *fakePhotonAdapter) RemoveReaction(_ context.Context, chatID, messageID string) (any, error) {
	a.calls = append(a.calls, []any{"remove", chatID, messageID})
	return map[string]any{"success": true}, nil
}

// Registers the photon plugin platform so its name parses as a Platform member
func registerPhoton(t *testing.T) {
	registerEntry(t, gateway.NewPlatformEntry("photon", "Photon", func(context.Context, *gateway.PlatformConfig) (any, error) { return nil, nil }, func() bool { return true }))
}

func TestReactDispatchesToAddReaction(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	patchDiscovery(t, nil)
	registerPhoton(t)
	a := newFakePhotonAdapter()
	gateway.SetGatewayRunnerRef(newMapRunner(gateway.Platform("photon"), a))
	t.Cleanup(func() { gateway.SetGatewayRunnerRef(nil) })
	res := runTool(t, map[string]any{"action": "react", "target": "photon:+15551234567", "emoji": "❤️"})
	if res["success"] != true {
		t.Fatalf("res=%v", res)
	}
	if want := [][]any{{"add", "+15551234567", "❤️", ""}}; !reflect.DeepEqual(a.calls, want) {
		t.Fatalf("calls=%v", a.calls)
	}
}

func TestReactWithoutLiveGateway(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	patchDiscovery(t, nil)
	registerPhoton(t)
	gateway.SetGatewayRunnerRef(nil)
	raw, err := SendMessageTool(context.Background(), map[string]any{"action": "react", "target": "photon:+15551234567", "emoji": "👍"})
	if err != nil {
		t.Fatal(err)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(raw), &res)
	if res["success"] == true || !strings.Contains(raw, "live") {
		t.Fatalf("raw=%s", raw)
	}
}

// Whether the text holds a WTF-8 encoded surrogate code point
func hasSurrogate(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf {
			return true
		}
	}
	return false
}

func sendSurrogateCase(t *testing.T, message string) (map[string]any, []sendCall) {
	cfg, _ := telegramConfig()
	calls := installSendSeams(t, cfg, nil)
	res := runTool(t, map[string]any{"action": "send", "target": "telegram:12345", "message": message})
	return res, *calls
}

func TestLoneSurrogatesAreScrubbedBeforeThePlatformSend(t *testing.T) {
	res, calls := sendSurrogateCase(t, "警报 \xed\xa0\x80\xed\xbf\xbf done")
	if res["success"] != true || len(calls) != 1 {
		t.Fatalf("res=%v calls=%v", res, calls)
	}
	delivered := calls[0].message
	if hasSurrogate(delivered) || delivered != "警报 �� done" {
		t.Fatalf("delivered=%q", delivered)
	}
}

func TestSurrogateFreeMessageIsSentUnchanged(t *testing.T) {
	res, calls := sendSurrogateCase(t, "all clean: 你好 \U0001f44d")
	if res["success"] != true || len(calls) != 1 || calls[0].message != "all clean: 你好 \U0001f44d" {
		t.Fatalf("res=%v calls=%v", res, calls)
	}
}
