package gatewaycli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

func sendHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.Mkdir(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func writeHomeFile(t *testing.T, h, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWhatsappMentionRejectionsNeverReachTheBridge(t *testing.T) {
	group := "whatsapp:120363000000000000@g.us"
	arabic := "١٥٥٥٠٠٠٠٠٠١"
	for name, argv := range map[string][2]string{"other platform": {"telegram", "15550000001"}, "not a phone": {group, "not-a-phone"},
		"non ascii digits": {group, arabic}, "non ascii jid": {group, arabic + "@s.whatsapp.net"}} {
		t.Run(name, func(t *testing.T) {
			h := sendHome(t)
			var out, errOut bytes.Buffer
			ns := map[string]any{"to": argv[0], "mentions": []string{argv[1]}, "message": "hello", "file": nil, "subject": nil,
				"list_targets": false, "quiet": false, "json": false}
			code, err := cmdSend(context.Background(), h, ns, strings.NewReader(""), &out, &errOut)
			if err != nil || code != 2 {
				t.Fatalf("exit %d %v", code, err)
			}
			if !strings.Contains(strings.ToLower(errOut.String()), "mention") || out.Len() != 0 {
				t.Fatalf("stderr %q stdout %q", errOut.String(), out.String())
			}
		})
	}
}

func TestListIncludesConfiguredPlatformWithoutDiscoveredChannels(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, "config.yaml", []byte("platforms:\n  telegram:\n    enabled: true\n    token: \"123:configured\"\n"))
	var out, errOut bytes.Buffer
	code, err := listTargets(context.Background(), h, nil, false, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v %s", code, err, errOut.String())
	}
	if !strings.Contains(out.String(), "telegram") || !strings.Contains(out.String(), "no channels discovered yet") {
		t.Fatalf("listing %q", out.String())
	}
}

func TestListJsonIncludesConfiguredPlatform(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, "channel_directory.json", []byte(`{"updated_at": null, "platforms": {"telegram": [{"id": "1", "name": "home"}]}}`))
	writeHomeFile(t, h, "config.yaml", []byte("platforms:\n  discord:\n    enabled: true\n    token: configured\n"))
	t.Setenv("API_SERVER_ENABLED", "true")
	var out, errOut bytes.Buffer
	code, err := listTargets(context.Background(), h, nil, true, &out, &errOut)
	if err != nil || code != 0 {
		t.Fatalf("exit %d %v %s", code, err, errOut.String())
	}
	decoded, err := pyjson.Loads(out.String())
	if err != nil {
		t.Fatal(err)
	}
	platforms, _ := pyjson.AsObject(decoded.(*pyjson.Object).Vals["platforms"])
	discord, has := platforms.Get("discord")
	if list, ok := discord.([]any); !has || !ok || len(list) != 0 {
		t.Fatalf("discord %v in %s", discord, out.String())
	}
	for _, infra := range []string{"local", "api_server", "webhook"} {
		if _, has := platforms.Get(infra); has {
			t.Fatalf("%s listed in %s", infra, out.String())
		}
	}
	if telegram, _ := platforms.Vals["telegram"].([]any); len(telegram) == 0 {
		t.Fatalf("discovered entries dropped in %s", out.String())
	}
}

func clearEnv(t *testing.T, names ...string) {
	t.Helper()
	unsetEnv(t, names...)
}

func TestLoadHermesEnvBridgesConfigYamlScalars(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("SOME_TOKEN=abc123\n"))
	writeHomeFile(t, h, "config.yaml", []byte("TELEGRAM_HOME_CHANNEL: '5550001111'\nnested:\n  ignored: true\n"))
	clearEnv(t, "TELEGRAM_HOME_CHANNEL", "SOME_TOKEN", "nested")
	loadHermesEnv(context.Background(), h)
	if got := os.Getenv("SOME_TOKEN"); got != "abc123" {
		t.Fatalf("SOME_TOKEN %q", got)
	}
	if got := os.Getenv("TELEGRAM_HOME_CHANNEL"); got != "5550001111" {
		t.Fatalf("TELEGRAM_HOME_CHANNEL %q", got)
	}
}

