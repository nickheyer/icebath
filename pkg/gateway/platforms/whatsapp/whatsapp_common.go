package whatsapp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

// Bridge directory for the CLI and adapter, a read only install tree mirrored under the home
func ResolveWhatsAppBridgeDir(ctx context.Context) string {
	installBridge := filepath.Join(pm.RepoRoot(), "scripts", "whatsapp-bridge")
	homeBridge := filepath.Join(home.Dir(ctx), "scripts", "whatsapp-bridge")
	probe := filepath.Join(installBridge, ".write_test")
	if fh, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o666); err == nil {
		fh.Close()
		if os.Remove(probe) == nil {
			return installBridge
		}
	}
	if _, err := os.Stat(homeBridge); err == nil {
		return homeBridge
	}
	if err := pyfs.Makedirs(filepath.Dir(homeBridge), 0o777, true); err != nil {
		return installBridge
	}
	if err := pyfs.CopyTree(installBridge, homeBridge, false, false, nil); err != nil {
		return installBridge
	}
	return homeBridge
}

// Values a string require mention setting counts as on
var whatsappTruthy = map[string]bool{"true": true, "1": true, "yes": true, "on": true}

// Practical reply length limit, not the protocol maximum
const whatsappMaxMessageLength = 4096

// Reply prefix of self chat replies when neither config nor env sets one
const whatsappDefaultReplyPrefix = "☤ *Icebath Agent*\n────────────\n"

