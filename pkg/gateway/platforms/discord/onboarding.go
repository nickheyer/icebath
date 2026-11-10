package discord

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Permission bits of every capability the adapter exercises, named so the invite integer can be re derived
var InvitePermissionBits = []struct {
	Name string
	Bit  uint
}{{"Add Reactions", 6}, {"View Channels", 10}, {"Send Messages", 11}, {"Embed Links", 14}, {"Attach Files", 15},
	{"Read Message History", 16}, {"Connect", 20}, {"Speak", 21}, {"Create Public Threads", 35}, {"Send Messages in Threads", 38}}

// Permissions integer of the invite link
var InvitePermissions = func() int64 {
	var total int64
	for _, p := range InvitePermissionBits {
		total += 1 << p.Bit
	}
	return total
}()

const teamMemberAccepted = 2

// What GET /applications/@me says about a bot token, owners being the app owner or the accepted team members
type DiscordBotCheck struct {
	AppID, BotName                string
	Owners                        [][2]string
	MessageContent, ServerMembers bool
	ServerCount                   *int64
}

// Server install invite link with the permissions the adapter uses
func (c *DiscordBotCheck) InviteURL() string {
	return "https://discord.com/oauth2/authorize?" + pyurl.URLEncode([][2]string{{"client_id", c.AppID},
		{"scope", "bot applications.commands"}, {"permissions", strconv.FormatInt(InvitePermissions, 10)}, {"integration_type", "0"}})
}

// Developer Portal bot settings page of the application
func (c *DiscordBotCheck) BotSettingsURL() string {
	return "https://discord.com/developers/applications/" + c.AppID + "/bot"
}

// Python dict of a decoded JSON value, nil for anything else
func object(v any) *pyjson.Object {
	o, _ := v.(*pyjson.Object)
	return o
}

// Value of a key of a JSON object, the KeyError of a missing one
func mustKey(o *pyjson.Object, key string) (any, error) {
	if o == nil {
		return nil, fmt.Errorf("KeyError: '%s'", key)
	}
	v, ok := o.Vals[key]
	if !ok {
		return nil, fmt.Errorf("KeyError: '%s'", key)
	}
	return v, nil
}

// Whether a decoded JSON number equals the integer n the way Python compares int and float
func numberIs(v any, n int64) bool {
	switch x := v.(type) {
	case int64:
		return x == n
	case float64:
		return x == float64(n)
	case bool:
		return (x && n == 1) || (!x && n == 0)
	}
	return false
}

// Owner ids and names of an application, the accepted team members or the single owner
func owners(app *pyjson.Object) [][2]string {
	if team := object(app.Vals["team"]); team != nil {
		var out [][2]string
		members, _ := team.Vals["members"].([]any)
		for _, raw := range members {
			m := object(raw)
			if m == nil || !numberIs(m.Vals["membership_state"], teamMemberAccepted) {
				continue
			}
			user := object(m.Vals["user"])
			if user == nil || !pyrepr.Truthy(user.Vals["id"]) {
				continue
			}
			id := pyrepr.Str(user.Vals["id"])
			name := id
			if pyrepr.Truthy(user.Vals["username"]) {
				name = pyrepr.Str(user.Vals["username"])
			}
			out = append(out, [2]string{id, name})
		}
		return out
	}
	owner := object(app.Vals["owner"])
	if owner == nil || !pyrepr.Truthy(owner.Vals["id"]) {
		return nil
	}
	id := pyrepr.Str(owner.Vals["id"])
	name := id
	if pyrepr.Truthy(owner.Vals["username"]) {
		name = pyrepr.Str(owner.Vals["username"])
	}
	return [][2]string{{id, name}}
}

