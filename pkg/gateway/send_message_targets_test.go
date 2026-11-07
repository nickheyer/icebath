package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Registers a throwaway platform entry for one test and answers the channel directory with resolved
func registerSendTargetEntry(t *testing.T, name string, resolved string, resolveErr error) *PlatformEntry {
	t.Helper()
	ctx := context.Background()
	entry := NewPlatformEntry(name, name, nil, func() bool { return true })
	Registry.register(entry, CurrentScopeKey(ctx))
	t.Cleanup(func() { _, _ = Registry.Unregister(ctx, name, CurrentScopeKey(ctx)) })
	stubChannelDirectory(t, resolved, resolveErr)
	return entry
}

// Answers every channel directory lookup with the given name or error for one test
func stubChannelDirectory(t *testing.T, resolved string, resolveErr error) {
	saved := resolveChannelNameFn
	t.Cleanup(func() { resolveChannelNameFn = saved })
	resolveChannelNameFn = func(context.Context, string, string) (string, error) { return resolved, resolveErr }
}

func TestRegexParsersAndParseNonempty(t *testing.T) {
	groups := RegexGroupsParser(`(-?\d+)(?::(\d+))?`, true)
	if p := groups("-100:7").(*TargetRefParse); *p.ChatID != "-100" || *p.ThreadID != "7" {
		t.Fatal(p)
	}
	if p := groups("-100").(*TargetRefParse); *p.ChatID != "-100" || p.ThreadID != nil {
		t.Fatal(p)
	}
	if groups("-100:x") != nil || groups("x-100") != nil {
		t.Fatal("partial matches must not parse")
	}
	if p := RegexGroupsParser(`(\w+):(\w+)`, false)("a:b").(*TargetRefParse); *p.ChatID != "a" || p.ThreadID != nil {
		t.Fatal(p)
	}
	stripped := RegexStrippedParser(`\s*room-\d+\s*`)
	if p := stripped("  room-4 ").(*TargetRefParse); *p.ChatID != "room-4" || p.ThreadID != nil {
		t.Fatal(p)
	}
	if stripped("room-x") != nil || ParseNonempty("   ") != nil {
		t.Fatal("non matching refs must answer nil")
	}
	if p := ParseNonempty(" topic ").(*TargetRefParse); *p.ChatID != "topic" {
		t.Fatal(p)
	}
}

func TestParseTargetRefGenericRules(t *testing.T) {
	ctx := context.Background()
	chatID, threadID, explicit := parseTargetRef(ctx, "photon", "+15551234567")
	if *chatID != "+15551234567" || threadID != nil || !explicit {
		t.Fatal(chatID, threadID, explicit)
	}
	if _, _, explicit := parseTargetRef(ctx, "matrix", "+15551234567"); explicit {
		t.Fatal("E164 targets need a phone platform")
	}
	for _, c := range []struct{ platform, ref string }{{"telegram", "-1001"}, {"matrix", "!room:x"}, {"matrix", "@u:x"}, {"xmpp", "a@b"}} {
		if got, _, explicit := parseTargetRef(ctx, c.platform, c.ref); !explicit || *got != c.ref {
			t.Fatal(c)
		}
	}
	if _, _, explicit := parseTargetRef(ctx, "telegram", "ops-room"); explicit {
		t.Fatal("names are not explicit")
	}
}

func TestTargetSyntaxParserRunsFirstAndNotExplicitSkipsGenericRules(t *testing.T) {
	entry := registerSendTargetEntry(t, "syntaxchat", "", nil)
	entry.TargetRefSyntaxFn = func(ref string) any {
		switch ref {
		case "123":
			return NotExplicit
		case "room":
			return &TargetRefParse{ChatID: strp("r-1"), ThreadID: strp("t-2")}
		}
		return nil
	}
	ctx := context.Background()
	if _, _, explicit := parseTargetRef(ctx, "syntaxchat", "123"); explicit {
		t.Fatal("NOT_EXPLICIT must skip the numeric rule")
	}
	if c, th, explicit := parseTargetRef(ctx, "syntaxchat", "room"); !explicit || *c != "r-1" || *th != "t-2" {
		t.Fatal(c, th, explicit)
	}
	if c, _, explicit := parseTargetRef(ctx, "syntaxchat", "456"); !explicit || *c != "456" {
		t.Fatal("a nil parse falls through to the generic rules")
	}
}

func TestUnresolvedBuiltinTargetKeepsDirectoryErrorUnlessPassedThrough(t *testing.T) {
	ctx := context.Background()
	stubChannelDirectory(t, "", nil)
	chatID, _, errText := ResolveSendTarget(ctx, "telegram", "missing-room", false)
	if chatID != "" || !strings.Contains(errText, "missing-room") || !strings.HasSuffix(errText, "Use send_message(action='list') to see available targets.") {
		t.Fatal(chatID, errText)
	}
	chatID, threadID, errText := ResolveSendTarget(ctx, "telegram", "ops-room", true)
	if errText != "" || chatID != "ops-room" || threadID != nil {
		t.Fatal(chatID, threadID, errText)
	}
	stubChannelDirectory(t, "", errors.New("directory unreadable"))
	if _, _, errText := ResolveSendTarget(ctx, "telegram", "ops-room", false); errText != "Could not resolve 'ops-room' on telegram. Try using a numeric channel ID instead." {
		t.Fatal(errText)
	}
}

