package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/pm"
)

func chStamp(t *testing.T, root string, fields map[string]any) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(fields)
	if err := os.WriteFile(filepath.Join(root, "install-stamp.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func chConfigFor(root, channel string) map[string]any {
	return map[string]any{"update": map[string]any{"installs": map[string]any{
		InstallID(root): map[string]any{"path": root, "channel": channel}}}}
}

func chHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	if err := os.MkdirAll(h, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	return h
}

func chReadConfig(t *testing.T, h string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChannelDynamicNameRoundTripAndInvalidNames(t *testing.T) {
	h := chHome(t)
	base := filepath.Dir(h)
	root := filepath.Join(base, "source")
	os.MkdirAll(root, 0o755)
	name := "new-preview-493"
	if _, err := SetInstallChannel(context.Background(), name, root); err != nil {
		t.Fatal(err)
	}
	saved := chReadConfig(t, h)
	if got, err := ResolveUpdateChannel(saved, root); err != nil || got != name {
		t.Fatalf("got %q %v", got, err)
	}
	if got, _ := ResolveUpdateChannel(saved, filepath.Join(base, "other")); got != ChannelMain {
		t.Fatalf("other install resolved %q", got)
	}
	for _, invalid := range []string{" New-preview", "PREVIEW", "preview/other", "preview--other", "", strings.Repeat("a", 33), "com1", "lpt9"} {
		if _, err := SetInstallChannel(context.Background(), invalid, root); err == nil {
			t.Fatalf("set accepted %q", invalid)
		}
		if _, err := ResolveUpdateChannel(chConfigFor(root, invalid), root); err == nil {
			t.Fatalf("resolve accepted %q", invalid)
		}
	}
}

func TestChannelInstallIDIsPathDerivedAndMatchesStateKey(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	os.MkdirAll(a, 0o755)
	os.MkdirAll(b, 0o755)
	if InstallID(a) != InstallID(a) || len(InstallID(a)) != 16 {
		t.Fatal("install id is not a stable sixteen digit key")
	}
	if InstallID(a) == InstallID(b) {
		t.Fatal("two installs share one id")
	}
	if InstallID(a) != pm.InstallKey(a) {
		t.Fatal("install id differs from the state folder key")
	}
}

func TestChannelBundleKeepsBakedSubscription(t *testing.T) {
	root := t.TempDir()
	chStamp(t, root, map[string]any{"payload": "bundled", "source": "channel-build", "updateMechanism": "electron-updater",
		"tag": nil, "channelBuild": map[string]any{"channel": "r2-preview"}})
	if got, err := DefaultChannel(root); err != nil || got != "r2-preview" {
		t.Fatalf("default %q %v", got, err)
	}
	if got, err := ResolveUpdateChannel(chConfigFor(root, "stable"), root); err != nil || got != "r2-preview" {
		t.Fatalf("resolve %q %v", got, err)
	}
	chStamp(t, root, map[string]any{"payload": "bundled", "source": "channel-build", "channelBuild": "x"})
	if _, err := DefaultChannel(root); err == nil || err.Error() != "Channel bundle has no baked subscription" {
		t.Fatalf("got %v", err)
	}
}

func TestChannelMultiInstallIsolation(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	chStamp(t, a, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	chStamp(t, b, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	cfg := chConfigFor(a, "stable")
	if got, _ := ResolveUpdateChannel(cfg, a); got != ChannelStable {
		t.Fatalf("a resolved %q", got)
	}
	if got, _ := ResolveUpdateChannel(cfg, b); got != ChannelMain {
		t.Fatalf("b resolved %q", got)
	}
}

func TestChannelPolicy(t *testing.T) {
	cases := []struct{ mechanism, tag, payload, record, expected string }{
		{"self", "", "", "", "main"},
		{"self", "v1.2.3-canary.20260818", "", "", "main"},
		{"", "", "", "", "main"},
		{"self", "", "", "canary", "canary"},
		{"self", "", "", "yolo", "yolo"},
		{"electron-updater", "v1.2.3", "", "", "stable"},
		{"electron-updater", "v1.2.3+canary.20260819T171926Z", "", "stable", "canary"},
		{"app-installer", "v1.2.3+canary.20260819T171926Z", "", "", "canary"},
		{"microsoft-store", "v1.2.3+canary.20260819T171926Z", "", "", "canary"},
		{"electron-updater", "v1.2.3-canary.20260818", "", "", "stable"},
		{"electron-updater", "", "", "canary", "stable"},
		{"external", "v1.2.3", "bundled", "main", "stable"},
		{"external", "v1.2.3", "light", "main", "stable"},
		{"external", "v1.2.4+canary.20260911T125822Z", "bundled", "main", "canary"},
		{"external", "v1.2.4+canary.20260911T125822Z", "light", "main", "canary"},
	}
	for _, tc := range cases {
		root := t.TempDir()
		if tc.mechanism != "" {
			stamp := map[string]any{"updateMechanism": tc.mechanism, "tag": nil}
			if tc.tag != "" {
				stamp["tag"] = tc.tag
			}
			if tc.payload != "" {
				stamp["payload"] = tc.payload
			}
			chStamp(t, root, stamp)
		}
		cfg := map[string]any{}
		if tc.record != "" {
			cfg = chConfigFor(root, tc.record)
		}
		if got, err := ResolveUpdateChannel(cfg, root); err != nil || got != tc.expected {
			t.Fatalf("%+v resolved %q %v", tc, got, err)
		}
		if tc.record == "" || tc.mechanism != "self" {
			if got, _ := DefaultChannel(root); got != tc.expected {
				t.Fatalf("%+v default %q", tc, got)
			}
		}
	}
}

func TestChannelOfficialCheckoutTracksStableAndForksMain(t *testing.T) {
	for url, expected := range map[string]string{
		"https://github.com/nickheyer/icebath.git": "stable",
		"git@github.com:NickHeyer/Icebath":         "stable",
		"https://github.com/someone/icebath.git":   "main",
		"/srv/mirror/icebath":                      "main",
	} {
		root := t.TempDir()
		os.MkdirAll(filepath.Join(root, ".git"), 0o755)
		body := "[core]\n\turl = https://github.com/nickheyer/icebath\n[remote \"origin\"]\n\turl = " + url + "\n"
		os.WriteFile(filepath.Join(root, ".git", "config"), []byte("\xef\xbb\xbf"+body), 0o644)
		if got, err := DefaultChannel(root); err != nil || got != expected {
			t.Fatalf("%s resolved %q %v", url, got, err)
		}
	}
	worktree := t.TempDir()
	common := filepath.Join(t.TempDir(), "repo.git")
	os.MkdirAll(filepath.Join(common, "worktrees", "w"), 0o755)
	os.WriteFile(filepath.Join(common, "config"), []byte("[remote \"origin\"]\nurl = ssh://git@github.com/nickheyer/icebath/\n"), 0o644)
	os.WriteFile(filepath.Join(common, "worktrees", "w", "commondir"), []byte("../..\n"), 0o644)
	os.WriteFile(filepath.Join(worktree, ".git"), []byte("gitdir: "+filepath.Join(common, "worktrees", "w")+"\n"), 0o644)
	if got, err := DefaultChannel(worktree); err != nil || got != ChannelStable {
		t.Fatalf("linked worktree resolved %q %v", got, err)
	}
}

func TestChannelSetResolveRoundTrip(t *testing.T) {
	for _, channel := range []string{"stable", "canary", "main"} {
		h := chHome(t)
		root := filepath.Join(filepath.Dir(h), "install")
		chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
		sha16, err := SetInstallChannel(context.Background(), channel, root)
		if err != nil || sha16 != InstallID(root) {
			t.Fatalf("set %q %v", sha16, err)
		}
		written := chReadConfig(t, h)
		record := written["update"].(map[string]any)["installs"].(map[string]any)[sha16].(map[string]any)
		if record["channel"] != channel || record["path"] != root {
			t.Fatalf("record %v", record)
		}
		if got, _ := ResolveUpdateChannel(written, root); got != channel {
			t.Fatalf("resolved %q", got)
		}
	}
}

func TestChannelOwnedInstallsRefuseWrites(t *testing.T) {
	for _, mechanism := range []string{"external", "electron-updater", "app-installer", "microsoft-store"} {
		h := chHome(t)
		root := filepath.Join(filepath.Dir(h), "os-owned-tree")
		chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": mechanism})
		if _, err := SetInstallChannel(context.Background(), "stable", root); err == nil || !strings.Contains(err.Error(), "owned by") {
			t.Fatalf("%s: %v", mechanism, err)
		}
	}
}

func TestChannelCommitBuildRefusalKeepsConfig(t *testing.T) {
	for _, channel := range []string{"main", "stable", "canary"} {
		h := chHome(t)
		cfg := filepath.Join(h, "config.yaml")
		os.WriteFile(cfg, []byte("# preserve\nupdate: {}\n"), 0o644)
		root := filepath.Join(filepath.Dir(h), "commit-build")
		chStamp(t, root, map[string]any{"source": "commit-build", "updateMechanism": "external"})
		_, err := SetInstallChannel(context.Background(), channel, root)
		if err == nil || err.Error() != CommitBuildUpdateMessage {
			t.Fatalf("got %v", err)
		}
		if after, _ := os.ReadFile(cfg); string(after) != "# preserve\nupdate: {}\n" {
			t.Fatalf("config changed %q", after)
		}
	}
}

func TestChannelBadNameAndMalformedSectionsRefused(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	var channelErr *ChannelError
	if _, err := SetInstallChannel(context.Background(), "../beta", root); !errors.As(err, &channelErr) || err.Error() != "Invalid channel name" {
		t.Fatalf("got %v", err)
	}
	for _, bad := range []string{"update: not-a-mapping\n", "update:\n  installs: 7\n"} {
		cfg := filepath.Join(h, "config.yaml")
		os.WriteFile(cfg, []byte("# user header\n"+bad), 0o644)
		if _, err := SetInstallChannel(context.Background(), "stable", root); err == nil || !strings.Contains(err.Error(), "not a mapping") {
			t.Fatalf("%q: %v", bad, err)
		}
		if after, _ := os.ReadFile(cfg); string(after) != "# user header\n"+bad {
			t.Fatalf("config changed %q", after)
		}
	}
}

func TestChannelMetadataArgs(t *testing.T) {
	h := chHome(t)
	root := filepath.Dir(h)
	cfg := filepath.Join(h, "config.yaml")
	before := "# retain user comment\nmodel:\n  provider: fixture\n"
	os.WriteFile(cfg, []byte(before), 0o644)
	for _, tc := range []struct{ mechanism, channel, message string }{
		{"self", "../bogus", "Invalid channel name"},
		{"external", "stable", "owned by"},
		{"app-installer", "canary", "owned by"},
	} {
		chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": tc.mechanism})
		var out bytes.Buffer
		channel := tc.channel
		handled, err := HandleMetadataArgs(context.Background(), &out, false, &channel, root)
		var exit *pm.SystemExit
		if !handled || !errors.As(err, &exit) || exit.Code != 2 || !strings.Contains(out.String(), tc.message) {
			t.Fatalf("%+v: %v %v %q", tc, handled, err, out.String())
		}
		if after, _ := os.ReadFile(cfg); string(after) != before {
			t.Fatalf("config changed %q", after)
		}
	}
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self", "tag": "v0.28.0+canary.20260818T000000Z"})
	var out bytes.Buffer
	stable := "stable"
	if handled, err := HandleMetadataArgs(context.Background(), &out, false, &stable, root); !handled || err != nil {
		t.Fatalf("%v %v", handled, err)
	}
	if text := out.String(); !strings.Contains(text, "Back up your data") || !strings.Contains(text, "older stable release") || strings.Contains(text, "Wait") {
		t.Fatalf("got %q", text)
	}
	out.Reset()
	if handled, _ := HandleMetadataArgs(context.Background(), &out, true, nil, root); !handled || out.String() != InstallID(root)+"\n" {
		t.Fatalf("install id printed %q", out.String())
	}
	if handled, err := HandleMetadataArgs(context.Background(), &out, false, nil, root); handled || err != nil {
		t.Fatal("no metadata flag was handled")
	}
}

func TestChannelDoctorRecordsAreReadOnly(t *testing.T) {
	for _, kind := range []string{"missing", "replaced", "unclaimed", "healthy"} {
		h := chHome(t)
		root := filepath.Join(filepath.Dir(h), "install")
		if kind != "missing" {
			os.MkdirAll(root, 0o755)
		}
		key := InstallID(root)
		if kind == "replaced" {
			key = strings.Repeat("0", 16)
		}
		cfg := map[string]any{"update": map[string]any{"installs": map[string]any{key: map[string]any{"path": root, "channel": "main"}}}}
		if kind == "healthy" {
			state := filepath.Join(h, "installs", key)
			os.MkdirAll(state, 0o755)
			os.WriteFile(filepath.Join(state, "install.json"), []byte(`{"root": "x"}`), 0o644)
		}
		stale, err := StaleChannelRecords(context.Background(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if kind == "healthy" {
			if len(stale) != 0 {
				t.Fatalf("healthy record reported %v", stale)
			}
			continue
		}
		if len(stale) != 1 || stale[0].SHA16 != key || stale[0].Reason != kind {
			t.Fatalf("%s: %v", kind, stale)
		}
	}
}

func TestChannelAdoptRetiredOnlyOverUnchangedRecord(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	if _, err := SetInstallChannel(context.Background(), "old-preview", root); err != nil {
		t.Fatal(err)
	}
	original := map[string]any{"path": root, "channel": "old-preview"}
	request := map[string]any{"source": root, "home": h, "channel_retirement": map[string]any{"original": original, "destination": "stable"}}
	if adopted, err := AdoptRetiredChannel(context.Background(), request); err != nil || !adopted {
		t.Fatalf("%v %v", adopted, err)
	}
	if got, _ := ResolveUpdateChannel(chReadConfig(t, h), root); got != ChannelStable {
		t.Fatalf("resolved %q", got)
	}
	if adopted, err := AdoptRetiredChannel(context.Background(), request); err != nil || adopted {
		t.Fatalf("changed record adopted %v %v", adopted, err)
	}
	if adopted, _ := AdoptRetiredChannel(context.Background(), map[string]any{}); adopted {
		t.Fatal("request without retirement adopted")
	}
}

func TestChannelCanaryTags(t *testing.T) {
	if !IsCanaryTag(" v1.2.3+canary.20260819T171926Z\n") || IsCanaryTag("v1.2.3-canary.20260818") || IsCanaryTag(7) || IsCanaryTag("v1000.0.0+canary.20260819T171926Z") {
		t.Fatal("canary shape misjudged")
	}
	if stamp := CanaryTimestamp("v1.2.3+canary.20260819T171926Z"); stamp == nil || *stamp != "20260819T171926Z" {
		t.Fatal("canary stamp not extracted")
	}
	if CanaryTimestamp("v1.2.3") != nil {
		t.Fatal("stable tag gave a canary stamp")
	}
	if tag, err := CanaryTagForDate("v1.2.3", "20260819T171926Z"); err != nil || tag != "v1.2.3+canary.20260819T171926Z" {
		t.Fatalf("%q %v", tag, err)
	}
	if _, err := CanaryTagForDate("2026.7.20", "20260819T171926Z"); err == nil || err.Error() != "invalid stable version for canary: '2026.7.20'" {
		t.Fatalf("got %v", err)
	}
	if _, err := CanaryTagForDate("1.2.3", "2026-08-19"); err == nil || err.Error() != "invalid canary UTC timestamp: '2026-08-19'" {
		t.Fatalf("got %v", err)
	}
}

func TestChannelAdoptRetiredNoneOriginalAndMissingKeys(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	if _, err := SetInstallChannel(context.Background(), "old-preview", root); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"source": root, "home": h, "channel_retirement": map[string]any{"original": nil, "destination": "stable"}}
	if adopted, err := AdoptRetiredChannel(context.Background(), request); err != nil || !adopted {
		t.Fatalf("a None original adopts unconditionally %v %v", adopted, err)
	}
	var key *KeyError
	if _, err := AdoptRetiredChannel(context.Background(), map[string]any{"home": h, "channel_retirement": map[string]any{"original": nil, "destination": "stable"}}); !errors.As(err, &key) || err.Error() != "'source'" {
		t.Fatalf("missing source %v", err)
	}
	if _, err := AdoptRetiredChannel(context.Background(), map[string]any{"source": root, "home": h, "channel_retirement": map[string]any{"original": nil}}); !errors.As(err, &key) || key.Key != "destination" {
		t.Fatalf("missing destination %v", err)
	}
}

func TestChannelMetadataArgsPropagatesUnreadableConfig(t *testing.T) {
	h := chHome(t)
	root := filepath.Join(filepath.Dir(h), "install")
	chStamp(t, root, map[string]any{"schemaVersion": 2, "updateMechanism": "self"})
	os.WriteFile(filepath.Join(h, "config.yaml"), []byte("update: [broken"), 0o644)
	var out bytes.Buffer
	channel := "stable"
	handled, err := HandleMetadataArgs(context.Background(), &out, false, &channel, root)
	var exit *pm.SystemExit
	if err == nil || errors.As(err, &exit) || IsValueError(err) || out.Len() != 0 || !strings.Contains(err.Error(), "formatting error") {
		t.Fatalf("%v %v %q", handled, err, out.String())
	}
}