var (
	whatsappDeviceSuffix   = pyre.MustCompile(`:\d+(?=@)`)
	whatsappFence          = pyre.MustCompile("```[\\s\\S]*?```")
	whatsappInlineCode     = pyre.MustCompile("`[^`\\n]+`")
	whatsappItalic         = pyre.MustCompile(`(?<!\*)\*(?!\s|\*)([^*\n]*?\S[^*\n]*?)\*(?!\*)`)
	whatsappBold           = pyre.MustCompile(`\*\*(.+?)\*\*`)
	whatsappUnderlineBold  = pyre.MustCompile(`__(.+?)__`)
	whatsappStrike         = pyre.MustCompile(`~~(.+?)~~`)
	whatsappHeader         = pyre.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	whatsappLink           = pyre.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`)
	whatsappMissingDefault = "\x00unset"
)

// Text with every regexp match replaced by the function of that match
func whatsappSub(re *regexp2.Regexp, text string, fn func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(text, fn, -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Text of capture group n of a match
func whatsappGroup(m regexp2.Match, n int) string { return m.GroupByNumber(n).String() }

// Every pattern match swapped for a NUL tagged marker, the originals in order
func whatsappStash(re *regexp2.Regexp, text, tag string) (string, []string) {
	var saved []string
	out := whatsappSub(re, text, func(m regexp2.Match) string {
		saved = append(saved, m.String())
		return fmt.Sprintf("\x00%s%d\x00", tag, len(saved)-1)
	})
	return out, saved
}

// Header text as bold, already bolded asterisks stripped so the title shows no literal stars
func whatsappHeaderToBold(m regexp2.Match) string {
	inner := pystr.Strip(whatsappGroup(m, 1))
	for pystr.Len(inner) > 1 && strings.HasPrefix(inner, "*") && strings.HasSuffix(inner, "*") {
		inner = pystr.Strip(pystr.Slice(inner, 1, pystr.Len(inner)-1))
	}
	return "*" + inner + "*"
}

// WhatsApp behaviour shared by the bridge and Cloud API adapters, gating, mentions and formatting
type WhatsAppBehaviorMixin struct {
	*gateway.OwnAccessPolicyMixin
	Config          *gateway.PlatformConfig
	Name            string
	MentionPatterns []*regexp2.Regexp
	ReplyPrefix     *string

	dmAllowlistSource *string
}

// Mixin over its host config with the WhatsApp allowlist matching and live DM allowlist installed
func NewWhatsAppBehaviorMixin(name string, cfg *gateway.PlatformConfig) *WhatsAppBehaviorMixin {
	policy, err := gateway.NewOwnAccessPolicyMixin(name, "WHATSAPP")
	if err != nil {
		panic(err)
	}
	b := &WhatsAppBehaviorMixin{OwnAccessPolicyMixin: policy, Config: cfg, Name: name}
	policy.EntryMatches = b.EntryMatches
	policy.LiveDMAllowFrom = b.LiveDMAllowFrom
	return b
}

// WhatsApp allowlist match of the target across its phone and LID forms
func (b *WhatsAppBehaviorMixin) EntryMatches(ctx context.Context, entries map[string]bool, target string) (bool, error) {
	return MatchesWhatsAppAllowlist(ctx, target, entries)
}

// Config extras of the host, empty when it has none
func (b *WhatsAppBehaviorMixin) extra() map[string]any {
	if b.Config == nil || b.Config.Extra == nil {
		return map[string]any{}
	}
	return b.Config.Extra
}

// Scoped secret read, false when the profile has no such key
func whatsappSecret(ctx context.Context, name string) (string, bool, error) {
	value, err := gateway.GetScopedSecret(ctx, name, whatsappMissingDefault, false)
	if err != nil || value == whatsappMissingDefault {
		return "", false, err
	}
	return value, true, nil
}

// Drops zero width format characters and normalises odd unicode spaces WhatsApp renders as mojibake
func SanitizeOutboundText(content string) string {
	if content == "" {
		return content
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\u200b' || r == '\u2060' || r == '\u2063' || r == '\ufeff':
			return -1
		case r == '\u00a0' || r == '\u1680' || r == '\u180e' || (r >= '\u2000' && r <= '\u200a') || r == '\u202f' || r == '\u205f' || r == '\u3000':
			return ' '
		}
		return r
	}, content)
}

// Prefix of outgoing self chat replies, empty outside self chat mode
func (b *WhatsAppBehaviorMixin) EffectiveReplyPrefix(ctx context.Context) (string, error) {
	mode, _, err := whatsappSecret(ctx, "WHATSAPP_MODE")
	if err != nil {
		return "", err
	}
	if mode == "" {
		mode = "self-chat"
	}
	if mode != "self-chat" {
		return "", nil
	}
	if b.ReplyPrefix != nil {
		return strings.ReplaceAll(*b.ReplyPrefix, `\n`, "\n"), nil
	}
	envPrefix, ok, err := whatsappSecret(ctx, "WHATSAPP_REPLY_PREFIX")
	if err != nil || ok {
		return strings.ReplaceAll(envPrefix, `\n`, "\n"), err
	}
	return whatsappDefaultReplyPrefix, nil
}

// Chunk budget with room reserved for the reply prefix, floored for pagination and fence repair
func (b *WhatsAppBehaviorMixin) OutgoingChunkLimit(ctx context.Context) (int, error) {
	prefix, err := b.EffectiveReplyPrefix(ctx)
	return max(1024, whatsappMaxMessageLength-pystr.Len(prefix)), err
}

// Whether group messages need a mention, config first then the scoped env
func (b *WhatsAppBehaviorMixin) WhatsAppRequireMention(ctx context.Context) (bool, error) {
	configured, ok := b.extra()["require_mention"]
	if !ok || configured == nil {
		value, _, err := whatsappSecret(ctx, "WHATSAPP_REQUIRE_MENTION")
		if err != nil {
			return false, err
		}
		if value == "" {
			value = "false"
		}
		configured = value
	}
	if s, isStr := configured.(string); isStr {
		return whatsappTruthy[pystr.Lower(s)], nil
	}
	return pyrepr.Truthy(configured), nil
}

// Chats answered without a mention, the config extra unless blank then the scoped env list
func (b *WhatsAppBehaviorMixin) WhatsAppFreeResponseChats(ctx context.Context) (map[string]bool, error) {
	raw, err := gateway.ExtraOrSecret(ctx, b.extra(), "free_response_chats", "WHATSAPP_FREE_RESPONSE_CHATS", "", true)
	if err != nil {
		return nil, err
	}
	return CoerceAllowList(raw), nil
}

// Allowlist of config, a list or JSON list string, or an env CSV as a set of stripped entries
func CoerceAllowList(raw any) map[string]bool {
	out := map[string]bool{}
	if raw == nil {
		return out
	}
	raw = gateway.DecodeJSONListLiteral(raw)
	var parts []string
	if list, ok := raw.([]any); ok {
		for _, part := range list {
			parts = append(parts, pyrepr.Str(part))
		}
	} else {
		parts = strings.Split(pyrepr.Str(raw), ",")
	}
	for _, part := range parts {
		if part = pystr.Strip(part); part != "" {
			out[part] = true
		}
	}
	return out
}

// Source and raw allowlist by key presence, a config key winning then the first set env carrier
func SelectAllowlist(extra map[string]any, configKeys, envKeys []string, readEnv func(name string) (string, error)) (*string, any, error) {
	for _, key := range configKeys {
		if value, ok := extra[key]; ok {
			source := "config"
			return &source, value, nil
		}
	}
	for _, env := range envKeys {
		raw, err := readEnv(env)
		if err != nil {
			return nil, nil, err
		}
		if raw != "" {
			source := env
			return &source, raw, nil
		}
	}
	return nil, nil, nil
}

// Raw DM allowlist, the winning source recorded so live DM checks keep the same precedence
func (b *WhatsAppBehaviorMixin) SelectDMAllowlist(extra map[string]any, envKeys []string, readEnv func(name string) (string, error)) (any, error) {
	source, raw, err := SelectAllowlist(extra, []string{"allow_from", "allowFrom"}, envKeys, readEnv)
	if err != nil {
		return nil, err
	}
	b.dmAllowlistSource = source
	return raw, nil
}

// Allowlist enforced for DM intake now, an env seeded one re-read so pairing changes apply without restart
func (b *WhatsAppBehaviorMixin) LiveDMAllowFrom(ctx context.Context) (map[string]bool, error) {
	if source := b.dmAllowlistSource; source != nil && *source != "config" {
		live, ok, err := whatsappSecret(ctx, *source)
		if err != nil || !ok {
			return map[string]bool{}, err
		}
		return CoerceAllowList(live), nil
	}
	out := map[string]bool{}
	for entry := range b.AllowFrom {
		out[entry] = true
	}
	return out, nil
}

// WhatsApp id with its device qualifier dropped so it equals the bare form
func NormalizeWhatsAppID(value any) string {
	if !pyrepr.Truthy(value) {
		return ""
	}
	out, err := whatsappDeviceSuffix.Replace(pystr.Strip(pyrepr.Str(value)), "", -1, -1)
	if err != nil {
		panic(err)
	}
	return out
}

// Status updates and channel broadcasts, chats never replied to
func IsBroadcastChat(chatID string) bool {
	cid := pystr.Lower(pystr.Strip(chatID))
	return cid == "status@broadcast" || strings.HasSuffix(cid, "@broadcast") || strings.HasSuffix(cid, "@newsletter")
}

// Whether the identifier matches the allowlist across its phone and LID forms
func MatchesWhatsAppAllowlist(ctx context.Context, candidate string, allowFrom map[string]bool) (bool, error) {
	if len(allowFrom) == 0 {
		return false, nil
	}
	if allowFrom[candidate] {
		return true, nil
	}
	candidateAliases, err := gateway.ExpandWhatsAppAliases(ctx, candidate)
	if err != nil || len(candidateAliases) == 0 {
		return false, err
	}
	for entry := range allowFrom {
		if entry == "*" || candidateAliases[gateway.NormalizeWhatsAppIdentifier(entry)] {
			return true, nil
		}
		aliases, err := gateway.ExpandWhatsAppAliases(ctx, entry)
		if err != nil {
			return false, err
		}
		for alias := range aliases {
			if candidateAliases[alias] {
				return true, nil
			}
		}
	}
	return false, nil
}

// Mention patterns of the config extra or the scoped env, invalid ones logged and skipped
func (b *WhatsAppBehaviorMixin) CompileMentionPatterns(ctx context.Context) ([]*regexp2.Regexp, error) {
	patterns, configured := b.extra()["mention_patterns"]
	if !configured || patterns == nil {
		patterns = nil
		raw, _, err := whatsappSecret(ctx, "WHATSAPP_MENTION_PATTERNS")
		if err != nil {
			return nil, err
		}
		if raw = pystr.Strip(raw); raw != "" {
			if decoded, err := pyjson.Loads(raw); err == nil {
				patterns = pyjson.Plain(decoded)
			} else {
				var lines []any
				for _, p := range pystr.Splitlines(raw, false) {
					if p = pystr.Strip(p); p != "" {
						lines = append(lines, p)
					}
				}
				if len(lines) == 0 {
					for _, p := range strings.Split(raw, ",") {
						if p = pystr.Strip(p); p != "" {
							lines = append(lines, p)
						}
					}
				}
				patterns = lines
			}
		}
	}
	if patterns == nil {
		return []*regexp2.Regexp{}, nil
	}
	if s, ok := patterns.(string); ok {
		patterns = []any{s}
	}
	list, ok := patterns.([]any)
	if !ok {
		slog.Warn(fmt.Sprintf("[%s] whatsapp mention_patterns must be a list or string; got %s", b.Name, pyrepr.TypeName(patterns)))
		return []*regexp2.Regexp{}, nil
	}
	compiled := []*regexp2.Regexp{}
	for _, pattern := range list {
		text, ok := pattern.(string)
		if !ok || pystr.Strip(text) == "" {
			continue
		}
		re, err := pyre.CompileIgnoreCase(text)
		if err != nil {
			slog.Warn(fmt.Sprintf("[%s] Invalid WhatsApp mention pattern %s: %s", b.Name, pyrepr.Repr(text), err))
			continue
		}
		compiled = append(compiled, re)
	}
	if len(compiled) > 0 {
		slog.Info(fmt.Sprintf("[%s] Loaded %d WhatsApp mention pattern(s)", b.Name, len(compiled)))
	}
	return compiled, nil
}

// Normalised bot ids the bridge reported on the message
func BotIDsFromMessage(data map[string]any) map[string]bool {
	out := map[string]bool{}
	ids, _ := data["botIds"].([]any)
	for _, c := range ids {
		if nid := NormalizeWhatsAppID(c); nid != "" {
			out[nid] = true
		}
	}
	return out
}

// Whether the message quotes one of the bot's own messages
func MessageIsReplyToBot(data map[string]any) bool {
	quoted := NormalizeWhatsAppID(data["quotedParticipant"])
	return quoted != "" && BotIDsFromMessage(data)[quoted]
}

// Whether the message mentions the bot by id or by its bare number in the body
func MessageMentionsBot(data map[string]any) bool {
	botIDs := BotIDsFromMessage(data)
	if len(botIDs) == 0 {
		return false
	}
	mentioned, _ := data["mentionedIds"].([]any)
	for _, c := range mentioned {
		if nid := NormalizeWhatsAppID(c); nid != "" && botIDs[nid] {
			return true
		}
	}
	body := ""
	if pyrepr.Truthy(data["body"]) {
		body = pyrepr.Str(data["body"])
	}
	lowerBody := pystr.Lower(body)
	for botID := range botIDs {
		bare, _, _ := strings.Cut(botID, "@")
		if bare = pystr.Lower(bare); bare != "" && (strings.Contains(lowerBody, "@"+bare) || strings.Contains(lowerBody, bare)) {
			return true
		}
	}
	return false
}

// Whether a configured mention pattern matches the message body
func (b *WhatsAppBehaviorMixin) MessageMatchesMentionPatterns(data map[string]any) bool {
	body := ""
	if pyrepr.Truthy(data["body"]) {
		body = pyrepr.Str(data["body"])
	}
	for _, pattern := range b.MentionPatterns {
		if m, err := pattern.FindStringMatch(body); err == nil && m != nil {
			return true
		}
	}
	return false
}

// Text with the bot's own at mentions removed, the original when nothing else remains
func CleanBotMentionText(text string, data map[string]any) string {
	if text == "" {
		return text
	}
	cleaned := text
	for botID := range BotIDsFromMessage(data) {
		bare, _, _ := strings.Cut(botID, "@")
		if bare == "" {
			continue
		}
		re := pyre.MustCompile(`@` + regexp2.Escape(bare) + `\b[,:\-]*\s*`)
		out, err := re.Replace(cleaned, "", -1, -1)
		if err != nil {
			panic(err)
		}
		cleaned = out
	}
	if stripped := pystr.Strip(cleaned); stripped != "" {
		return stripped
	}
	return text
}

// Whether an inbound message is processed, broadcasts never and groups by policy and mention
func (b *WhatsAppBehaviorMixin) ShouldProcessMessage(ctx context.Context, data map[string]any) (bool, error) {
	chatID := ""
	if pyrepr.Truthy(data["chatId"]) {
		chatID = pyrepr.Str(data["chatId"])
	}
	if IsBroadcastChat(chatID) {
		return false, nil
	}
	if !pyrepr.Truthy(data["isGroup"]) {
		sender := ""
		if pyrepr.Truthy(data["senderId"]) {
			sender = pyrepr.Str(data["senderId"])
		} else if pyrepr.Truthy(data["from"]) {
			sender = pyrepr.Str(data["from"])
		}
		return b.IsDMIntakeAllowed(ctx, sender)
	}
	if allowed, err := b.IsGroupAllowed(ctx, chatID, ""); err != nil || !allowed {
		return false, err
	}
	free, err := b.WhatsAppFreeResponseChats(ctx)
	if err != nil {
		return false, err
	}
	requireMention, err := b.WhatsAppRequireMention(ctx)
	if err != nil {
		return false, err
	}
	if free[chatID] || !requireMention {
		return true, nil
	}
	body := ""
	if pyrepr.Truthy(data["body"]) {
		body = pyrepr.Str(data["body"])
	}
	return strings.HasPrefix(pystr.Strip(body), "/") || MessageIsReplyToBot(data) || MessageMentionsBot(data) ||
		b.MessageMatchesMentionPatterns(data), nil
}

// Markdown converted to WhatsApp syntax, fenced and inline code protected behind NUL markers
func FormatMessage(content string) string {
	if content == "" {
		return content
	}
	result, fences := whatsappStash(whatsappFence, SanitizeOutboundText(content), "FENCE")
	result, codes := whatsappStash(whatsappInlineCode, result, "CODE")
	group1 := func(wrap string) func(m regexp2.Match) string {
		return func(m regexp2.Match) string { return wrap + whatsappGroup(m, 1) + wrap }
	}
	result = whatsappSub(whatsappItalic, result, group1("_"))
	result = whatsappSub(whatsappBold, result, group1("*"))
	result = whatsappSub(whatsappUnderlineBold, result, group1("*"))
	result = whatsappSub(whatsappStrike, result, group1("~"))
	result = whatsappSub(whatsappHeader, result, whatsappHeaderToBold)
	result = whatsappSub(whatsappLink, result, func(m regexp2.Match) string {
		return whatsappGroup(m, 1) + " (" + whatsappGroup(m, 2) + ")"
	})
	for _, stash := range []struct {
		tag   string
		saved []string
	}{{"FENCE", fences}, {"CODE", codes}} {
		for i, original := range stash.saved {
			result = strings.ReplaceAll(result, fmt.Sprintf("\x00%s%d\x00", stash.tag, i), original)
		}
	}
	return result
}
