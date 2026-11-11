package gateway

import (
	"context"
	"maps"
	"testing"
)

// Slack descriptor of the unfurl cases with the ops and draft flag given
func relayUnfurlDesc(platform string, draft bool, ops ...string) CapabilityDescriptor {
	d := NewCapabilityDescriptor(RelayContractVersion, platform, "Slack", 39000, draft, true, true, "slack", "char")
	d.Emoji = "\U0001f4bc"
	if ops != nil {
		d.SupportedOps = ops
	}
	return d
}

// Adapter of the extras over a capture transport fronting slack
func relayUnfurlAdapter(t *testing.T, extra map[string]any, desc CapabilityDescriptor) (*RelayAdapter, *relayCapture) {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	c := &relayCapture{byPlatform: map[string]CapabilityDescriptor{}, identities: [][2]string{{"slack", ""}}}
	cfg := NewPlatformConfig()
	cfg.Extra = extra
	return NewRelayAdapter(cfg, desc, c), c
}

func relaySlackExtra(knobs map[string]any) map[string]any { return map[string]any{"slack": knobs} }

func relayMarkChat(a *RelayAdapter, platform Platform) {
	a.captureScope(relayInbound(platform, "chan-1", "channel", func(s *SessionSource) { s.ScopeID = relayStrPtr("w-1") }))
}

func relayLastMeta(t *testing.T, c *relayCapture) map[string]any {
	t.Helper()
	last, _ := c.last(t)
	return last["metadata"].(map[string]any)
}

func relayHasUnfurl(md map[string]any) bool {
	_, links := md["unfurl_links"]
	_, media := md["unfurl_media"]
	return links || media
}

var relayBothFalse = map[string]any{"unfurl_links": false, "unfurl_media": false}

func TestUnfurlHintsNonSlackReturnsNone(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": false}), relayUnfurlDesc("slack", false))
	if a.slackUnfurlHints("discord") != nil || a.slackUnfurlHints("") != nil {
		t.Fatal("non slack hints")
	}
}

func TestUnfurlHintsSlackExplicitBoolsReturned(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(maps.Clone(relayBothFalse)), relayUnfurlDesc("slack", false))
	if got := a.slackUnfurlHints("slack"); !maps.Equal(got, map[string]bool{"unfurl_links": false, "unfurl_media": false}) {
		t.Fatalf("hints %v", got)
	}
}

func TestUnfurlHintsOmittedKeysReturnNone(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{}), relayUnfurlDesc("slack", false))
	if got := a.slackUnfurlHints("slack"); got != nil {
		t.Fatalf("hints %v", got)
	}
}

func TestUnfurlHintsStringBoolsFromConfigSetAreCoerced(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": "true", "unfurl_media": "false"}), relayUnfurlDesc("slack", false))
	if got := a.slackUnfurlHints("slack"); !maps.Equal(got, map[string]bool{"unfurl_links": true, "unfurl_media": false}) {
		t.Fatalf("hints %v", got)
	}
}

func TestUnfurlHintsJunkValuesDropped(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": "maybe", "unfurl_media": 0}), relayUnfurlDesc("slack", false))
	if got := a.slackUnfurlHints("slack"); got != nil {
		t.Fatalf("hints %v", got)
	}
}

func TestUnfurlHintsFlatLegacyKeyFallback(t *testing.T) {
	a, _ := relayUnfurlAdapter(t, map[string]any{"unfurl_links": false}, relayUnfurlDesc("slack", false))
	if got := a.slackUnfurlHints("slack"); !maps.Equal(got, map[string]bool{"unfurl_links": false}) {
		t.Fatalf("hints %v", got)
	}
}

func TestSendStampsUnfurlSendStampsExplicitBools(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(maps.Clone(relayBothFalse)), relayUnfurlDesc("slack", false))
	relayMarkChat(a, PlatformSlack)
	a.Send(context.Background(), "chan-1", "see https://example.com", "", nil)
	if md := relayLastMeta(t, c); md["unfurl_links"] != false || md["unfurl_media"] != false {
		t.Fatalf("metadata %v", md)
	}
}