func TestLoadHermesEnvUtf8BomPreservesFirstKey(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("\xef\xbb\xbfSEND_BOM_BOT_TOKEN=tok-first\nSEND_BOM_SECOND=two\n"))
	clearEnv(t, "SEND_BOM_BOT_TOKEN", "SEND_BOM_SECOND", "\xef\xbb\xbfSEND_BOM_BOT_TOKEN")
	loadHermesEnv(context.Background(), h)
	if os.Getenv("SEND_BOM_BOT_TOKEN") != "tok-first" || os.Getenv("SEND_BOM_SECOND") != "two" {
		t.Fatalf("loaded %q %q", os.Getenv("SEND_BOM_BOT_TOKEN"), os.Getenv("SEND_BOM_SECOND"))
	}
	if _, has := os.LookupEnv("\xef\xbb\xbfSEND_BOM_BOT_TOKEN"); has {
		t.Fatal("the BOM stayed on the first key")
	}
}

func TestLoadHermesEnvBomlessUtf8StillLoads(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("SEND_PLAIN_TOKEN=plain-val\n"))
	clearEnv(t, "SEND_PLAIN_TOKEN")
	loadHermesEnv(context.Background(), h)
	if got := os.Getenv("SEND_PLAIN_TOKEN"); got != "plain-val" {
		t.Fatalf("SEND_PLAIN_TOKEN %q", got)
	}
}

func TestLoadHermesEnvLatin1FallbackStillLoads(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("\xef\xbb\xbfSEND_L1_TOKEN=tok-l1\nSEND_L1_NOTE=caf\xe9\n"))
	clearEnv(t, "SEND_L1_TOKEN", "SEND_L1_NOTE", "\xef\xbb\xbfSEND_L1_TOKEN")
	loadHermesEnv(context.Background(), h)
	if os.Getenv("SEND_L1_TOKEN") != "tok-l1" || os.Getenv("SEND_L1_NOTE") != "café" {
		t.Fatalf("loaded %q %q", os.Getenv("SEND_L1_TOKEN"), os.Getenv("SEND_L1_NOTE"))
	}
	if _, has := os.LookupEnv("\xef\xbb\xbfSEND_L1_TOKEN"); has {
		t.Fatal("the BOM stayed on the first key")
	}
}

func TestLoadHermesEnvLatin1FallbackOverridesShell(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("SEND_OVR_LABEL=caf\xe9-file\n"))
	t.Setenv("SEND_OVR_LABEL", "stale-shell-value")
	loadHermesEnv(context.Background(), h)
	if got := os.Getenv("SEND_OVR_LABEL"); got != "café-file" {
		t.Fatalf("SEND_OVR_LABEL %q", got)
	}
}

func TestLoadHermesEnvFallbackReadErrorIsSwallowed(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("SEND_ERR_TOKEN=caf\xe9\n"))
	clearEnv(t, "SEND_ERR_TOKEN")
	if err := os.Chmod(filepath.Join(h, ".env"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(h, ".env"), 0o644) })
	loadHermesEnv(context.Background(), h)
}

func TestLoadHermesEnvBomOnlyEnvIsNoop(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, ".env", []byte("\xef\xbb\xbf"))
	before := map[string]bool{}
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		before[name] = true
	}
	loadHermesEnv(context.Background(), h)
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); !before[name] && strings.Contains(kv, "\xef\xbb\xbf") {
			t.Fatalf("added %q", kv)
		}
	}
}

func TestSendResultFormatsFollowTheRequestedMode(t *testing.T) {
	for _, tc := range []struct {
		name, result       string
		jsonMode, quiet    bool
		code               int
		wantOut, wantErrLn string
	}{
		{"sent", `{"success": true, "message_id": "m123"}`, false, false, 0, "sent\n", ""},
		{"note", `{"success": true, "note": "queued for later"}`, false, false, 0, "queued for later\n", ""},
		{"quiet", `{"success": true}`, false, true, 0, "", ""},
		{"skipped", `{"skipped": true}`, false, false, 0, "{\n  \"skipped\": true\n}\n", ""},
		{"error", `{"error": "no home channel"}`, false, false, 1, "", "icebath send: no home channel\n"},
		{"error wins over success", `{"error": "partial", "success": true}`, false, true, 1, "", ""},
		{"json", `{"success": true}`, true, false, 0, "{\n  \"success\": true\n}\n", ""},
		{"empty", "", false, false, 1, "{}\n", ""},
		{"not json", "boom", false, false, 1, "", "icebath send: invalid JSON from send_message_tool\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			code, err := emitResult(tc.result, tc.jsonMode, tc.quiet, &out, &errOut)
			if err != nil || code != tc.code || out.String() != tc.wantOut || errOut.String() != tc.wantErrLn {
				t.Fatalf("code %d err %v out %q stderr %q", code, err, out.String(), errOut.String())
			}
		})
	}
}

