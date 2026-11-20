package slack

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

// Home for a manifest run with the namespace the slack manifest parser yields for unset flags
func manifestWorld(t *testing.T, overrides map[string]any) (context.Context, map[string]any) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	ns := map[string]any{"slack_command": "manifest", "write": nil, "name": nil, "description": nil, "long_description": nil,
		"long_description_file": nil, "slashes_only": false, "no_assistant": false, "agent_view": false}
	for key, value := range overrides {
		ns[key] = value
	}
	return context.Background(), ns
}

func stringList(t *testing.T, value any) []string {
	t.Helper()
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("not a list: %v", value)
	}
	out := []string{}
	for _, item := range items {
		out = append(out, item.(string))
	}
	return out
}

func TestSlackDispatcherPropagatesManifestFailure(t *testing.T) {
	ctx, ns := manifestWorld(t, map[string]any{"long_description": strings.Repeat("x", 174)})
	var out, errOut bytes.Buffer
	code, err := CmdSlack(ctx, ns, &out, &errOut)
	if err != nil || code != 2 {
		t.Fatalf("code %d err %v", code, err)
	}
}

func TestSlackManifestArgparseLongDescriptionFilePreservesNewlines(t *testing.T) {
	content := strings.Repeat("x", 175) + "\r\n" + strings.Repeat("y", 175) + "\r"
	source := filepath.Join(t.TempDir(), "AGENTS.md")
	if err := os.WriteFile(source, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, ns := manifestWorld(t, map[string]any{"long_description_file": source})
	var out, errOut bytes.Buffer
	if code, err := ManifestCommand(ctx, ns, &out, &errOut); err != nil || code != 0 {
		t.Fatalf("code %d err %v stderr %s", code, err, errOut.String())
	}
	manifest, err := pyjson.LoadsDict(out.String())
	if err != nil {
		t.Fatal(err)
	}
	display := manifest.GetOr("display_information", nil).(*pyjson.Object)
	if display.GetOr("long_description", nil) != content {
		t.Fatalf("long description = %q", display.GetOr("long_description", nil))
	}
}

func TestSlackManifestArgparseLongDescriptionFileReportsTildeExpansionErrors(t *testing.T) {
	source := "~icebath-user-that-does-not-exist-20260716/AGENTS.md"
	ctx, ns := manifestWorld(t, map[string]any{"long_description_file": source})
	var out, errOut bytes.Buffer
	if code, err := ManifestCommand(ctx, ns, &out, &errOut); err != nil || code != 2 {
		t.Fatalf("code %d err %v", code, err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "cannot read long description") || !strings.Contains(errOut.String(), source) {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
}

func TestSlackFullManifestAssistantFeaturesRemainEnabled(t *testing.T) {
	ctx, _ := manifestWorld(t, nil)
	manifest, err := BuildFullManifest(ctx, "Icebath", "Your Icebath agent on Slack", true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	features := manifest.GetOr("features", nil).(*pyjson.Object)
	if _, ok := features.Get("assistant_view"); !ok {
		t.Fatal("assistant_view missing")
	}
	if _, ok := features.Get("agent_view"); ok {
		t.Fatal("agent_view present")
	}
	scopes := stringList(t, manifest.GetOr("oauth_config", nil).(*pyjson.Object).GetOr("scopes", nil).(*pyjson.Object).GetOr("bot", nil))
	events := stringList(t, manifest.GetOr("settings", nil).(*pyjson.Object).GetOr("event_subscriptions", nil).(*pyjson.Object).GetOr("bot_events", nil))
	if !slices.Contains(scopes, "assistant:write") || !slices.Contains(events, "assistant_thread_started") {
		t.Fatalf("scopes %v events %v", scopes, events)
	}
}

func TestSlackFullManifestNoAssistantPreservesCoreSurface(t *testing.T) {
	ctx, _ := manifestWorld(t, nil)
	manifest, err := BuildFullManifest(ctx, "Icebath", "Your Icebath agent on Slack", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	features := manifest.GetOr("features", nil).(*pyjson.Object)
	if features.GetOr("app_home", nil).(*pyjson.Object).GetOr("messages_tab_enabled", nil) != true {
		t.Fatal("the Messages tab is not writable")
	}
	if slashes, _ := features.GetOr("slash_commands", nil).([]any); len(slashes) == 0 {
		t.Fatal("no slash commands")
	}
	settings := manifest.GetOr("settings", nil).(*pyjson.Object)
	if settings.GetOr("socket_mode_enabled", nil) != true {
		t.Fatal("socket mode disabled")
	}
	scopes := stringList(t, manifest.GetOr("oauth_config", nil).(*pyjson.Object).GetOr("scopes", nil).(*pyjson.Object).GetOr("bot", nil))
	for _, scope := range []string{"commands", "channels:history", "groups:read", "im:history"} {
		if !slices.Contains(scopes, scope) {
			t.Fatalf("scope %s missing from %v", scope, scopes)
		}
	}
	events := stringList(t, settings.GetOr("event_subscriptions", nil).(*pyjson.Object).GetOr("bot_events", nil))
	for _, event := range []string{"message.im", "message.channels", "message.groups", "app_mention"} {
		if !slices.Contains(events, event) {
			t.Fatalf("event %s missing from %v", event, events)
		}
	}
}

func TestSlackManifestWritesToTheHomeAndToAPath(t *testing.T) {
	ctx, ns := manifestWorld(t, map[string]any{"write": true, "slashes_only": true})
	var out, errOut bytes.Buffer
	if code, err := ManifestCommand(ctx, ns, &out, &errOut); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	target := filepath.Join(os.Getenv("ICEBATH_HOME"), "slack-manifest.json")
	raw, err := os.ReadFile(target)
	if err != nil || !strings.HasPrefix(string(raw), "[\n  {\n    \"command\": \"/") || !strings.HasSuffix(string(raw), "]\n") {
		t.Fatalf("manifest file %q %v", raw, err)
	}
	if out.Len() != 0 || !strings.HasPrefix(errOut.String(), "Slack manifest written to: "+target+"\n\nNext steps:\n") {
		t.Fatalf("stdout %q stderr %q", out.String(), errOut.String())
	}
	other := filepath.Join(t.TempDir(), "nested", "manifest.json")
	ns["write"] = other
	if code, err := ManifestCommand(ctx, ns, &out, &errOut); err != nil || code != 0 {
		t.Fatalf("code %d err %v", code, err)
	}
	if again, err := os.ReadFile(other); err != nil || string(again) != string(raw) {
		t.Fatalf("manifest at the given path %q %v", again, err)
	}
}
