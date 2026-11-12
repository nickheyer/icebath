package discord

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/dlclark/regexp2/v2"
	"github.com/gorilla/websocket"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	discordMarkdownLinkLabelRE = regexp.MustCompile(`([\\\[\]])`)
	discordURLLabelSchemeRE    = regexp.MustCompile(`(?i)^https?://`)
)

const (
	discordCommandSyncStateSubdir         = "gateway"
	discordNonconversationalStateFilename = "discord_nonconversational_messages.json"
	discordAppCommandTextLimit            = 100
	discordImageMaxRedirects              = 10
)

var discordNonconversationalMetadataKeys = []string{"non_conversational", "non_conversational_history"}

var discordImageRedirectStatuses = map[int]bool{301: true, 302: true, 303: true, 307: true, 308: true}

var discordNonconversationalHistoryMessagePatterns = func() []*regexp2.Regexp {
	var out []*regexp2.Regexp
	for _, p := range []string{
		`^\s*💾\s*Self-improvement review:\s+\S[\s\S]*$`,
		`^\s*💾\s+Skill\s+['"].+?['"]\s+(?:created|updated|improved|patched)\.?\s*$`,
		`^\s*⏳\s+Working\s+—\s+\d+\s+min(?:\s|$)`,
		`^\s*\[Background process\s+\S+\s+(?:finished with exit code|is still running~)[\s\S]*\]\s*$`,
		`^\s*(?:✅|❌)\s+Hermes update\s+(?:finished|failed|timed out)[\s\S]*$`,
		`^\s*♻️?\s+Gateway\s+(?:restarted successfully|online\b)[\s\S]*$`,
	} {
		re, err := pyre.CompileIgnoreCase(p)
		if err != nil {
			panic(err)
		}
		out = append(out, re)
	}
	return out
}()

// Attachment extension for a downloaded image, png unless jpeg gif or webp is evident
func imageExtFromContentType(contentType string) string {
	switch {
	case strings.Contains(contentType, "jpeg") || strings.Contains(contentType, "jpg"):
		return "jpg"
	case strings.Contains(contentType, "gif"):
		return "gif"
	case strings.Contains(contentType, "webp"):
		return "webp"
	}
	return "png"
}

// Discord markdown link whose label is never a bare URL and whose angle bracketed target never unfurls
func formatDiscordMarkdownLink(label, rawURL string) string {
	if loc := discordURLLabelSchemeRE.FindStringIndex(label); loc != nil {
		label = label[loc[1]:]
	}
	escapedLabel := discordMarkdownLinkLabelRE.ReplaceAllString(label, `\$1`)
	escapedURL := pyurl.Quote(rawURL, ":/?#[]@!$&'*+,;=%")
	return "[" + escapedLabel + "](<" + escapedURL + ">)"
}

// Phrases spoken while the agent works, overridden by voice_fx.ack_phrases in config
func defaultVoiceAckPhrases(ctx context.Context) []string {
	out := make([]string, 0, 5)
	for i := 1; i <= 5; i++ {
		out = append(out, i18n.T(ctx, fmt.Sprintf("platform.discord.voice.ack_%d", i), "", nil))
	}
	return out
}

// Translated catalog text cut to a Discord field cap in UTF-16 units
func tDiscord(ctx context.Context, key string, limit int, kwargs map[string]any) string {
	return truncateDiscordComponentText(i18n.T(ctx, key, "", kwargs), limit)
}

// Native slash command argument, its catalog description key and optional fixed choices
type slashArgSpec struct {
	Name     string
	IsInt    bool
	Required bool
	Default  any
	Desc     string
	Choices  [][2]string
}

// Native slash command, a nil template marking the thread command registered by its own handler
type slashCommandSpec struct {
	Name        string
	Desc        string
	Args        []slashArgSpec
	Template    *string
	FollowupKey string
}

func tpl(s string) *string { return &s }

func strArg(name, descKey string) slashArgSpec {
	return slashArgSpec{Name: name, Default: "", Desc: descKey}
}