func TestSendStampsUnfurlSendOmitsWhenUnconfigured(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{}), relayUnfurlDesc("slack", false))
	relayMarkChat(a, PlatformSlack)
	a.Send(context.Background(), "chan-1", "plain text", "", nil)
	if md := relayLastMeta(t, c); relayHasUnfurl(md) {
		t.Fatalf("metadata %v", md)
	}
}

func TestSendStampsUnfurlNonSlackChatNeverStamped(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": false}), relayUnfurlDesc("slack", false))
	relayMarkChat(a, PlatformDiscord)
	a.Send(context.Background(), "chan-1", "see https://example.com", "", nil)
	if md := relayLastMeta(t, c); relayHasUnfurl(md) {
		t.Fatalf("metadata %v", md)
	}
}

func TestSendStampsUnfurlSendFallsBackToDescriptorPlatform(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": false}), relayUnfurlDesc("slack", false))
	if len(a.platformByChat) != 0 {
		t.Fatal("chat platform cache not empty")
	}
	a.Send(context.Background(), "chan-1", "see https://example.com", "", nil)
	if md := relayLastMeta(t, c); md["unfurl_links"] != false {
		t.Fatalf("metadata %v", md)
	}
}

func relayUnfurlMedia(t *testing.T, extra map[string]any) (*RelayAdapter, *relayCapture) {
	return relayUnfurlAdapter(t, extra, relayUnfurlDesc("slack", false, "send", "send_media"))
}

func TestMediaLaneStampsUnfurlMediaLaneStampsExplicitBools(t *testing.T) {
	a, c := relayUnfurlMedia(t, relaySlackExtra(maps.Clone(relayBothFalse)))
	relayMarkChat(a, PlatformSlack)
	r, err := a.SendImage(context.Background(), "chan-1", "https://img.example/x.png", "cap", "", nil)
	last, _ := c.last(t)
	if md := relayLastMeta(t, c); err != nil || !r.Success || last["op"] != "send_media" || md["unfurl_links"] != false || md["unfurl_media"] != false {
		t.Fatalf("result %+v err %v frame %v", r, err, last)
	}
}

func TestMediaLaneStampsUnfurlMediaLaneFallsBackToDescriptorPlatform(t *testing.T) {
	a, c := relayUnfurlMedia(t, relaySlackExtra(map[string]any{"unfurl_links": false}))
	r, err := a.SendImage(context.Background(), "chan-1", "https://img.example/x.png", "", "", nil)
	last, _ := c.last(t)
	if md := relayLastMeta(t, c); err != nil || !r.Success || len(a.platformByChat) != 0 || last["op"] != "send_media" || md["unfurl_links"] != false {
		t.Fatalf("result %+v err %v frame %v", r, err, last)
	}
}

func TestMediaLaneStampsUnfurlMediaLaneOmitsWhenUnconfigured(t *testing.T) {
	a, c := relayUnfurlMedia(t, relaySlackExtra(map[string]any{}))
	relayMarkChat(a, PlatformSlack)
	a.SendImage(context.Background(), "chan-1", "https://img.example/x.png", "", "", nil)
	last, _ := c.last(t)
	if md := relayLastMeta(t, c); last["op"] != "send_media" || relayHasUnfurl(md) {
		t.Fatalf("frame %v", last)
	}
}

func TestSendForPlatformStampsUnfurlCronLaneStampsExplicitBools(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(maps.Clone(relayBothFalse)), relayUnfurlDesc("slack", false))
	r, err := a.SendForPlatform(context.Background(), PlatformSlack, "C123", "brief https://x.dev", nil)
	if md := relayLastMeta(t, c); err != nil || !r.Success || md["unfurl_links"] != false || md["unfurl_media"] != false {
		t.Fatalf("result %+v err %v metadata %v", r, err, md)
	}
}

func TestSendForPlatformStampsUnfurlCronLaneOmitsWhenUnconfigured(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{}), relayUnfurlDesc("slack", false))
	a.SendForPlatform(context.Background(), PlatformSlack, "C123", "brief", nil)
	if md := relayLastMeta(t, c); relayHasUnfurl(md) {
		t.Fatalf("metadata %v", md)
	}
}

func relayUnfurlDraft(t *testing.T, platform string, knobs map[string]any) *RelayAdapter {
	a, _ := relayUnfurlAdapter(t, relaySlackExtra(knobs), relayUnfurlDesc(platform, true, "send", "draft"))
	return a
}

