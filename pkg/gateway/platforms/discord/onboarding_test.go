package discord

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	hermetic.BlockExternalDNS()
	os.Exit(m.Run())
}

// Records of a scripted wizard run, the env saves and removals its fakes saw
type setupIO struct {
	saved    map[string]string
	writes   [][2]string
	removed  []string
	existing map[string]string
	infos    []string
	errors   []string
}

// Replaces the cli_output and config names the wizard calls with fakes over existing values and scripted prompts
func patchSetupIO(t *testing.T, prompts []string, yesNo []bool, existing map[string]string) *setupIO {
	t.Helper()
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var out bytes.Buffer
	t.Cleanup(provider.WithSetupOutput(&out))
	savedCLI, savedConfig := gateway.WizardCLI, gateway.WizardConfig
	t.Cleanup(func() { gateway.WizardCLI, gateway.WizardConfig = savedCLI, savedConfig })
	io := &setupIO{saved: map[string]string{}, existing: existing}
	gateway.WizardConfig.GetEnvValue = func(_ context.Context, key string) (string, error) { return io.existing[key], nil }
	gateway.WizardConfig.SaveEnvValue = func(_ context.Context, key, value string) error {
		io.writes = append(io.writes, [2]string{key, value})
		io.saved[key] = value
		return nil
	}
	gateway.WizardConfig.RemoveEnvValue = func(_ context.Context, key string) (bool, error) {
		io.removed = append(io.removed, key)
		_, ok := io.existing[key]
		delete(io.existing, key)
		return ok, nil
	}
	gateway.WizardCLI.Prompt = func(string, string, bool) string {
		if len(prompts) == 0 {
			t.Fatal("unexpected prompt")
		}
		v := prompts[0]
		prompts = prompts[1:]
		return v
	}
	gateway.WizardCLI.PromptYesNo = func(context.Context, string, bool) bool {
		if len(yesNo) == 0 {
			return false
		}
		v := yesNo[0]
		yesNo = yesNo[1:]
		return v
	}
	gateway.WizardCLI.PrintHeader, gateway.WizardCLI.PrintSuccess, gateway.WizardCLI.PrintWarning = func(string) {}, func(string) {}, func(string) {}
	gateway.WizardCLI.PrintInfo = func(line string) { io.infos = append(io.infos, line) }
	gateway.WizardCLI.PrintError = func(line string) { io.errors = append(io.errors, line) }
	return io
}

// Identity of the Discord stand in, a bot whose application id equals its user id
const (
	standinAppID = "1100000000000000001"
	standinToken = "MTEwMDAwMDAwMDAwMDAwMDAwMQ.standin.token"
)

// Local Discord REST stand in answering GET /applications/@me for its one token and 401 for any other
func startDiscordStandin(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bot "+standinToken {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"message": "401: Unauthorized", "code": 0}`)
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/applications/@me") {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message": "404: Not Found", "code": 0}`)
			return
		}
		fmt.Fprintf(w, `{"id": %q, "name": "Icebath Standin", "icon": null, "description": "", "flags": %d,
			"owner": {"id": "1", "username": "owner", "discriminator": "0", "global_name": null, "avatar": null}, "team": null,
			"bot": {"id": %q, "username": "icebath-standin", "discriminator": "0", "bot": true}, "approximate_guild_count": 1}`,
			standinAppID, (1<<19)|(1<<15), standinAppID)
	}))
	t.Cleanup(srv.Close)
	saved := tools.DiscordAPIBase
	tools.DiscordAPIBase = srv.URL + "/api/v10"
	t.Cleanup(func() { tools.DiscordAPIBase = saved })
}

// Replaces the token check with one that cannot reach Discord
func offlineCheck(t *testing.T) {
	t.Helper()
	saved := checkBotTokenFn
	t.Cleanup(func() { checkBotTokenFn = saved })
	checkBotTokenFn = func(context.Context, string) (*DiscordBotCheck, error) {
		return nil, &tools.DiscordOSError{Text: "offline"}
	}
}

func TestDiscordHomeChannelClearBlankRemovesExistingHomeChannel(t *testing.T) {
	io := patchSetupIO(t, []string{"fake-bot-token.part2.part3", "", ""}, nil, map[string]string{"DISCORD_HOME_CHANNEL": "987654321098765432"})
	offlineCheck(t)
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, k := range io.removed {
		found = found || k == "DISCORD_HOME_CHANNEL"
	}
	if _, ok := io.saved["DISCORD_HOME_CHANNEL"]; !found || ok {
		t.Fatalf("removed %v saved %v", io.removed, io.saved)
	}
}

func TestDiscordTokenCheckedWithDiscordRejectedTokenNeverWrittenAndOwnerAddedToAllowlist(t *testing.T) {
	startDiscordStandin(t)
	io := patchSetupIO(t, []string{"not.the.token", "\u201c" + standinToken + "\u201d", "", ""}, []bool{true, true, true}, map[string]string{"DISCORD_ALLOWED_USERS": "999"})
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	rejected := false
	for _, e := range io.errors {
		rejected = rejected || strings.Contains(e, "rejected")
	}
	var tokenWrites []string
	for _, w := range io.writes {
		if w[0] == "DISCORD_BOT_TOKEN" {
			tokenWrites = append(tokenWrites, w[1])
		}
	}
	if !rejected || len(tokenWrites) != 1 || tokenWrites[0] != standinToken {
		t.Fatalf("errors %v token writes %v", io.errors, tokenWrites)
	}
	if io.saved["DISCORD_ALLOWED_USERS"] != "999,1" {
		t.Fatalf("allowlist %q", io.saved["DISCORD_ALLOWED_USERS"])
	}
	invite := ""
	for _, line := range io.infos {
		if strings.Contains(line, "oauth2/authorize") {
			invite = strings.TrimSpace(line)
			break
		}
	}
	if !strings.Contains(invite, "client_id="+standinAppID) || !strings.Contains(invite, fmt.Sprintf("permissions=%d", InvitePermissions)) {
		t.Fatalf("invite %q", invite)
	}
}

func TestDiscordTokenShapeGuardNumericAppIdRepromptsThenAcceptsRealToken(t *testing.T) {
	realToken := "fake-bot-token." + "part2.part3"
	io := patchSetupIO(t, []string{"1234567890123456789", realToken, "", ""}, nil, map[string]string{})
	offlineCheck(t)
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	appID := false
	for _, e := range io.errors {
		appID = appID || strings.Contains(e, "application ID")
	}
	if io.saved["DISCORD_BOT_TOKEN"] != realToken || !appID {
		t.Fatalf("saved %v errors %v", io.saved, io.errors)
	}
}

func TestDiscordTokenShapeGuardNonNumericTokenSavesWithoutError(t *testing.T) {
	io := patchSetupIO(t, []string{"fake-bot-token.part2.part3", "", ""}, nil, map[string]string{})
	offlineCheck(t)
	if err := InteractiveSetup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if io.saved["DISCORD_BOT_TOKEN"] != "fake-bot-token.part2.part3" || len(io.errors) != 0 {
		t.Fatalf("saved %v errors %v", io.saved, io.errors)
	}
}