// Reads the application behind a token, the Discord API error for a bad one and the OSError when unreachable
func CheckBotToken(ctx context.Context, token string) (*DiscordBotCheck, error) {
	decoded, err := tools.DiscordRequest(ctx, "GET", "/applications/@me", pystr.Strip(token), nil, nil, 10*time.Second)
	if err != nil {
		return nil, err
	}
	app := object(decoded)
	if app == nil {
		return nil, fmt.Errorf("AttributeError: '%s' object has no attribute 'get'", pyrepr.TypeName(decoded))
	}
	var flags int64
	if raw := app.Vals["flags"]; pyrepr.Truthy(raw) {
		n, err := pyint.Of(raw)
		var intErr *pyint.Error
		if errors.As(err, &intErr) && intErr.Kind == "ValueError" {
			return nil, &tools.DiscordValueError{Msg: intErr.Error()}
		}
		if err != nil {
			return nil, err
		}
		flags = n.Int64()
	}
	id, err := mustKey(app, "id")
	if err != nil {
		return nil, err
	}
	name := id
	if bot := object(app.Vals["bot"]); bot != nil && pyrepr.Truthy(bot.Vals["username"]) {
		name = bot.Vals["username"]
	} else if pyrepr.Truthy(app.Vals["name"]) {
		name = app.Vals["name"]
	}
	check := &DiscordBotCheck{AppID: pyrepr.Str(id), BotName: pyrepr.Str(name), Owners: owners(app),
		MessageContent: flags&tools.DiscordFlagsMessageContent != 0, ServerMembers: flags&tools.DiscordFlagsGuildMembers != 0}
	switch count := app.Vals["approximate_guild_count"].(type) {
	case int64:
		check.ServerCount = &count
	case bool:
		n := int64(0)
		if count {
			n = 1
		}
		check.ServerCount = &n
	}
	return check, nil
}

// Token check of the wizard, the check_bot_token name its tests replace
var checkBotTokenFn = CheckBotToken

// User ids of a comma separated list with Discord mention and user prefixes stripped
func cleanDiscordUserIDs(raw string) []string {
	var cleaned []string
	for _, uid := range strings.Split(strings.ReplaceAll(raw, " ", ""), ",") {
		uid = pystr.Strip(uid)
		if strings.HasPrefix(uid, "<@") && strings.HasSuffix(uid, ">") {
			uid = pystr.RstripChars(pystr.LstripChars(uid, "<@!"), ">")
		}
		if strings.HasPrefix(pystr.Lower(uid), "user:") {
			uid = string([]rune(uid)[5:])
		}
		if uid != "" {
			cleaned = append(cleaned, uid)
		}
	}
	return cleaned
}

// Guidance when a pasted bot token is really the numeric application id, empty otherwise
func discordTokenShapeError(token string) string {
	if token != "" && pystr.StrIsDigit(pystr.Strip(token)) {
		return "That looks like a numeric application ID, not a bot token. " +
			"Paste the bot token from the Discord Developer Portal (Bot page), " +
			"not the application ID (General Information page)."
	}
	return ""
}

// Prompts for the bot token, asking once more when the answer is a numeric application id
func promptDiscordBotToken() string {
	token := ""
	for attempt := 0; attempt < 2; attempt++ {
		token = gateway.WizardCLI.Prompt("Discord bot token", "", true)
		if token == "" {
			return ""
		}
		msg := discordTokenShapeError(token)
		if msg == "" {
			return token
		}
		gateway.WizardCLI.PrintError(msg)
	}
	return token
}

// A token Discord accepted with its check, an unverifiable one with no check, or empty when the user gave up
func promptCheckedToken(ctx context.Context) (string, *DiscordBotCheck, error) {
	cli := gateway.WizardCLI
	for attempt := 0; attempt < 3; attempt++ {
		token := pystr.Strip(config.CheckNonASCIICredential("DISCORD_BOT_TOKEN", promptDiscordBotToken()))
		if token == "" {
			return "", nil, nil
		}
		check, err := checkBotTokenFn(ctx, token)
		var value *tools.DiscordValueError
		var api *tools.DiscordAPIError
		var offline *tools.DiscordOSError
		switch {
		case err == nil:
			cli.PrintSuccess(fmt.Sprintf("Token works: this is the bot \"%s\"", check.BotName))
			return token, check, nil
		case errors.As(err, &value):
			cli.PrintError("That isn't a bot token (it contains a line break). Copy it again from the Bot page.")
		case errors.As(err, &api):
			if api.Status != 401 {
				cli.PrintWarning(fmt.Sprintf("Couldn't verify the token (Discord answered %d); saving it anyway.", api.Status))
				return token, nil, nil
			}
			cli.PrintError("Discord rejected that token. On the Bot page click Reset Token, " +
				"copy the new token and paste it here.")
		case errors.As(err, &offline):
			cli.PrintWarning(fmt.Sprintf("Couldn't reach Discord to verify the token (%s); saving it anyway.", offline.Error()))
			return token, nil, nil
		default:
			return "", nil, err
		}
	}
	cli.PrintError("Discord rejected three tokens in a row; nothing was saved.")
	return "", nil, nil
}