func TestResolvedDirectoryNameIsReparsedForThreads(t *testing.T) {
	stubChannelDirectory(t, "-100:42", nil)
	entry := registerSendTargetEntry(t, "threadchat", "-100:42", nil)
	entry.TargetRefSyntaxFn = RegexGroupsParser(`(-\d+):(\d+)`, true)
	chatID, threadID, errText := ResolveSendTarget(context.Background(), "threadchat", "General", false)
	if errText != "" || chatID != "-100" || *threadID != "42" {
		t.Fatal(chatID, threadID, errText)
	}
	registerSendTargetEntry(t, "opaque-resolved-test", "opaque:directory-id", nil)
	if chatID, threadID, errText := ResolveSendTarget(context.Background(), "opaque-resolved-test", "Friendly name", false); errText != "" || chatID != "opaque:directory-id" || threadID != nil {
		t.Fatal(chatID, threadID, errText)
	}
}

func TestParserlessPluginTargetPassesThroughWhenRequested(t *testing.T) {
	registerSendTargetEntry(t, "opaque-cron-fallback-test", "", nil)
	chatID, threadID, errText := ResolveSendTarget(context.Background(), "opaque-cron-fallback-test", "dm:panyaozhen", true)
	if errText != "" || chatID != "dm:panyaozhen" || threadID != nil {
		t.Fatal(chatID, threadID, errText)
	}
	if _, _, errText := ResolveSendTarget(context.Background(), "opaque-cron-fallback-test", "dm:panyaozhen", false); errText != "Could not resolve 'dm:panyaozhen' on opaque-cron-fallback-test. The plugin parser did not recognize it and no channel-directory entry matched." {
		t.Fatal(errText)
	}
}

func TestPluginParserStaysAuthoritativeDespiteFallback(t *testing.T) {
	entry := registerSendTargetEntry(t, "opaque-parser-strict-test", "", nil)
	entry.ParseTargetRefFn = func(string) (string, *string, bool) { return "", nil, false }
	chatID, _, errText := ResolveSendTarget(context.Background(), "opaque-parser-strict-test", "dm:panyaozhen", true)
	if chatID != "" || errText == "" {
		t.Fatal(chatID, errText)
	}
}

func TestPluginParserAndValidatorOutcomes(t *testing.T) {
	ctx := context.Background()
	entry := registerSendTargetEntry(t, "validchat", "", nil)
	entry.ParseTargetRefFn = func(ref string) (string, *string, bool) {
		switch ref {
		case "boom":
			panic("parser exploded")
		case "empty":
			return "", nil, true
		}
		return "id-" + ref, strp("th"), true
	}
	if _, _, e := ResolveSendTarget(ctx, "validchat", "boom", false); e != "Target parser failed for platform 'validchat'" {
		t.Fatal(e)
	}
	if _, _, e := ResolveSendTarget(ctx, "validchat", "empty", false); e != "Target parser for platform 'validchat' returned an invalid result" {
		t.Fatal(e)
	}
	if c, th, e := ResolveSendTarget(ctx, "validchat", "a", false); e != "" || c != "id-a" || *th != "th" {
		t.Fatal(c, th, e)
	}
	entry.ValidateTargetRefFn = func(chatID string) any {
		switch chatID {
		case "id-bad":
			return "not a room"
		case "id-no":
			return false
		case "id-boom":
			panic("validator exploded")
		}
		return true
	}
	cases := map[string]string{
		"bad":  "Invalid target 'bad' on validchat: not a room",
		"no":   "Invalid target 'no' on validchat",
		"boom": "Target parser failed for platform 'validchat'",
		"ok":   "",
	}
	for ref, want := range cases {
		if _, _, e := ResolveSendTarget(ctx, "validchat", ref, false); e != want {
			t.Fatal(ref, e)
		}
	}
	entry.ParseTargetRefFn = nil
	if _, _, e := ResolveSendTarget(ctx, "validchat", "-5", false); e != "" {
		t.Fatal(e)
	}
	entry.ValidateTargetRefFn = func(string) any { panic("validator exploded") }
	if _, _, e := ResolveSendTarget(ctx, "validchat", "-5", false); e != "Target validator failed for platform 'validchat'" {
		t.Fatal(e)
	}
}

func TestUnknownPlatformThatLeftCoreNamesItsInstallCommand(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	stubChannelDirectory(t, "", nil)
	_, _, errText := ResolveSendTarget(context.Background(), "homeassistant", "living room", false)
	if !strings.Contains(errText, "unregistered plugin platform: homeassistant") || !strings.Contains(errText, "`icebath plugins install homeassistant`") {
		t.Fatal(errText)
	}
	if got := UnknownPlatformError(context.Background(), "nosuchplatform"); got != "Unknown or unregistered plugin platform: nosuchplatform" {
		t.Fatal(got)
	}
}
