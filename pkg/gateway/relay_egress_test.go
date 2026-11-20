package gateway

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const declineText = "discord egress declined: target is not an approved destination for this connection"

func declineDict() map[string]any { return map[string]any{"success": false, "error": declineText} }

func TestDeclineIsRecognisedByCodeAndByUniformText(t *testing.T) {
	if EgressDeclineCode != "egress_declined" || !IsEgressDecline(map[string]any{"success": false, "code": "egress_declined"}) ||
		!IsEgressDecline(map[string]any{"success": false, "code": EgressDeclineCode}) || !IsEgressDecline(declineDict()) {
		t.Fatal("decline code or uniform text not recognised")
	}
	for _, v := range []string{"Discord Egress Declined: target is not approved", "EGRESS DECLINED: target is not approved", "discord EGRESS declined: target is not approved"} {
		if !IsEgressDecline(map[string]any{"success": false, "error": v}) {
			t.Fatal(v)
		}
	}
}

func TestAnAmbiguousFailureIsNotADecline(t *testing.T) {
	if IsEgressDecline(map[string]any{"success": false, "error": declineText, "ambiguous": true}) {
		t.Fatal("ambiguous failure classified as decline")
	}
}

func TestAnOrdinaryFailureIsNotADecline(t *testing.T) {
	if IsEgressDecline(map[string]any{"success": false, "error": "file too large"}) || IsEgressDecline(map[string]any{"success": true}) || IsEgressDecline(nil) {
		t.Fatal("ordinary outcome classified as decline")
	}
}

func TestDeclineErrorIsTheConnectorTextVerbatim(t *testing.T) {
	if got := DeclineError(declineDict()); got != declineText {
		t.Fatalf("got=%q", got)
	}
}

func writeHomeFile(t *testing.T, h, rel, text string) {
	t.Helper()
	p := filepath.Join(h, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestThreadQualifiedSessionIdAttestsTheBareChat(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	writeHomeFile(t, h, "sessions/sessions.json", `{"s": {"origin": {"platform": "telegram", "chat_id": "-100999", "thread_id": "77"}}}`)
	ids := egressSessionIDs(context.Background(), "telegram")
	if !ids["-100999:77"] || !ids["-100999"] {
		t.Fatalf("ids=%v", ids)
	}
}

func TestRelayPlaneAttestsTheUnionOfFrontedPlatforms(t *testing.T) {
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", "discord,slack")
	SetGatewayRunnerRef(nil)
	writeHomeFile(t, h, "channel_directory.json", `{"platforms": {"discord": [{"id": "C-DISCORD"}], "slack": [{"id": "C-SLACK"}], "relay": []}}`)
	targets, err := AttestedRelayTargets(context.Background(), "relay")
	if err != nil || !targets["C-DISCORD"] || !targets["C-SLACK"] {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
}

// Relay env fronting one platform with an empty directory, no sessions, no home channel and no live adapter
func relayEnv(t *testing.T, platform string) {
	t.Helper()
	h := t.TempDir()
	t.Setenv("ICEBATH_HOME", h)
	t.Setenv("GATEWAY_RELAY_PLATFORMS", platform)
	SetGatewayRunnerRef(nil)
	writeHomeFile(t, h, "channel_directory.json", `{"platforms": {"`+platform+`": []}}`)
}

func TestTelegramUsernameDefersToTheConnector(t *testing.T) {
	relayEnv(t, "telegram")
	if denial, err := AuthorizeRelayTarget(context.Background(), "telegram", "@some_public_channel", nil, NativeToken{}); err != nil || denial != "" {
		t.Fatalf("denial=%q err=%v", denial, err)
	}
}

func TestNumericTelegramTargetIsStillGuarded(t *testing.T) {
	relayEnv(t, "telegram")
	denial, err := AuthorizeRelayTarget(context.Background(), "telegram", "-1009999999999", nil, NativeToken{})
	if err != nil || !strings.Contains(denial, "-1009999999999") {
		t.Fatalf("denial=%q err=%v", denial, err)
	}
}

func TestCarveOutIsTelegramOnly(t *testing.T) {
	relayEnv(t, "matrix")
	if denial, err := AuthorizeRelayTarget(context.Background(), "matrix", "@someone:server.org", nil, NativeToken{}); err != nil || denial == "" {
		t.Fatalf("denial=%q err=%v", denial, err)
	}
}