func requiredArg(name, descKey string) slashArgSpec {
	return slashArgSpec{Name: name, Required: true, Desc: descKey}
}

var nativeSlashCommandSpecs = []slashCommandSpec{
	{"new", "platform.discord.command.new.description", nil, tpl("/reset"), "platform.discord.command.new.followup"},
	{"reset", "platform.discord.command.reset.description", nil, tpl("/reset"), "platform.discord.command.reset.followup"},
	{"model", "platform.discord.command.model.description", []slashArgSpec{strArg("name", "platform.discord.command.model.arg_name")}, tpl("/model {name}"), ""},
	{"reasoning", "platform.discord.command.reasoning.description", []slashArgSpec{{Name: "effort", Default: "", Desc: "platform.discord.command.reasoning.arg_effort", Choices: [][2]string{
		{"platform.discord.command.reasoning.choice_none", "none"}, {"minimal", "minimal"}, {"low", "low"},
		{"medium", "medium"}, {"high", "high"}, {"xhigh", "xhigh"}, {"max", "max"},
		{"platform.discord.command.reasoning.choice_ultra", "ultra"}, {"platform.discord.command.reasoning.choice_reset", "reset"},
		{"platform.discord.command.reasoning.choice_show", "show"}, {"platform.discord.command.reasoning.choice_hide", "hide"}}}}, tpl("/reasoning {effort}"), ""},
	{"personality", "platform.discord.command.personality.description", []slashArgSpec{strArg("name", "platform.discord.command.personality.arg_name")}, tpl("/personality {name}"), ""},
	{"retry", "platform.discord.command.retry.description", nil, tpl("/retry"), "platform.discord.command.retry.followup"},
	{"undo", "platform.discord.command.undo.description", nil, tpl("/undo"), ""},
	{"status", "platform.discord.command.status.description", nil, tpl("/status"), "platform.discord.command.status.followup"},
	{"sethome", "slash.sethome.description", nil, tpl("/sethome"), ""},
	{"stop", "platform.discord.command.stop.description", nil, tpl("/stop"), "platform.discord.command.stop.followup"},
	{"steer", "platform.discord.command.steer.description", []slashArgSpec{requiredArg("prompt", "platform.discord.command.steer.arg_prompt")}, tpl("/steer {prompt}"), ""},
	{"plan", "platform.discord.command.plan.description", []slashArgSpec{strArg("task", "platform.discord.command.plan.arg_task")}, tpl("/plan {task}"), ""},
	{"compress", "platform.discord.command.compress.description", nil, tpl("/compress"), ""},
	{"title", "platform.discord.command.title.description", []slashArgSpec{strArg("name", "platform.discord.command.title.arg_name")}, tpl("/title {name}"), ""},
	{"resume", "slash.resume.description", []slashArgSpec{strArg("name", "platform.discord.command.resume.arg_name")}, tpl("/resume {name}"), ""},
	{"usage", "platform.discord.command.usage.description", nil, tpl("/usage"), ""},
	{"help", "platform.discord.command.help.description", nil, tpl("/help"), ""},
	{"insights", "slash.insights.description", []slashArgSpec{{Name: "days", IsInt: true, Default: 7, Desc: "platform.discord.command.insights.arg_days"}}, tpl("/insights {days}"), ""},
	{"reload-mcp", "slash.reload_mcp.description", nil, tpl("/reload-mcp"), ""},
	{"reload-skills", "platform.discord.command.reload_skills.description", nil, tpl("/reload-skills"), ""},
	{"voice", "platform.discord.command.voice.description", []slashArgSpec{{Name: "mode", Default: "", Desc: "platform.discord.command.voice.arg_mode", Choices: [][2]string{
		{"platform.discord.command.voice.choice_join", "join"}, {"platform.discord.command.voice.choice_channel", "channel"},
		{"platform.discord.command.voice.choice_leave", "leave"}, {"platform.discord.command.voice.choice_mode_on", "on"},
		{"platform.discord.command.voice.choice_tts", "tts"}, {"platform.discord.command.voice.choice_mode_off", "off"},
		{"platform.discord.command.voice.choice_status", "status"}}}}, tpl("/voice {mode}"), ""},
	{"update", "slash.update.description", nil, tpl("/update"), "platform.discord.command.update.followup"},
	{"restart", "platform.discord.command.restart.description", nil, tpl("/restart"), "platform.discord.command.restart.followup"},
	{"approve", "slash.approve.description", []slashArgSpec{strArg("scope", "platform.discord.command.approve.arg_scope")}, tpl("/approve {scope}"), ""},
	{"deny", "platform.discord.command.deny.description", []slashArgSpec{strArg("scope", "platform.discord.command.deny.arg_scope")}, tpl("/deny {scope}"), ""},
	{"thread", "platform.discord.command.thread.description", nil, nil, ""},
	{"queue", "platform.discord.command.queue.description", []slashArgSpec{requiredArg("prompt", "platform.discord.command.queue.arg_prompt")}, tpl("/queue {prompt}"), "platform.discord.command.queue.followup"},
	{"bg", "slash.bg.description", []slashArgSpec{requiredArg("prompt", "platform.discord.command.bg.arg_prompt")}, tpl("/bg {prompt}"), "platform.discord.command.bg.followup"},
	{"btw", "platform.discord.command.btw.description", []slashArgSpec{requiredArg("question", "platform.discord.command.btw.arg_question")}, tpl("/btw {question}"), "platform.discord.command.btw.followup"},
}