func TestSendReadsItsBodyFromTheArgumentThenTheFileThenStdin(t *testing.T) {
	h := sendHome(t)
	writeHomeFile(t, h, "body.md", []byte("\xef\xbb\xbfline one\r\nline two\r"))
	writeHomeFile(t, h, "photo.png", []byte("\x89PNG\r\n\x1a\n"))
	body := filepath.Join(h, "body.md")
	if text, ok, err := readMessageBody("typed", body, strings.NewReader("piped")); err != nil || !ok || text != "typed" {
		t.Fatal(text, ok, err)
	}
	if text, ok, err := readMessageBody(nil, body, strings.NewReader("piped")); err != nil || !ok || text != "line one\nline two\n" {
		t.Fatalf("%q %v %v", text, ok, err)
	}
	if text, ok, err := readMessageBody(nil, "-", strings.NewReader("forced\r\nstdin")); err != nil || !ok || text != "forced\nstdin" {
		t.Fatalf("%q %v %v", text, ok, err)
	}
	if text, ok, err := readMessageBody(nil, nil, strings.NewReader("piped")); err != nil || !ok || text != "piped" {
		t.Fatalf("%q %v %v", text, ok, err)
	}
	if _, ok, err := readMessageBody(nil, nil, strings.NewReader("")); err != nil || ok {
		t.Fatal(ok, err)
	}
	photo := filepath.Join(h, "photo.png")
	_, _, err := readMessageBody(nil, photo, strings.NewReader(""))
	var exit *sendExit
	if !errors.As(err, &exit) || exit.code != 2 || !strings.Contains(exit.msg, "icebath send: "+photo+" is not a text file") ||
		!strings.Contains(exit.msg, `icebath send --to telegram "[[as_document]] MEDIA:`+photo+`"`) {
		t.Fatalf("error %v", err)
	}
	missing := filepath.Join(h, "absent.txt")
	_, _, err = readMessageBody(nil, missing, strings.NewReader(""))
	if !errors.As(err, &exit) || exit.code != 2 || exit.msg != "icebath send: cannot read "+missing+": [Errno 2] No such file or directory: '"+missing+"'" {
		t.Fatalf("error %v", err)
	}
}

func TestSendUsageErrorsExitTwoAndListFiltersByPlatform(t *testing.T) {
	ctx := context.Background()
	h := sendHome(t)
	writeHomeFile(t, h, "channel_directory.json", []byte(`{"updated_at": null, "platforms": {"telegram": [{"id": "1", "name": "home"}, {"id": "ops", "name": "ops"}], "slack": []}}`))
	base := func() map[string]any {
		return map[string]any{"to": nil, "mentions": nil, "message": nil, "file": nil, "subject": nil, "list_targets": false, "quiet": false, "json": false}
	}
	var out, errOut bytes.Buffer
	if code, err := cmdSend(ctx, h, base(), strings.NewReader(""), &out, &errOut); err != nil || code != 2 ||
		!strings.HasPrefix(errOut.String(), "icebath send: --to PLATFORM[:channel[:thread]] is required\n") {
		t.Fatalf("exit %d %v %q", code, err, errOut.String())
	}
	errOut.Reset()
	ns := base()
	ns["to"] = " telegram "
	if code, err := cmdSend(ctx, h, ns, strings.NewReader(" \n"), &out, &errOut); err != nil || code != 2 ||
		!strings.HasPrefix(errOut.String(), "icebath send: no message provided.") {
		t.Fatalf("exit %d %v %q", code, err, errOut.String())
	}
	errOut.Reset()
	ns = base()
	ns["list_targets"], ns["message"] = true, "Telegram"
	if code, err := cmdSend(ctx, h, ns, strings.NewReader(""), &out, &errOut); err != nil || code != 0 ||
		out.String() != "telegram:\n  telegram:home  [1]\n  telegram:ops\n\n" {
		t.Fatalf("exit %d %v %q %q", code, err, out.String(), errOut.String())
	}
	out.Reset()
	ns["message"] = "slack"
	if code, err := cmdSend(ctx, h, ns, strings.NewReader(""), &out, &errOut); err != nil || code != 0 ||
		out.String() != "slack:\n  (no channels discovered yet)\n" {
		t.Fatalf("exit %d %v %q", code, err, out.String())
	}
	out.Reset()
	ns["message"] = "matrix"
	if code, err := cmdSend(ctx, h, ns, strings.NewReader(""), &out, &errOut); err != nil || code != 1 ||
		errOut.String() != "icebath send: no targets found for platform 'matrix'. Configured: slack, telegram\n" {
		t.Fatalf("exit %d %v %q", code, err, errOut.String())
	}
}