// Whether an error is the Discord API error or OSError class a re check gives up on
func discordCheckFailed(err error) bool {
	var api *tools.DiscordAPIError
	var offline *tools.DiscordOSError
	return errors.As(err, &api) || errors.As(err, &offline)
}

// Walks the user to turning on Message Content Intent, re checking the token after each confirmation
func ensureMessageContentIntent(ctx context.Context, token string, check *DiscordBotCheck) (*DiscordBotCheck, error) {
	cli := gateway.WizardCLI
	for attempt := 0; attempt < 5; attempt++ {
		if check.MessageContent {
			break
		}
		cli.PrintWarning("Message Content Intent is OFF: Discord will refuse the bot's connection until it's on.")
		cli.PrintInfo("   Turn it on here: " + check.BotSettingsURL())
		cli.PrintInfo("   (Privileged Gateway Intents → Message Content Intent → Save Changes)")
		if pystr.Lower(pystr.Strip(cli.Prompt("Press Enter once it's saved to re-check, or type 'skip'", "", false))) == "skip" {
			return check, nil
		}
		next, err := checkBotTokenFn(ctx, token)
		if err != nil {
			if discordCheckFailed(err) {
				return check, nil
			}
			return nil, err
		}
		check = next
	}
	if check.MessageContent {
		cli.PrintSuccess("Message Content Intent is on")
	}
	if !check.ServerMembers {
		cli.PrintInfo("Optional: turn on Server Members Intent on the same page if you want to " +
			"allow people by username or role instead of user ID.")
	}
	return check, nil
}

// Prints the invite link, saying when the bot is in no server yet
func printInvite(check *DiscordBotCheck) {
	cli := gateway.WizardCLI
	gateway.WizardPrintln("")
	if check.ServerCount != nil && *check.ServerCount == 0 {
		cli.PrintInfo("📨 The bot isn't in any server yet. Open this link to add it to yours:")
	} else {
		cli.PrintInfo("📨 Invite link (adds the bot to a server with the permissions Icebath uses):")
	}
	cli.PrintInfo("   " + check.InviteURL())
	cli.PrintInfo("   Once you share a server with the bot you can also DM it directly.")
}

// Adds the bot owners and typed users to the kept allowlist, warning when it stays empty
func promptAllowlist(ctx context.Context, check *DiscordBotCheck) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	gateway.WizardPrintln("")
	cli.PrintInfo("🔒 Security: only allowlisted Discord users can talk to your bot.")
	existing, err := cfg.GetEnvValue(ctx, "DISCORD_ALLOWED_USERS")
	if err != nil {
		return err
	}
	allowed := cleanDiscordUserIDs(existing)
	if len(allowed) > 0 {
		cli.PrintInfo("   Already allowed: " + strings.Join(allowed, ", "))
	}
	contains := func(id string) bool {
		for _, a := range allowed {
			if a == id {
				return true
			}
		}
		return false
	}
	if check != nil && len(check.Owners) > 0 {
		allIn := true
		names := make([]string, 0, len(check.Owners))
		for _, o := range check.Owners {
			allIn = allIn && contains(o[0])
			names = append(names, "@"+o[1])
		}
		if !allIn {
			who := strings.Join(names, ", ")
			question := fmt.Sprintf("Allow your team (%s) to talk to the bot?", who)
			if len(check.Owners) == 1 {
				question = fmt.Sprintf("Allow yourself (%s) to talk to the bot?", who)
			}
			if cli.PromptYesNo(ctx, question, true) {
				for _, o := range check.Owners {
					if !contains(o[0]) {
						allowed = append(allowed, o[0])
					}
				}
			}
		}
	}
	cli.PrintInfo("   Others need their Discord user ID: Settings → Advanced → Developer Mode,")
	cli.PrintInfo("   then right-click their name → Copy User ID. Usernames work too.")
	question := "Allowed user IDs or usernames (comma-separated, leave empty for open access)"
	if len(allowed) > 0 {
		question = "Other allowed users (comma-separated, Enter to skip)"
	}
	extra := cleanDiscordUserIDs(cli.Prompt(question, "", false))
	for _, uid := range extra {
		if !contains(uid) {
			allowed = append(allowed, uid)
		}
	}
	if check != nil && !check.ServerMembers {
		for _, uid := range extra {
			if !pystr.StrIsDigit(uid) {
				cli.PrintInfo("   Usernames are resolved when the gateway connects, which needs Server Members Intent: " + check.BotSettingsURL())
				break
			}
		}
	}
	if len(allowed) > 0 {
		if err := cfg.SaveEnvValue(ctx, "DISCORD_ALLOWED_USERS", strings.Join(allowed, ",")); err != nil {
			return err
		}
		cli.PrintSuccess("Discord allowlist configured")
		return nil
	}
	cli.PrintInfo("⚠️  No allowlist set. Discord will deny messages until you set " +
		"DISCORD_ALLOWED_USERS, DISCORD_ALLOWED_ROLES, DISCORD_ALLOWED_CHANNELS, " +
		"or DISCORD_ALLOW_ALL_USERS=true for open access.")
	return nil
}