// Native slash specs with descriptions, argument descriptions and choice labels resolved for the active language
func nativeSlashCommands(ctx context.Context) []slashCommandSpec {
	text := func(key string) string { return tDiscord(ctx, key, discordAppCommandTextLimit, nil) }
	choiceLabel := func(label string) string {
		if strings.Contains(label, ".") {
			return text(label)
		}
		return label
	}
	out := make([]slashCommandSpec, 0, len(nativeSlashCommandSpecs))
	for _, spec := range nativeSlashCommandSpecs {
		args := make([]slashArgSpec, 0, len(spec.Args))
		for _, a := range spec.Args {
			la := a
			la.Desc = text(a.Desc)
			if len(a.Choices) > 0 {
				la.Choices = make([][2]string, 0, len(a.Choices))
				for _, c := range a.Choices {
					la.Choices = append(la.Choices, [2]string{choiceLabel(c[0]), c[1]})
				}
			}
			args = append(args, la)
		}
		out = append(out, slashCommandSpec{Name: spec.Name, Desc: text(spec.Desc), Args: args, Template: spec.Template, FollowupKey: spec.FollowupKey})
	}
	return out
}

// Reports a connection shaped send failure that never reached Discord, timeouts excluded since they may have landed
func isDiscordTransportError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, errDiscordReadyTimeout) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return false
	}
	var opErr *net.OpError
	var sysErr *os.SyscallError
	var pathErr *fs.PathError
	var errno syscall.Errno
	if errors.As(err, &opErr) || errors.As(err, &sysErr) || errors.As(err, &pathErr) || errors.As(err, &errno) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return true
	}
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) || errors.Is(err, discordgo.ErrWSNotFound) {
		return true
	}
	var restErr *discordgo.RESTError
	if errors.As(err, &restErr) && restErr.Response != nil && restErr.Response.StatusCode >= 500 {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, marker := range []string{"websocket closed", "connection reset", "connection closed", "session is closed",
		"cannot write to closing transport", "not connected"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// Reads an image URL without following redirects automatically, rechecking every redirect target for SSRF
func readURLImageWithRedirectGuard(ctx context.Context, env *tools.Env, client *http.Client, rawURL string, timeout time.Duration, header http.Header) (int, []byte, map[string]string, error) {
	guarded := *client
	guarded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	guarded.Timeout = timeout
	currentURL := rawURL
	for i := 0; i < discordImageMaxRedirects+1; i++ {
		if !tools.IsSafeURL(env, currentURL) {
			return 0, nil, nil, errors.New("Blocked unsafe image URL redirect")
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, currentURL, nil)
		if err != nil {
			return 0, nil, nil, err
		}
		for k, vs := range header {
			req.Header[k] = vs
		}
		resp, err := guarded.Do(req)
		if err != nil {
			return 0, nil, nil, err
		}
		headers := map[string]string{}
		for k, vs := range resp.Header {
			if len(vs) > 0 {
				headers[strings.ToLower(k)] = vs[len(vs)-1]
			}
		}
		if discordImageRedirectStatuses[resp.StatusCode] {
			resp.Body.Close()
			location := headers["location"]
			if location == "" {
				return resp.StatusCode, []byte{}, headers, nil
			}
			base, err := url.Parse(currentURL)
			if err != nil {
				return 0, nil, nil, err
			}
			ref, err := url.Parse(location)
			if err != nil {
				return 0, nil, nil, err
			}
			nextURL := base.ResolveReference(ref).String()
			if !tools.IsSafeURL(env, nextURL) {
				return 0, nil, nil, errors.New("Blocked redirect to private/internal address")
			}
			currentURL = nextURL
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return 0, nil, nil, err
		}
		return resp.StatusCode, body, headers, nil
	}
	return 0, nil, nil, errors.New("Too many image URL redirects")
}

// Text cut to a Discord component field budget in UTF-16 units
func truncateDiscordComponentText(text string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	return gateway.PrefixWithinUTF16Limit(text, limit)
}

// Records the raw socket under the gateway websocket so a hung close can be aborted
type wsTransport struct {
	conn net.Conn
}

// Aborts the active websocket transport after a bounded close times out, false when no socket is live
func abortDiscordWebsocketTransport(t *wsTransport) bool {
	if t == nil || t.conn == nil {
		return false
	}
	t.conn.Close()
	return true
}

// Sentinel raised when Discord never becomes ready within the startup wait
var errDiscordReadyTimeout = errors.New("TimeoutError")

// Lifetime of the gateway session goroutine, its error set before done closes
type botTask struct {
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func newBotTask(cancel context.CancelFunc) *botTask {
	return &botTask{done: make(chan struct{}), cancel: cancel}
}

// Ends the task with its exit error
func (t *botTask) finish(err error) {
	t.err = err
	close(t.done)
}

// Whether the task has exited
func (t *botTask) isDone() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
}

// Waits for ready or surfaces an early bot exit, a negative timeout waiting forever
var waitForReadyOrBotExit = func(ctx context.Context, ready <-chan struct{}, bot *botTask, timeout time.Duration) error {
	var timer <-chan time.Time
	if timeout >= 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ready:
	case <-bot.done:
	case <-timer:
		return errDiscordReadyTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
	if bot.isDone() {
		if bot.err != nil {
			return bot.err
		}
		select {
		case <-ready:
		default:
			return errors.New("Discord bot task exited before ready")
		}
	}
	return nil
}

// Whether the Server Members intent is needed for username allowlist entries or role allowlists
func needsServerMembersIntent(allowedUserIDs, allowedRoleIDs []string) bool {
	for _, entry := range allowedUserIDs {
		if entry != "*" && !isDigitString(entry) {
			return true
		}
	}
	return len(allowedRoleIDs) > 0
}

// Python str.isdigit, non empty with every rune a digit
func isDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !pystr.IsDigit(r) {
			return false
		}
	}
	return true
}