func TestUnfurlDisablesDraftStreamingExplicitUnfurlDisablesSlackDraftStream(t *testing.T) {
	if relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": true}).SupportsDraftStreaming("", nil, "") {
		t.Fatal("explicit unfurl must disable the slack draft stream")
	}
}

func TestUnfurlDisablesDraftStreamingOmittedUnfurlKeepsSlackDraftStream(t *testing.T) {
	if !relayUnfurlDraft(t, "slack", map[string]any{}).SupportsDraftStreaming("", nil, "") {
		t.Fatal("omitted unfurl must keep the slack draft stream")
	}
}

func TestUnfurlDisablesDraftStreamingStringTrueAlsoDisablesStream(t *testing.T) {
	if relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": "true"}).SupportsDraftStreaming("", nil, "") {
		t.Fatal("string true must disable the slack draft stream")
	}
}

func TestFreshFinalForForceOnUnfurlForceOnWithLinkPrefersFreshFinal(t *testing.T) {
	if !relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": true, "unfurl_media": true}).PrefersFreshFinalStreaming("see https://studiotwin.ai", nil, "") {
		t.Fatal("force on with a link must prefer a fresh final")
	}
}

func TestFreshFinalForForceOnUnfurlForceOnStringTruePrefersFreshFinal(t *testing.T) {
	if !relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": "true"}).PrefersFreshFinalStreaming("see https://x.dev", nil, "") {
		t.Fatal("string true must prefer a fresh final")
	}
}

func TestFreshFinalForForceOnUnfurlForceOnMrkdwnLinkPrefersFreshFinal(t *testing.T) {
	if !relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": true}).PrefersFreshFinalStreaming("see <https://x.dev|x.dev>", nil, "") {
		t.Fatal("mrkdwn link must prefer a fresh final")
	}
}

func TestFreshFinalForForceOnUnfurlForceOnWithoutLinkKeepsEditLane(t *testing.T) {
	if relayUnfurlDraft(t, "slack", map[string]any{"unfurl_links": true}).PrefersFreshFinalStreaming("plain text answer", nil, "") {
		t.Fatal("a linkless final must keep the edit lane")
	}
}

func TestFreshFinalForForceOnUnfurlFalseOnlyHintsKeepEditLane(t *testing.T) {
	if relayUnfurlDraft(t, "slack", maps.Clone(relayBothFalse)).PrefersFreshFinalStreaming("see https://studiotwin.ai", nil, "") {
		t.Fatal("false only hints must keep the edit lane")
	}
}

func TestFreshFinalForForceOnUnfurlUnconfiguredKeepsEditLane(t *testing.T) {
	if relayUnfurlDraft(t, "slack", map[string]any{}).PrefersFreshFinalStreaming("see https://studiotwin.ai", nil, "") {
		t.Fatal("unconfigured must keep the edit lane")
	}
}

func TestFreshFinalForForceOnUnfurlNonSlackPlatformKeepsEditLane(t *testing.T) {
	if relayUnfurlDraft(t, "telegram", map[string]any{"unfurl_links": true}).PrefersFreshFinalStreaming("see https://studiotwin.ai", nil, "") {
		t.Fatal("non slack platform must keep the edit lane")
	}
}

func TestDeleteOpForFreshFinalCleanupDeleteEmittedWhenAdvertised(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": true}), relayUnfurlDesc("slack", false, "send", "edit", "delete"))
	ok, err := a.DeleteMessage(context.Background(), "D1", "1700000000.000200")
	last, _ := c.last(t)
	if err != nil || !ok || last["op"] != "delete" || last["message_id"] != "1700000000.000200" {
		t.Fatalf("ok %v err %v frame %v", ok, err, last)
	}
}

func TestDeleteOpForFreshFinalCleanupDeleteRefusedWhenNotAdvertised(t *testing.T) {
	a, c := relayUnfurlAdapter(t, relaySlackExtra(map[string]any{"unfurl_links": true}), relayUnfurlDesc("slack", false, "send", "edit"))
	ok, err := a.DeleteMessage(context.Background(), "D1", "1700000000.000200")
	if err != nil || ok || len(c.sent) != 0 {
		t.Fatalf("ok %v err %v sent %v", ok, err, c.sent)
	}
}
