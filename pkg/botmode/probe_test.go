package botmode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Install root under a temp dir with a fresh section cache and the managed scope off
func probeHome(t *testing.T) string {
	t.Helper()
	ResetProtocolCache()
	t.Cleanup(ResetProtocolCache)
	t.Setenv(config.EnvManagedDir, filepath.Join(t.TempDir(), "no_managed_scope"))
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

// Writes a file and its parent dirs
func writeProbeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Named profile dir, Bot Mode managed when asked, with an optional SOUL.md
func makeBotProfile(t *testing.T, root, name string, managed bool, soul string) string {
	t.Helper()
	d := filepath.Join(root, "profiles", name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if managed {
		writeProbeFile(t, filepath.Join(d, "profile.yaml"), "ui_meta:\n  icebath-bots:\n    shape: cloud\n    color: '#8b5cf6'\n")
	}
	if soul != "" {
		writeProbeFile(t, filepath.Join(d, "SOUL.md"), soul)
	}
	return d
}

// Folder ids of a roster
func rosterNames(roster []config.BotProfile) []string {
	out := []string{}
	for _, p := range roster {
		out = append(out, p.Name)
	}
	return out
}

func TestRosterExcludesInfraDirsAndTombstones(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	for _, stray := range []string{"sessions", "logs"} {
		if err := os.MkdirAll(filepath.Join(h, "profiles", stray, "cron"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ghost := makeBotProfile(t, h, "ghost", true, "")
	if err := home.MarkProfileDeleted(ghost); err != nil {
		t.Fatal(err)
	}
	if got := rosterNames(Roster(h)); !reflect.DeepEqual(got, []string{"default", "researcher"}) {
		t.Fatal(got)
	}
	section := ProtocolSection(h, false)
	if !strings.Contains(section, "`@researcher`") {
		t.Fatal(section)
	}
	for _, s := range []string{"sessions", "logs", "ghost", ".deleted"} {
		if strings.Contains(section, "`@"+s+"`") {
			t.Fatal(s)
		}
	}
}

func TestRosterExcludesDirsFailingTheProfileIDRegex(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	for _, stray := range []string{"_backup_removed_20260920", ".staging-area"} {
		writeProbeFile(t, filepath.Join(h, "profiles", stray, "config.yaml"), "model:\n  name: test\n")
	}
	if got := rosterNames(Roster(h)); !reflect.DeepEqual(got, []string{"default", "researcher"}) {
		t.Fatal(got)
	}
	section := ProtocolSection(h, false)
	if !strings.Contains(section, "`@researcher`") || strings.Contains(section, "`@_backup_removed_20260920`") || strings.Contains(section, "`@.staging-area`") {
		t.Fatal(section)
	}
}

func TestSilentWhenNoProfileIsBotManaged(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", false, "")
	if got := ProtocolSection(h, false); got != "" {
		t.Fatal(got)
	}
}

func TestEmitsForDefaultWhenAnyProfileIsManaged(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	section := ProtocolSection(h, false)
	if !strings.HasPrefix(section, "## Messaging other agents") || !strings.Contains(section, "@icebath") ||
		strings.Contains(section, "@default") || !strings.Contains(section, "@researcher") || !strings.Contains(section, "message_agent") {
		t.Fatal(section)
	}
}

func TestEmitsForNamedProfileWithOwnHandle(t *testing.T) {
	h := probeHome(t)
	dir := makeBotProfile(t, h, "coder", true, "")
	section := ProtocolSection(dir, false)
	if !strings.Contains(section, "@coder") {
		t.Fatal(section)
	}
	block := strings.SplitN(section, "Your teammates", 2)[1]
	if !strings.Contains(block, "`@icebath`") || strings.Contains(block, "`@coder`") {
		t.Fatal(block)
	}
}

func TestRosterLinesCarryRoles(t *testing.T) {
	h := probeHome(t)
	writeProbeFile(t, filepath.Join(h, "profiles", "researcher", "profile.yaml"),
		"description: Deep research and literature review\nui_meta:\n  icebath-bots:\n    title: Research Buddy\n")
	section := ProtocolSection(h, false)
	for _, want := range []string{"`@researcher`", "Research Buddy", "Deep research and literature review"} {
		if !strings.Contains(section, want) {
			t.Fatal(want, section)
		}
	}
}

func TestSoulLegacyProtocolNoLongerSuppressesLiveSection(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "coder", true, "")
	writeProbeFile(t, filepath.Join(h, "SOUL.md"), "# Me\n\n## Messaging other agents\nold plugin text\n")
	if !strings.Contains(ProtocolSection(h, false), "`@coder`") {
		t.Fatal("live section")
	}
	data, err := os.ReadFile(filepath.Join(h, "SOUL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tools.StripLegacyBotProtocol(string(data)); got != "# Me\n" {
		t.Fatalf("%q", got)
	}
}

func TestDeterministicAcrossCalls(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	first := ProtocolSection(h, false)
	makeBotProfile(t, h, "newbot", true, "")
	if second := ProtocolSection(h, false); first != second {
		t.Fatal(second)
	}
}

func TestNeverRaisesOnGarbage(t *testing.T) {
	h := probeHome(t)
	writeProbeFile(t, filepath.Join(h, "profiles", "bad", "profile.yaml"), "ui_meta: [unclosed")
	ProtocolSection(h, false)
	prev := rosterFn
	rosterFn = func(string) ([]config.BotProfile, error) { return nil, errors.New("boom") }
	t.Cleanup(func() { rosterFn = prev })
	ResetProtocolCache()
	if got := ProtocolSection(h, false); got != "" {
		t.Fatal(got)
	}
	if IsBotModeManaged(h) || len(LocalAliasMap(h)) != 0 {
		t.Fatal("a failing roster must read as unmanaged")
	}
}

func TestFingerprintChangesOnEachCapabilityAxis(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	base := CapabilityFingerprint(h)
	writeProbeFile(t, filepath.Join(h, "skills", "web", "scraping", "SKILL.md"), "---\nname: scraping\n---\n")
	afterSkill := CapabilityFingerprint(h)
	if afterSkill == base {
		t.Fatal("skill axis")
	}
	writeProbeFile(t, filepath.Join(h, "config.yaml"), "platform_toolsets:\n  desktop: [icebath-desktop, computer_use]\n")
	afterTools := CapabilityFingerprint(h)
	if afterTools == afterSkill {
		t.Fatal("toolsets axis")
	}
	writeProbeFile(t, filepath.Join(h, "config.yaml"), "platform_toolsets:\n  desktop: [icebath-desktop, computer_use]\nagent:\n  disabled_toolsets: [computer_use]\n")
	afterDisabled := CapabilityFingerprint(h)
	if afterDisabled == afterTools {
		t.Fatal("disabled toolsets axis")
	}
	writeProbeFile(t, filepath.Join(h, "config.yaml"), "platform_toolsets:\n  desktop: [icebath-desktop, computer_use]\nagent:\n  disabled_toolsets: [computer_use]\nmcp_servers:\n  github:\n    preset: github\n")
	afterMCP := CapabilityFingerprint(h)
	if afterMCP == afterDisabled {
		t.Fatal("mcp axis")
	}
	writeProbeFile(t, filepath.Join(h, "SOUL.md"), "# New identity\n")
	afterSoul := CapabilityFingerprint(h)
	if afterSoul == afterMCP {
		t.Fatal("soul axis")
	}
	makeBotProfile(t, h, "coder", true, "")
	if CapabilityFingerprint(h) == afterSoul {
		t.Fatal("roster axis")
	}
}

func TestFingerprintChangesWhenModelVisionOverrideFlips(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	cfg := filepath.Join(h, "config.yaml")
	writeProbeFile(t, cfg, "model:\n  supports_vision: true\n")
	enabled := CapabilityFingerprint(h)
	stamped := "system stuff\n\n" + EpochLine(h)
	if enabled != CapabilityFingerprint(h) || StoredPromptCapabilityStale(stamped, h) {
		t.Fatal("stable epoch")
	}
	writeProbeFile(t, cfg, "model:\n  supports_vision: false\n")
	disabled := CapabilityFingerprint(h)
	if disabled == enabled || !StoredPromptCapabilityStale(stamped, h) {
		t.Fatal("vision flip")
	}
	if restamped := "system stuff\n\n" + EpochLine(h); StoredPromptCapabilityStale(restamped, h) {
		t.Fatal("restamped")
	}
	writeProbeFile(t, cfg, "model:\n  supports_vision: true\n")
	if CapabilityFingerprint(h) == disabled {
		t.Fatal("flip back")
	}
}

func TestVisionOverrideSpellingsShareOneFingerprint(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	cfg := filepath.Join(h, "config.yaml")
	writeProbeFile(t, cfg, "model:\n  supports_vision: true\n")
	enabled := CapabilityFingerprint(h)
	writeProbeFile(t, cfg, "model:\n  supports_vision: yes\n")
	if CapabilityFingerprint(h) != enabled {
		t.Fatal("yes and true differ")
	}
	writeProbeFile(t, cfg, "model:\n  supports_vision: false\n")
	if CapabilityFingerprint(h) == enabled {
		t.Fatal("false matches true")
	}
}

func TestFingerprintChangesWhenModelContextLengthOverrideChanges(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	cfg := filepath.Join(h, "config.yaml")
	writeProbeFile(t, cfg, "model:\n  context_length: 32768\n")
	narrow := CapabilityFingerprint(h)
	writeProbeFile(t, cfg, "model:\n  context_length: 131072\n")
	if CapabilityFingerprint(h) == narrow {
		t.Fatal("wider window")
	}
	writeProbeFile(t, cfg, "model:\n  context_length: 32768\n")
	if CapabilityFingerprint(h) != narrow {
		t.Fatal("same window")
	}
}

func TestModelPromptCapabilitySurfaceCoercion(t *testing.T) {
	for _, c := range []struct {
		raw  any
		want string
	}{
		{true, "<nil>"}, {0, "<nil>"}, {-5, "<nil>"}, {4096, "4096"}, {" 8192 ", "8192"}, {"12k", "<nil>"}, {"٣٢", "32"}, {nil, "<nil>"},
	} {
		surface, err := modelPromptCapabilitySurface(map[string]any{"context_length": c.raw, "supports_vision": "on"})
		if err != nil {
			t.Fatal(c.raw, err)
		}
		if got := fmtAny(surface["context_length"]); got != c.want || surface["supports_vision"] != true {
			t.Fatalf("%v gave %v", c.raw, surface)
		}
	}
	if _, err := modelPromptCapabilitySurface(map[string]any{"context_length": "²"}); err == nil {
		t.Fatal("a superscript digit is isdigit but not an int literal")
	}
}

// Text of a surface value for table comparison
func fmtAny(v any) string {
	if v == nil {
		return "<nil>"
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func TestStoredPromptStaleness(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	stamped := "system stuff\n\n" + EpochLine(h)
	if StoredPromptCapabilityStale(stamped, h) {
		t.Fatal("fresh stamp")
	}
	writeProbeFile(t, filepath.Join(h, "skills", "new-skill", "SKILL.md"), "---\nname: new-skill\n---\n")
	if !StoredPromptCapabilityStale(stamped, h) {
		t.Fatal("new skill")
	}
	restamped := "system stuff\n\n" + EpochLine(h)
	if StoredPromptCapabilityStale(restamped, h) || StoredPromptCapabilityStale("ordinary prompt", h) || StoredPromptCapabilityStale("", h) {
		t.Fatal("restamped or unstamped")
	}
}

func TestLegacyBotChatUpgrade(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	legacy := "old prompt with no protocol and no stamp"
	if !StoredBotChatPromptNeedsUpgrade(legacy, h) {
		t.Fatal("legacy")
	}
	upgraded := legacy + "\n\n" + ProtocolSection(h, false) + "\n\n" + EpochLine(h)
	if StoredBotChatPromptNeedsUpgrade(upgraded, h) {
		t.Fatal("upgraded")
	}
	if !StoredBotChatPromptNeedsUpgrade("prompt containing\n## Messaging other agents\nfrom SOUL", h) {
		t.Fatal("soul era")
	}
	ResetProtocolCache()
	h2 := filepath.Join(filepath.Dir(h), ".icebath2")
	if err := os.MkdirAll(h2, 0o755); err != nil {
		t.Fatal(err)
	}
	if StoredBotChatPromptNeedsUpgrade(legacy, h2) {
		t.Fatal("unmanaged install")
	}
}

func TestPeerParagraphListsRegisteredPeers(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	writeProbeFile(t, filepath.Join(h, "config.yaml"), "bot_peers:\n  spark:\n    url: http://spark.lan:8377\n  homelab:\n    url: http://homelab.lan:8377\n")
	section := ProtocolSection(h, false)
	if !strings.Contains(section, "`homelab`") || !strings.Contains(section, "`spark`") || !strings.Contains(section, "`icebath peer list`") {
		t.Fatal(section)
	}
}

func TestFingerprintChangesWhenAPeerIsRegistered(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	before := CapabilityFingerprint(h)
	writeProbeFile(t, filepath.Join(h, "config.yaml"), "bot_peers:\n  spark:\n    url: http://spark.lan:8377\n")
	if CapabilityFingerprint(h) == before {
		t.Fatal("peer axis")
	}
}

func TestRosterResolvesDefaultToRootHomeOverStrayDirectory(t *testing.T) {
	h := probeHome(t)
	makeBotProfile(t, h, "researcher", true, "")
	writeProbeFile(t, filepath.Join(h, "profiles", "default", "state.db"), "")
	roster := Roster(h)
	dirs := map[string]string{}
	count := 0
	for _, p := range roster {
		dirs[p.Name] = p.Dir
		if p.Name == "default" {
			count++
		}
	}
	if dirs["default"] != h || dirs["researcher"] != filepath.Join(h, "profiles", "researcher") || count != 1 {
		t.Fatal(roster)
	}
}

// Skill dir with a SKILL.md under the profile skills tree
func installSkill(t *testing.T, h, rel, name string) {
	t.Helper()
	writeProbeFile(t, filepath.Join(h, "skills", filepath.FromSlash(rel), "SKILL.md"), "---\nname: "+name+"\n---\n")
}

func TestInstallingARealSkillStillMovesTheEpoch(t *testing.T) {
	h := probeHome(t)
	before := CapabilityFingerprint(h)
	installSkill(t, h, "web/scraping", "scraping")
	if CapabilityFingerprint(h) == before {
		t.Fatal("install")
	}
}

func TestArchivingASkillDoesNotMoveTheEpoch(t *testing.T) {
	h := probeHome(t)
	installSkill(t, h, "web/scraping", "scraping")
	installSkill(t, h, "web/keep", "keep")
	before := CapabilityFingerprint(h)
	dst := filepath.Join(h, "skills", ".archive", "scraping")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(h, "skills", "web", "scraping"), dst); err != nil {
		t.Fatal(err)
	}
	afterArchive := CapabilityFingerprint(h)
	if afterArchive == before || CapabilityFingerprint(h) != afterArchive {
		t.Fatal("archive")
	}
	installSkill(t, h, ".archive/another-old-skill", "another")
	if CapabilityFingerprint(h) != afterArchive {
		t.Fatal("archived skill moved the epoch")
	}
}

// Relay roster file holding the given agent rows
func writeRelayRoster(t *testing.T, h string, rows []map[string]any) {
	t.Helper()
	data, err := json.Marshal(map[string]any{"updated_at": 1, "agents": rows})
	if err != nil {
		t.Fatal(err)
	}
	writeProbeFile(t, filepath.Join(h, RelayDirName, RosterFile), string(data))
}

// Install with one managed researcher teammate
func managedProbeHome(t *testing.T) string {
	h := probeHome(t)
	writeProbeFile(t, filepath.Join(h, "profiles", "researcher", "profile.yaml"), "description: teammate for tests\nui_meta:\n  icebath-bots:\n    shape: cloud\n")
	return h
}

func TestProtocolSectionListsRemoteTeammates(t *testing.T) {
	h := managedProbeHome(t)
	writeRelayRoster(t, h, []map[string]any{{"profile": "default", "handle": "icebath", "connection_id": "cloud-1", "connection_label": "Icebath Cloud", "title": "Moxie"}})
	section := ProtocolSection(h, true)
	if !strings.Contains(section, "`@moxie`") || !strings.Contains(section, "Icebath Cloud") {
		t.Fatal(section)
	}
}

func TestCapabilityFingerprintChangesWithRelayRoster(t *testing.T) {
	h := managedProbeHome(t)
	before := CapabilityFingerprint(h)
	writeRelayRoster(t, h, []map[string]any{{"profile": "default", "handle": "icebath", "connection_id": "cloud-1"}})
	if CapabilityFingerprint(h) == before {
		t.Fatal("relay roster axis")
	}
}

func TestRemoteDefaultIsAddressableByItsTitleSlug(t *testing.T) {
	h := managedProbeHome(t)
	writeRelayRoster(t, h, []map[string]any{
		{"profile": "cos-bot", "handle": "cos-bot", "connection_id": "cloud-1", "title": "Other"},
		{"profile": "default", "handle": "icebath", "connection_id": "cloud-1", "title": "Ops Bot"},
		{"profile": "default", "handle": "icebath", "connection_id": "vps-1", "title": "CoS Bot"},
	})
	roster := ReadRemoteRoster(h)
	taken, err := LocalTakenForms(h)
	if err != nil {
		t.Fatal(err)
	}
	forms := map[string]string{}
	for i, form := range RemoteTargetForms(roster, taken) {
		forms[roster[i].ConnectionID+"/"+roster[i].Profile] = form
	}
	if want := map[string]string{"cloud-1/cos-bot": "cos-bot", "vps-1/default": "icebath@vps-1", "cloud-1/default": "ops-bot"}; !reflect.DeepEqual(forms, want) {
		t.Fatal(forms)
	}
	section := ProtocolSection(h, false)
	if !strings.Contains(section, "`@ops-bot`") || !strings.Contains(section, "`@icebath@vps-1`") || strings.Contains(section, "- `@icebath` —") {
		t.Fatal(section)
	}
}

func TestRemoteRosterRejectsBadRowsAndCorruptFiles(t *testing.T) {
	h := probeHome(t)
	if got := ReadRemoteRoster(h); len(got) != 0 {
		t.Fatal(got)
	}
	writeProbeFile(t, filepath.Join(h, RelayDirName, RosterFile), "{not json")
	if got := ReadRemoteRoster(h); len(got) != 0 {
		t.Fatal(got)
	}
	writeRelayRoster(t, h, []map[string]any{
		{"profile": "ok", "connection_id": "c1", "description": "  a\n\tb  ", "online": true},
		{"profile": "bad name", "connection_id": "c1"},
		{"profile": "p", "connection_id": "c/1"},
		{"profile": "", "connection_id": "c1"},
		{"profile": "default", "connection_id": "c2", "online": "yes"},
	})
	rows := ReadRemoteRoster(h)
	if len(rows) != 2 || rows[0].Handle != "ok" || rows[0].Description != "a b" || rows[0].Online == nil || !*rows[0].Online ||
		rows[1].Handle != "icebath" || rows[1].Online != nil {
		t.Fatalf("%+v", rows)
	}
}

func TestAliasFormsAndHandles(t *testing.T) {
	if got := AliasForms("Dr. Foo"); !reflect.DeepEqual(got, map[string]bool{"dr-foo": true, "drfoo": true}) {
		t.Fatal(got)
	}
	if got := AliasForms("Icebath"); len(got) != 0 {
		t.Fatal(got)
	}
	if Handle("default", []string{"default"}) != "icebath" || Handle("default", []string{"default", "icebath"}) != "default" {
		t.Fatal("handle")
	}
	if ProfileForHandle("icebath", []string{"default"}) != "default" || ProfileForHandle("icebath", []string{"icebath"}) != "icebath" {
		t.Fatal("profile for handle")
	}
	h := probeHome(t)
	writeProbeFile(t, filepath.Join(h, "profiles", "scribe", "profile.yaml"), "display_name: Maia\nui_meta:\n  icebath-bots:\n    title: Scribe Bot\n")
	if got := DisplayName("scribe", filepath.Join(h, "profiles", "scribe"), nil); got != "Scribe Bot" {
		t.Fatal(got)
	}
	if got := LocalAliasMap(h); !got["maia"]["scribe"] || !got["scribe-bot"]["scribe"] || !got["scribebot"]["scribe"] {
		t.Fatal(got)
	}
	if got := ConfigRoster(filepath.Join(h, "profiles", "scribe")); len(got) != 2 || got[0].Dir != h {
		t.Fatal(got)
	}
}