// Actionable fix text when Discord rejects privileged Gateway Intents
func formatPrivilegedIntentsGuidance(needsMembers bool) string {
	lines := []string{
		"Discord rejected the connection because privileged Gateway Intents are not enabled for this bot in the Developer Portal.",
		"Hermes is requesting:",
		"  - Message Content Intent (required to read message text)",
	}
	if needsMembers {
		lines = append(lines, "  - Server Members Intent (required for username allowlists and/or DISCORD_ALLOWED_ROLES)")
	}
	lines = append(lines,
		"Fix: https://discord.com/developers/applications → your application → Bot → Privileged Gateway Intents → enable the intent(s) listed above → Save Changes, then restart the gateway.",
		"Docs: https://hermes-agent.nousresearch.com/docs/user-guide/messaging/discord",
	)
	return strings.Join(lines, "\n")
}

// UTC creation time encoded in a Discord snowflake
func discordSnowflakeTime(snowflake uint64) time.Time {
	return time.UnixMilli(int64(snowflake>>22) + 1420070400000).UTC()
}

// Whether an outbound send was explicitly marked as status only
func metadataMarksNonconversational(metadata map[string]any) bool {
	if metadata == nil {
		return false
	}
	for _, key := range discordNonconversationalMetadataKeys {
		if pyrepr.Truthy(metadata[key]) {
			return true
		}
	}
	return false
}

