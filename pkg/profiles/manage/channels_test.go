package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/profiles"
)

const channelSourceEnv = "OPENAI_API_KEY=sk-model-key\n" +
	"FIRECRAWL_API_KEY=fc-tool-key\n" +
	"# telegram\n" +
	"TELEGRAM_BOT_TOKEN=111111:default-telegram-token\n" +
	"TELEGRAM_ALLOWED_USERS=12345\n" +
	"TELEGRAM_GROUP_ALLOWED_CHATS=-100999\n" +
	"DISCORD_BOT_TOKEN=default-discord-token-abcdef\n" +
	"DISCORD_ALLOWED_USERS=777\n" +
	"WHATSAPP_ENABLED=true\n" +
	"API_SERVER_KEY=default-api-server-key-0123456789\n"

const channelSourceConfig = `model: {default: gpt-5, provider: openai}
memory: {provider: builtin}
platforms:
  telegram: {enabled: true, token: "111111:default-telegram-token"}
  discord: {enabled: true}
telegram: {reactions: true, allowed_chats: "-100999"}
discord: {require_mention: false, dm_role_auth_guild: "42"}
gateway:
  multiplex_profiles: true
  profile_routes: [{profile: x, platform: telegram}]
  platform_connect_timeout: 45
`

// Default home seeded with the channel source env, config and soul, returning the default home
func channelHome(t *testing.T) string {
	t.Helper()
	tmp := manageEnv(t)
	for _, name := range []string{"TELEGRAM_BOT_TOKEN", "DISCORD_BOT_TOKEN", "API_SERVER_KEY", "WHATSAPP_ENABLED",
		"GATEWAY_MULTIPLEX_PROFILES", "TELEGRAM_ALLOWED_USERS"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	root := filepath.Join(tmp, ".icebath")
	writeFile(t, filepath.Join(root, ".env"), channelSourceEnv)
	writeFile(t, filepath.Join(root, "config.yaml"), channelSourceConfig)
	writeFile(t, filepath.Join(root, "SOUL.md"), "Be helpful.")
	return root
}

func loadYAML(t *testing.T, p string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := yaml.Unmarshal([]byte(readText(t, p)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Assigned keys of a profile .env, comments skipped
func envKeys(t *testing.T, p string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(readText(t, p), "\n") {
		if strings.Contains(line, "=") && !strings.HasPrefix(line, "#") {
			keys = append(keys, strings.SplitN(line, "=", 2)[0])
		}
	}
	sort.Strings(keys)
	return keys
}

func mustCreate(t *testing.T, name string, o CreateOptions) string {
	t.Helper()
	dir, err := CreateProfile(context.Background(), name, o)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCloneStripsEveryChannelCredentialButKeepsModelAndToolKeys(t *testing.T) {
	root := channelHome(t)
	dir := mustCreate(t, "bot2", CreateOptions{CloneConfig: true})
	shared, err := SharedChannelCredentials(context.Background(), dir, root)
	if err != nil || len(shared) != 0 {
		t.Fatalf("shared = %v, %v", shared, err)
	}
	env := readText(t, filepath.Join(dir, ".env"))
	if !strings.Contains(env, "OPENAI_API_KEY=sk-model-key") || !strings.Contains(env, "FIRECRAWL_API_KEY=fc-tool-key") {
		t.Fatalf("model or tool key lost: %q", env)
	}
	for _, gone := range []string{"TELEGRAM", "DISCORD", "WHATSAPP_ENABLED", "API_SERVER_KEY"} {
		if strings.Contains(env, gone) {
			t.Fatalf("%s survived: %q", gone, env)
		}
	}
	cfg := loadYAML(t, filepath.Join(dir, "config.yaml"))
	if !reflect.DeepEqual(cfg["model"], map[string]any{"default": "gpt-5", "provider": "openai"}) ||
		!reflect.DeepEqual(cfg["memory"], map[string]any{"provider": "builtin"}) {
		t.Fatalf("general config lost: %v", cfg)
	}
	if got := readText(t, filepath.Join(dir, "SOUL.md")); got != "Be helpful." {
		t.Fatalf("soul = %q", got)
	}
	for _, section := range []string{"platforms", "telegram", "discord"} {
		if _, ok := cfg[section]; ok {
			t.Fatalf("%s survived", section)
		}
	}
	gw := cfg["gateway"].(map[string]any)
	if _, ok := gw["multiplex_profiles"]; ok {
		t.Fatal("multiplex_profiles survived")
	}
	if _, ok := gw["profile_routes"]; ok {
		t.Fatal("profile_routes survived")
	}
	if gw["platform_connect_timeout"] != 45 {
		t.Fatalf("gateway knob lost: %v", gw)
	}
}

func TestCloneChannelsOptInKeepsTheSourceChannels(t *testing.T) {
	root := channelHome(t)
	dir := mustCreate(t, "twin", CreateOptions{CloneConfig: true, CloneChannels: true})
	ctx := context.Background()
	shared, err := SharedChannelCredentials(ctx, dir, root)
	if err != nil || !containsAll(shared, "telegram", "discord") {
		t.Fatalf("shared = %v, %v", shared, err)
	}
	configured, err := ChannelPlatformsConfigured(ctx, dir)
	if err != nil || !containsAll(configured, "telegram", "discord", "whatsapp", "api_server") {
		t.Fatalf("configured = %v, %v", configured, err)
	}
}

func containsAll(list []string, want ...string) bool {
	for _, w := range want {
		if !contains(list, w) {
			return false
		}
	}
	return true
}

func TestCloneAllDropsPairingAndPlatformState(t *testing.T) {
	root := channelHome(t)
	writeFile(t, filepath.Join(root, "platforms", "pairing", "telegram_approved.json"), "{}")
	writeFile(t, filepath.Join(root, "discord_threads.json"), "{}")
	writeFile(t, filepath.Join(root, "memories", "MEMORY.md"), "remember")
	dir := mustCreate(t, "full", CreateOptions{CloneAll: true})
	if pathExists(filepath.Join(dir, "platforms")) || pathExists(filepath.Join(dir, "discord_threads.json")) {
		t.Fatal("platform state survived")
	}
	if got := readText(t, filepath.Join(dir, "memories", "MEMORY.md")); got != "remember" {
		t.Fatalf("memory = %q", got)
	}
}

func TestStripChannelConfigCanDeliberatelyRemoveMostTopLevelKeys(t *testing.T) {
	manageEnv(t)
	p := filepath.Join(t.TempDir(), "config.yaml")
	writeFile(t, p, "discord: {require_mention: false}\ngateway:\n  multiplex_profiles: true\n  profile_routes: [{profile: x}]\n"+
		"model: {default: gpt-5}\nplatforms:\n  telegram: {enabled: true}\ntelegram: {reactions: true}\n")
	removed, err := StripChannelConfig(context.Background(), p, &ChannelKeyIndex{Platforms: []string{"telegram", "discord"}})
	if err != nil {
		t.Fatal(err)
	}
	if !containsAll(removed, "platforms", "telegram", "discord", "gateway.multiplex_profiles", "gateway.profile_routes") {
		t.Fatalf("removed = %v", removed)
	}
	if got := loadYAML(t, p); !reflect.DeepEqual(got, map[string]any{"model": map[string]any{"default": "gpt-5"}}) {
		t.Fatalf("config = %v", got)
	}
}

func TestStripEnvFileKeepsCommentsAndUnknownKeysVerbatim(t *testing.T) {
	manageEnv(t)
	env := filepath.Join(t.TempDir(), ".env")
	writeFile(t, env, "# header\nexport OPENAI_API_KEY=abc\n\nTELEGRAM_BOT_TOKEN=1:x\nMY_CUSTOM_THING=1\n")
	removed, err := StripChannelEnvFile(context.Background(), env, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := pyjson.NewObject()
	want.Set("telegram", []any{"TELEGRAM_BOT_TOKEN"})
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %#v", removed)
	}
	if got := readText(t, env); got != "# header\nexport OPENAI_API_KEY=abc\n\nMY_CUSTOM_THING=1\n" {
		t.Fatalf("env = %q", got)
	}
}

func TestCloneAllNeverWritesThroughASymlinkedSourceEnv(t *testing.T) {
	root := channelHome(t)
	shared := filepath.Join(t.TempDir(), "shared-secrets.env")
	writeFile(t, shared, channelSourceEnv)
	os.Remove(filepath.Join(root, ".env"))
	if err := os.Symlink(shared, filepath.Join(root, ".env")); err != nil {
		t.Skip("symlinks unavailable")
	}
	dir := mustCreate(t, "full", CreateOptions{CloneAll: true})
	if readText(t, shared) != channelSourceEnv {
		t.Fatal("source env rewritten through the link")
	}
	if st, err := os.Lstat(filepath.Join(dir, ".env")); err != nil || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("clone env is a link: %v", err)
	}
	if strings.Contains(readText(t, filepath.Join(dir, ".env")), "TELEGRAM_BOT_TOKEN") {
		t.Fatal("bot token cloned")
	}
}

func servedNames(t *testing.T) []string {
	t.Helper()
	served, err := profiles.ProfilesToServe(context.Background(), true, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range served {
		names = append(names, s.Name)
	}
	return names
}

func TestCloneIsPublishedAtomicallyAfterStripping(t *testing.T) {
	root := channelHome(t)
	type seenState struct {
		finalExists, workDirHidden bool
		served                     []string
	}
	var seen seenState
	real := stripChannelSettingsFn
	stripChannelSettingsFn = func(ctx context.Context, profileDir string, includeState bool, sourceDir string) (*pyjson.Object, error) {
		seen = seenState{pathExists(filepath.Join(root, "profiles", "bot2")), strings.HasPrefix(filepath.Base(profileDir), "."), servedNames(t)}
		return real(ctx, profileDir, includeState, sourceDir)
	}
	t.Cleanup(func() { stripChannelSettingsFn = real })
	dir := mustCreate(t, "bot2", CreateOptions{CloneConfig: true})
	if !reflect.DeepEqual(seen, seenState{false, true, []string{"default"}}) {
		t.Fatalf("seen = %+v", seen)
	}
	if !isDir(dir) || !reflect.DeepEqual(servedNames(t), []string{"default", "bot2"}) {
		t.Fatalf("served after = %v", servedNames(t))
	}
	entries, _ := os.ReadDir(filepath.Join(root, "profiles"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("staging dir left: %s", e.Name())
		}
	}
}

func TestCloneChannelsRefusalLivesInCreateProfile(t *testing.T) {
	root := channelHome(t)
	real := recordedServedProfiles
	recordedServedProfiles = func() ([]string, bool, error) { return []string{"default", "other"}, true, nil }
	t.Cleanup(func() { recordedServedProfiles = real })
	ctx := context.Background()
	var ve *profiles.ValueError
	if _, err := CreateProfile(ctx, "twin", CreateOptions{CloneConfig: true, CloneChannels: true}); !errors.As(err, &ve) || !strings.Contains(err.Error(), "already serves") {
		t.Fatalf("err = %v", err)
	}
	if pathExists(filepath.Join(root, "profiles", "twin")) {
		t.Fatal("refused clone left a profile")
	}
	if _, err := CreateProfile(ctx, "plain", CreateOptions{CloneChannels: true}); !errors.As(err, &ve) || !strings.Contains(err.Error(), "only applies to a clone") {
		t.Fatalf("err = %v", err)
	}
	if !isDir(mustCreate(t, "twin", CreateOptions{CloneConfig: true})) {
		t.Fatal("plain clone failed")
	}
}

const channelSharedEnv = "OPENAI_API_KEY=sk\n" +
	"GATEWAY_ALLOW_ALL_USERS=true\nGATEWAY_ALLOWED_USERS=1,2\n" +
	"GATEWAY_RELAY_ID=gw-1\nGATEWAY_RELAY_SECRET=s\nGATEWAY_RELAY_DELIVERY_KEY=k\n" +
	"WECOM_DM_POLICY=open\nSMS_WEBHOOK_PORT=8700\n" +
	"TWILIO_ACCOUNT_SID=AC1\nTWILIO_AUTH_TOKEN=tw\nTWILIO_PHONE_NUMBER=+1\n" +
	"EMAIL_ADDRESS=a@b\nEMAIL_PASSWORD=p\nEMAIL_SMTP_HOST=smtp\nEMAIL_IMAP_HOST=imap\nEMAIL_ALLOWED_USERS=x@y\n"

func TestOwnershipInventoryStripsPolicyRelayAndAliasesButKeepsToolCredentials(t *testing.T) {
	root := channelHome(t)
	writeFile(t, filepath.Join(root, ".env"), channelSharedEnv)
	writeFile(t, filepath.Join(root, "config.yaml"), "model: {default: gpt-5, provider: openai}\nplatforms:\n  sms: {enabled: false}\n")
	dir := mustCreate(t, "bot3", CreateOptions{CloneConfig: true})
	want := []string{"OPENAI_API_KEY", "TWILIO_ACCOUNT_SID", "TWILIO_AUTH_TOKEN", "TWILIO_PHONE_NUMBER"}
	if got := envKeys(t, filepath.Join(dir, ".env")); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v", got)
	}
}

func TestLeftCorePlatformKeepsChannelOwnershipWhileItsPluginIsAbsent(t *testing.T) {
	cases := []struct {
		name, config string
		stripped     bool
	}{
		{"ha11", "gateway:\n  homeassistant: {enabled: true, token: ha-short-token}\n", true},
		{"ha01", "", true},
		{"ha10", "platforms:\n  homeassistant: {enabled: false}\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := channelHome(t)
			writeFile(t, filepath.Join(root, ".env"), "OPENAI_API_KEY=sk\nHASS_TOKEN=ha-token\nHASS_URL=http://ha.local\n")
			writeFile(t, filepath.Join(root, "config.yaml"), "model: {default: gpt-5, provider: openai}\n"+c.config)
			if pathExists(filepath.Join(root, "plugins", "homeassistant")) {
				t.Fatal("plugin present")
			}
			dir := mustCreate(t, c.name, CreateOptions{CloneConfig: true})
			want := []string{"HASS_TOKEN", "HASS_URL", "OPENAI_API_KEY"}
			if c.stripped {
				want = []string{"OPENAI_API_KEY"}
			}
			if got := envKeys(t, filepath.Join(dir, ".env")); !reflect.DeepEqual(got, want) {
				t.Fatalf("keys = %v", got)
			}
			if strings.Contains(readText(t, filepath.Join(dir, "config.yaml")), "ha-short-token") {
				t.Fatal("platform token cloned")
			}
		})
	}
}

func TestCloneAllDropsDirectoryShapedChannelState(t *testing.T) {
	root := channelHome(t)
	writeFile(t, filepath.Join(root, "google_chat_user_tokens", "u@x.json"), "{}")
	dir := mustCreate(t, "full", CreateOptions{CloneAll: true, CloneFrom: nil})
	if pathExists(filepath.Join(dir, "google_chat_user_tokens")) {
		t.Fatal("directory shaped channel state survived")
	}
}

func TestBackfillNeverCopiesDefaultCredentials(t *testing.T) {
	tmp := manageEnv(t)
	writeFile(t, filepath.Join(tmp, ".icebath", ".env"), "OPENROUTER_API_KEY=root-key\nTELEGRAM_BOT_TOKEN=123:abc\nDISCORD_BOT_TOKEN=d1\n")
	p := mustCreate(t, "legacy", CreateOptions{NoSkills: true})
	os.Remove(filepath.Join(p, ".env"))
	got, err := BackfillProfileEnvs(true)
	if err != nil || !reflect.DeepEqual(got, []string{"legacy"}) {
		t.Fatalf("backfill = %v, %v", got, err)
	}
	for _, line := range strings.Split(strings.TrimPrefix(readText(t, filepath.Join(p, ".env")), "\ufeff"), "\n") {
		if !strings.HasPrefix(line, "#") && strings.TrimSpace(line) != "" {
			t.Fatalf("assignment copied: %q", line)
		}
	}
}

func TestChannelMessageTexts(t *testing.T) {
	if got := FormatStrippedNotice("p", nil, "--clone"); got != nil {
		t.Fatalf("notice = %v", got)
	}
	lines := FormatStrippedNotice("p", []string{"discord", "telegram"}, "--clone-all")
	if len(lines) != 3 || !strings.Contains(lines[0], "(discord, telegram)") || !strings.HasSuffix(lines[2], "icebath profile create p --clone-all --clone-channels") {
		t.Fatalf("notice = %v", lines)
	}
	w := SharedCredentialWarning("p", []string{"telegram"}, "default")
	if !strings.HasPrefix(w, "⚠ Profile 'p' shares its telegram credential with default:") || !strings.Contains(w, "icebath -p p setup") {
		t.Fatalf("warning = %q", w)
	}
	if got := PlatformEnvPrefixes("wecom-callback"); !reflect.DeepEqual(got, []string{"WECOM_CALLBACK_"}) {
		t.Fatalf("prefixes = %v", got)
	}
	if got := PlatformEnvPrefixes("sms"); !reflect.DeepEqual(got, []string{"SMS_", "TWILIO_"}) {
		t.Fatalf("prefixes = %v", got)
	}
}
