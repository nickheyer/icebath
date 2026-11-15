//go:build !windows

package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Authorized adapter on the loopback fake under a launch home with a launch only credential and the real secret source
func gmailTriageAdapter(t *testing.T) (*TelegramAdapter, *scriptedBot, string) {
	t.Helper()
	a, fake := newScriptedAdapter(t, nil)
	a.SetMessageHandler(closureHandler)
	a.SetAuthorizationCheck(func(string, *string, *string, gateway.AuthorizationOpts) (any, error) { return true, nil })
	launch := filepath.Join(t.TempDir(), "launch")
	if err := os.MkdirAll(launch, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", launch)
	prev := tools.ProfileSecrets
	tools.ProfileSecrets = provider.ProfileSecretSource()
	t.Cleanup(func() { tools.ProfileSecrets = prev })
	gmailTriageWrite(t, filepath.Join(launch, ".env"), "LAUNCH_ONLY_TOKEN=launch-secret\n", 0o600)
	t.Setenv("LAUNCH_ONLY_TOKEN", "launch-secret")
	t.Setenv("OPENROUTER_API_KEY", "sk-or-v1-"+strings.Repeat("f", 48))
	for _, name := range []string{"SVC_TOKEN", "SKILL_TOKEN"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	return a, fake, launch
}

func gmailTriageWrite(t *testing.T, path, text string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
}

// Installs an archive script under h that records which probed secrets reached it, returning the record path
func gmailTriageProbeScript(t *testing.T, h string) string {
	t.Helper()
	record := filepath.Join(t.TempDir(), "probe.out")
	gmailTriageWrite(t, filepath.Join(h, "scripts", "gmail-triage", "archive.sh"), "#!/bin/sh\n"+
		`{ [ "$SVC_TOKEN" = served-secret ] && echo svc=served || echo svc=other; `+
		`[ "$SKILL_TOKEN" = skill-secret ] && echo skill=served || echo skill=other; echo "launch=${LAUNCH_ONLY_TOKEN:-MISSING}"; `+
		`echo "key=${OPENROUTER_API_KEY:-}"; echo "home=$ICEBATH_HOME"; echo "arg=$1"; } > '`+record+"'\n", 0o700)
	return record
}

// Taps gt:archive:msg-1 and returns the toast answered and the record the script wrote
func gmailTriageTap(t *testing.T, ctx context.Context, a *TelegramAdapter, fake *scriptedBot, record string) (string, []string) {
	t.Helper()
	update := &Update{Raw: &models.Update{CallbackQuery: &models.CallbackQuery{ID: "cb-1", From: models.User{ID: 42, FirstName: "Ana"},
		Data: "gt:archive:msg-1", Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 7,
			Chat: models.Chat{ID: 123, Type: models.ChatTypePrivate}, Text: "mail"}}}}}
	if err := a.handleCallbackQuery(ctx, update); err != nil {
		t.Fatal(err)
	}
	answers := fake.callsOf("answerCallbackQuery")
	if len(answers) != 1 {
		t.Fatalf("answers %v", answers)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("toast %q and no script record: %v", answers[0].params["text"], err)
	}
	return answers[0].params["text"], strings.Split(strings.TrimSpace(string(raw)), "\n")
}

func TestGmailTriageUnderMultiplexRunsWithTheServedProfilesSecretsNeverTheLaunchResidue(t *testing.T) {
	a, fake, launch := gmailTriageAdapter(t)
	served := filepath.Join(launch, "profiles", "beta")
	gmailTriageWrite(t, filepath.Join(served, ".env"), "SVC_TOKEN=served-secret\nSKILL_TOKEN=skill-secret\n", 0o600)
	t.Setenv("SKILL_TOKEN", "launch-skill")
	tools.RegisterEnvPassthrough(nil, []string{"SKILL_TOKEN"})
	t.Cleanup(tools.ClearEnvPassthrough)
	gmailTriageWrite(t, filepath.Join(served, "config.yaml"), "terminal:\n  env_passthrough: [SVC_TOKEN]\n", 0o600)
	record := gmailTriageProbeScript(t, served)
	provider.SetMultiplexActive(true)
	t.Cleanup(func() { provider.SetMultiplexActive(false) })
	ctx := gateway.ProfileRuntimeScope(context.Background(), served, true)
	toast, got := gmailTriageTap(t, ctx, a, fake, record)
	if want := tr(ctx, "platform.telegram.gmail_triage.done_archive"); toast != want {
		t.Fatalf("toast %q, want %q", toast, want)
	}
	want := []string{"svc=served", "skill=served", "launch=MISSING", "key=", "home=" + served, "arg=msg-1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("script saw %q, want %q", got, want)
	}
}

func TestGmailTriageOfTheLaunchProfileIsScrubbedAndKeepsItsOwnEnvCredential(t *testing.T) {
	a, fake, launch := gmailTriageAdapter(t)
	record := gmailTriageProbeScript(t, launch)
	ctx := context.Background()
	toast, got := gmailTriageTap(t, ctx, a, fake, record)
	if want := tr(ctx, "platform.telegram.gmail_triage.done_archive"); toast != want {
		t.Fatalf("toast %q, want %q", toast, want)
	}
	want := []string{"svc=other", "skill=other", "launch=launch-secret", "key=", "home=" + launch, "arg=msg-1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("script saw %q, want %q", got, want)
	}
}