// Interactive prompt target, the metadata thread when present else the chat
func promptTargetID(chatID string, metadata map[string]any) string {
	if metadata != nil && pyrepr.Truthy(metadata["thread_id"]) {
		return pyrepr.Str(metadata["thread_id"])
	}
	return chatID
}

// Fallback recognizer for legacy status bumps missing persisted ids
func looksLikeNonconversationalHistoryMessage(content string) bool {
	for _, pattern := range discordNonconversationalHistoryMessagePatterns {
		if ok, _ := pattern.MatchString(content); ok {
			return true
		}
	}
	return false
}

// Strips pasted user prefixes and mention brackets to a bare id or username
func cleanDiscordID(entry string) string {
	entry = pystr.Strip(entry)
	if strings.HasPrefix(entry, "<@") && strings.HasSuffix(entry, ">") {
		entry = strings.TrimRight(strings.TrimLeft(entry, "<@!"), ">")
	}
	if strings.HasPrefix(strings.ToLower(entry), "user:") {
		entry = string([]rune(entry)[5:])
	}
	return pystr.Strip(entry)
}

// Allowed mentions denying everyone here and role pings by default, overridable per profile in config or env
func buildAllowedMentions(ctx context.Context, extra map[string]any) (*discordgo.MessageAllowedMentions, error) {
	configured, _ := extra["allow_mentions"].(map[string]any)
	if configured == nil {
		configured = map[string]any{}
	}
	b := func(name, key string, def bool) (bool, error) {
		raw, err := gateway.ExtraOrSecret(ctx, configured, key, name, nil, false)
		if err != nil {
			return false, err
		}
		if raw == nil {
			return def, nil
		}
		if v, ok := raw.(bool); ok {
			return v, nil
		}
		switch strings.ToLower(pystr.Strip(pyrepr.Str(raw))) {
		case "true", "1", "yes", "on":
			return true, nil
		}
		return false, nil
	}
	am := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	for _, row := range []struct {
		env, key string
		def      bool
		kind     discordgo.AllowedMentionType
	}{
		{"DISCORD_ALLOW_MENTION_EVERYONE", "everyone", false, discordgo.AllowedMentionTypeEveryone},
		{"DISCORD_ALLOW_MENTION_ROLES", "roles", false, discordgo.AllowedMentionTypeRoles},
		{"DISCORD_ALLOW_MENTION_USERS", "users", true, discordgo.AllowedMentionTypeUsers},
	} {
		on, err := b(row.env, row.key, row.def)
		if err != nil {
			return nil, err
		}
		if on {
			am.Parse = append(am.Parse, row.kind)
		}
	}
	replied, err := b("DISCORD_ALLOW_MENTION_REPLIED_USER", "replied_user", true)
	if err != nil {
		return nil, err
	}
	am.RepliedUser = replied
	return am, nil
}

// Seconds the gateway startup waits for Discord ready, overridable by env
func discordReadyTimeoutSeconds() float64 {
	raw := pystr.Strip(os.Getenv("ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT"))
	if raw != "" {
		v, err := pyint.ParseFloat(raw)
		if err == nil {
			if v > 0 {
				return v
			}
			return 0
		}
		slog.Warn(fmt.Sprintf("Ignoring invalid ICEBATH_GATEWAY_PLATFORM_CONNECT_TIMEOUT=%s", pyrepr.Repr(raw)))
	}
	return 30.0
}