// Check of the saved token, none when it is empty, rejected or unreachable
func checkSavedToken(ctx context.Context, token string) (*DiscordBotCheck, error) {
	if token == "" {
		return nil, nil
	}
	check, err := checkBotTokenFn(ctx, token)
	if err != nil && discordCheckFailed(err) {
		return nil, nil
	}
	return check, err
}

// Discord wizard of icebath gateway setup, the live checked token, intents, invite link, allowlist and home channel
func InteractiveSetup(ctx context.Context) error {
	cli, cfg := gateway.WizardCLI, gateway.WizardConfig
	cli.PrintHeader("Discord")
	declined, err := gateway.PlatformSetup.DeclinesReconfigure(ctx, "Discord", "Reconfigure Discord?", "DISCORD_BOT_TOKEN")
	if err != nil {
		return err
	}
	if declined {
		allowed, err := cfg.GetEnvValue(ctx, "DISCORD_ALLOWED_USERS")
		if err != nil || allowed != "" {
			return err
		}
		cli.PrintInfo("⚠️  Discord has no user allowlist. With the fail-closed default, " +
			"messages are denied unless you configure allowed users, roles, " +
			"or channels, or set DISCORD_ALLOW_ALL_USERS=true.")
		if cli.PromptYesNo(ctx, "Add allowed users now?", true) {
			token, err := cfg.GetEnvValue(ctx, "DISCORD_BOT_TOKEN")
			if err != nil {
				return err
			}
			saved, err := checkSavedToken(ctx, token)
			if err != nil {
				return err
			}
			return promptAllowlist(ctx, saved)
		}
		return nil
	}
	for _, line := range []string{
		"1. Open https://discord.com/developers/applications → New Application",
		"2. Open the Bot page → Reset Token → copy the token",
		"Icebath checks the token, the intents and the invite link for you next.",
		"Guide: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/discord",
	} {
		cli.PrintInfo(line)
	}
	token, check, err := promptCheckedToken(ctx)
	if err != nil || token == "" {
		return err
	}
	if err := cfg.SaveEnvValue(ctx, "DISCORD_BOT_TOKEN", token); err != nil {
		return err
	}
	cli.PrintSuccess("Discord token saved")
	if check == nil {
		cli.PrintInfo("Make sure Message Content Intent is on (Bot page → Privileged Gateway Intents), " +
			"or Discord will refuse the bot's connection.")
	} else {
		if check, err = ensureMessageContentIntent(ctx, token, check); err != nil {
			return err
		}
		printInvite(check)
	}
	if err := promptAllowlist(ctx, check); err != nil {
		return err
	}
	gateway.WizardPrintln("")
	for _, line := range []string{
		"📬 Home Channel: where Icebath delivers cron job results,",
		"   cross-platform messages, and notifications.",
		"   Easiest: type /set-home in a Discord channel once the bot is running.",
		"   Or paste a channel ID (Developer Mode → right-click a channel → Copy Channel ID).",
	} {
		cli.PrintInfo(line)
	}
	if channel := pystr.Strip(cli.Prompt("Home channel ID (leave empty to set later with /set-home)", "", false)); channel != "" {
		return cfg.SaveEnvValue(ctx, "DISCORD_HOME_CHANNEL", channel)
	}
	removed, err := cfg.RemoveEnvValue(ctx, "DISCORD_HOME_CHANNEL")
	if err == nil && removed {
		cli.PrintInfo("Home channel cleared.")
	}
	return err
}
