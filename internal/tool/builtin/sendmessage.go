package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/internal/fileutil"
	"github.com/nickheyer/icebath/internal/model/wire"
	"github.com/nickheyer/icebath/internal/tool"
)

type SyntaxVerdict uint8

const (
	SyntaxUnknown SyntaxVerdict = iota
	SyntaxExplicit
	SyntaxNotExplicit
)

type PlatformEntry struct {
	Plugin             bool
	TargetSyntax       func(ref string) (chatID, threadID string, verdict SyntaxVerdict)
	ParseTarget        func(ref string) (chatID, threadID string, parsed bool, err error)
	ValidateTarget     func(chatID string) (valid bool, detail string, err error)
	StandaloneMentions bool
	CustomHandler      bool
}

type Destination struct {
	Platform string
	Home     string
	Handle   any
}

type Media struct {
	Path  string
	Voice bool
}

type Outbound struct {
	ChatID        string
	ThreadID      string
	Message       string
	Media         []Media
	ForceDocument bool
	Mentions      []string
	Args          tool.Args
}

type Reaction struct {
	ChatID    string
	MessageID string
	Emoji     string
	Remove    bool
}

var (
	ErrNoLiveAdapter        = errors.New("no live gateway adapter for the platform")
	ErrReactionsUnsupported = errors.New("platform does not support message reactions")
)

type Sender interface {
	Prepare(ctx context.Context) error
	Platform(ctx context.Context, name string) (PlatformEntry, bool)
	Builtin(name string) bool
	Enumerated(name string) bool
	InstallHint(name string) string
	ResolveChannel(ctx context.Context, platform, ref string) (string, error)
	Directory(ctx context.Context) (json.RawMessage, error)
	Destination(ctx context.Context, platform string) (Destination, string, error)
	HomeChannel(ctx context.Context, platform string) (string, error)
	ExtractMedia(ctx context.Context, message string) ([]Media, string, []json.RawMessage)
	ResolveSlackUser(ctx context.Context, d Destination, target string) (string, []wire.Member)
	AuthorizeRelay(ctx context.Context, platform, chatID, threadID string, d *Destination) string
	Send(ctx context.Context, d Destination, o Outbound) ([]wire.Member, error)
	Mirror(ctx context.Context, platform, chatID, text, threadID, source, userID string) bool
	React(ctx context.Context, platform string, r Reaction) ([]wire.Member, bool, error)
	SanitizeError(text string) string
}

type Messenger struct {
	s Sender
}

func NewMessenger(s Sender) (*Messenger, error) {
	if s == nil {
		return nil, &tool.UnfilledError{Name: "gateway message delivery"}
	}
	return &Messenger{s: s}, nil
}

var phonePlatforms = []string{"photon", "signal", "sms", "whatsapp"}

var homeChannelEnvOverrides = map[string]string{"email": "EMAIL_HOME_ADDRESS"}

var (
	imageExts = []string{".jpg", ".jpeg", ".png", ".webp", ".gif"}
	videoExts = []string{".mp4", ".mov", ".avi", ".mkv", ".webm", ".3gp"}
	audioExts = []string{".ogg", ".opus", ".mp3", ".m2a", ".wav", ".m4a", ".flac"}
	voiceExts = []string{".ogg", ".opus"}
)

func e164(ref string) bool {
	t := strings.TrimFunc(ref, isPySpace)
	if !strings.HasPrefix(t, "+") {
		return false
	}
	digits := t[1:]
	return len(digits) >= 7 && len(digits) <= 15 && isDigits(digits)
}

func (m *Messenger) parseTargetRef(ctx context.Context, platform, ref string) (string, string, bool) {
	entry, registered := m.s.Platform(ctx, platform)
	if registered && entry.TargetSyntax != nil {
		chatID, threadID, verdict := entry.TargetSyntax(ref)
		switch verdict {
		case SyntaxNotExplicit:
			return "", "", false
		case SyntaxExplicit:
			return chatID, threadID, true
		}
	}
	if contains(phonePlatforms, platform) && e164(ref) {
		return strings.TrimFunc(ref, isPySpace), "", true
	}
	if isDigits(strings.TrimLeft(ref, "-")) || platform == "matrix" && (strings.HasPrefix(ref, "!") || strings.HasPrefix(ref, "@")) ||
		platform == "xmpp" && strings.Contains(ref, "@") {
		return ref, "", true
	}
	return "", "", false
}

func (m *Messenger) unknownPlatform(platform string) string {
	return "Unknown or unregistered plugin platform: " + platform + m.s.InstallHint(platform)
}

func (m *Messenger) ResolveTarget(ctx context.Context, platform, ref string, passUnresolved bool) (string, string, error) {
	entry, registered := m.s.Platform(ctx, platform)
	validated := func(chatID, threadID string) (string, string, error) {
		if !registered || entry.ValidateTarget == nil {
			return chatID, threadID, nil
		}
		valid, detail, err := entry.ValidateTarget(chatID)
		if err != nil {
			slog.DebugContext(ctx, "Plugin target validator failed", "platform", platform, "error", err)
			return "", "", fmt.Errorf("Target validator failed for platform '%s'", platform)
		}
		if valid {
			return chatID, threadID, nil
		}
		if detail != "" {
			detail = ": " + detail
		}
		return "", "", fmt.Errorf("Invalid target '%s' on %s%s", ref, platform, detail)
	}
	if registered && entry.ParseTarget != nil {
		chatID, threadID, parsed, err := entry.ParseTarget(ref)
		if err != nil {
			slog.DebugContext(ctx, "Plugin target parser failed", "platform", platform, "error", err)
			return "", "", fmt.Errorf("Target parser failed for platform '%s'", platform)
		}
		if parsed {
			if chatID == "" {
				return "", "", fmt.Errorf("Target parser for platform '%s' returned an invalid result", platform)
			}
			return validated(chatID, threadID)
		}
	}
	if chatID, threadID, explicit := m.parseTargetRef(ctx, platform, ref); explicit {
		return validated(chatID, threadID)
	}
	resolved, err := m.s.ResolveChannel(ctx, platform, ref)
	resolutionFailed := err != nil
	if resolutionFailed {
		resolved = ""
	}
	if resolved != "" {
		chatID, threadID, _ := m.parseTargetRef(ctx, platform, resolved)
		if chatID == "" {
			chatID = resolved
		}
		return validated(chatID, threadID)
	}
	builtin := m.s.Builtin(platform)
	if !registered && !builtin {
		return "", "", errors.New(m.unknownPlatform(platform))
	}
	plugin := registered && entry.Plugin && !builtin
	if passUnresolved && (!plugin || entry.ParseTarget == nil) {
		chatID, threadID, err := validated(ref, "")
		if err == nil {
			slog.DebugContext(ctx, "Handing unresolved target to the adapter unchanged (the adapter validates it)", "target", ref, "platform", platform)
		}
		return chatID, threadID, err
	}
	hint := "Use send_message(action='list') to see available targets."
	switch {
	case plugin:
		hint = "The plugin parser did not recognize it and no channel-directory entry matched."
	case resolutionFailed:
		hint = "Try using a numeric channel ID instead."
	}
	return "", "", fmt.Errorf("Could not resolve '%s' on %s. %s", ref, platform, hint)
}

func (m *Messenger) resolveToolTarget(ctx context.Context, target string, passUnresolved bool) (string, string, string, error, error) {
	platform, ref, _ := strings.Cut(target, ":")
	platform = strings.ToLower(strip(platform))
	ref = strip(ref)
	if err := m.s.Prepare(ctx); err != nil {
		return "", "", "", nil, err
	}
	if ref == "" {
		return platform, "", "", nil, nil
	}
	chatID, threadID, problem := m.ResolveTarget(ctx, platform, ref, passUnresolved)
	return platform, chatID, threadID, problem, nil
}

func asciiJSON(members []wire.Member) (string, error) {
	raw, err := fileutil.EncodeJSON(wire.Object(members), fileutil.JSONStyle{ASCII: true})
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (m *Messenger) errorJSON(message string) (string, error) {
	return asciiJSON([]wire.Member{{Key: "error", Value: wire.Quote(m.s.SanitizeError(message))}})
}

func toolError(msg string) string {
	return tool.Failure(msg).Text
}

func (m *Messenger) Run(ctx context.Context, origin tool.Origin, args tool.Args) (string, error) {
	action := "send"
	if v, present := args["action"]; present {
		action = pyStr(v)
	}
	switch action {
	case "list":
		return m.list(ctx)
	case "react", "unreact":
		return m.react(ctx, args, action == "unreact")
	}
	return m.send(ctx, origin, args)
}

func (m *Messenger) list(ctx context.Context) (string, error) {
	targets, err := m.s.Directory(ctx)
	if err != nil {
		return m.errorJSON("Failed to load channel directory: " + err.Error())
	}
	return asciiJSON([]wire.Member{{Key: "targets", Value: targets}})
}

func stringArg(args tool.Args, key string) string {
	v, present := args[key]
	if !present || v == nil {
		return ""
	}
	return pyStr(v)
}

func (m *Messenger) react(ctx context.Context, args tool.Args, remove bool) (string, error) {
	target := stringArg(args, "target")
	emoji := strip(stringArg(args, "emoji"))
	messageID := strip(stringArg(args, "message_id"))
	if target == "" || !remove && emoji == "" {
		if remove {
			return toolError("'target' is required when action='unreact'"), nil
		}
		return toolError("Both 'target' and 'emoji' are required when action='react'"), nil
	}
	platform, chatID, threadID, problem, err := m.resolveToolTarget(ctx, target, true)
	if err != nil {
		return "", err
	}
	if problem != nil {
		return toolError(problem.Error()), nil
	}
	if !m.s.Enumerated(platform) {
		return toolError("Unknown platform: " + platform), nil
	}
	if chatID == "" {
		home, err := m.s.HomeChannel(ctx, platform)
		if err != nil || home == "" {
			return toolError(fmt.Sprintf("No chat specified and no home channel set for %s. Use '%s:chat_id'.", platform, platform)), nil
		}
		chatID = home
	}
	if denial := m.s.AuthorizeRelay(ctx, platform, chatID, threadID, nil); denial != "" {
		return toolError(denial), nil
	}
	members, success, err := m.s.React(ctx, platform, Reaction{ChatID: chatID, MessageID: messageID, Emoji: emoji, Remove: remove})
	switch {
	case errors.Is(err, ErrNoLiveAdapter):
		return toolError("Reactions require a live " + platform + " adapter in the running gateway (not available from cron/standalone contexts)."), nil
	case errors.Is(err, ErrReactionsUnsupported):
		return toolError("Platform '" + platform + "' does not support message reactions."), nil
	case err != nil:
		return m.errorJSON("Reaction failed: " + err.Error())
	}
	if members != nil {
		return asciiJSON(members)
	}
	return asciiJSON([]wire.Member{{Key: "success", Value: boolJSON(success)}})
}

func boolJSON(b bool) json.RawMessage {
	if b {
		return json.RawMessage("true")
	}
	return json.RawMessage("false")
}

func scrubSurrogates(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

func describeMedia(media []Media) string {
	switch len(media) {
	case 0:
		return ""
	case 1:
	default:
		return fmt.Sprintf("[Sent %d media attachments]", len(media))
	}
	ext := strings.ToLower(filepath.Ext(media[0].Path))
	if media[0].Voice && contains(voiceExts, ext) {
		return "[Sent voice message]"
	}
	kind := "document"
	switch {
	case contains(imageExts, ext):
		kind = "image"
	case contains(videoExts, ext):
		kind = "video"
	case contains(audioExts, ext):
		kind = "audio"
	}
	return "[Sent " + kind + " attachment]"
}

func cronDuplicate(origin tool.Origin, platform, chatID, threadID string, threadSet bool) []wire.Member {
	autoPlatform := strings.ToLower(strip(origin.CronDeliver.Platform))
	autoChat := strip(origin.CronDeliver.ChatID)
	autoThread := strip(origin.CronDeliver.ThreadID)
	if autoPlatform == "" || autoChat == "" || autoPlatform != platform || autoChat != chatID {
		return nil
	}
	if (autoThread != "") != threadSet || autoThread != threadID {
		return nil
	}
	label := platform + ":" + chatID
	if threadSet {
		label += ":" + threadID
	}
	return []wire.Member{
		{Key: "success", Value: boolJSON(true)},
		{Key: "skipped", Value: boolJSON(true)},
		{Key: "reason", Value: wire.Quote("cron_auto_delivery_duplicate_target")},
		{Key: "target", Value: wire.Quote(label)},
		{Key: "note", Value: wire.Quote("Skipped send_message to " + label + ". This cron job will already auto-deliver " +
			"its final response to that same target. Put the intended user-facing content in " +
			"your final response instead, or use a different target if you want an additional message.")},
	}
}

func member(members []wire.Member, key string) (json.RawMessage, bool) {
	for _, mb := range members {
		if mb.Key == key {
			return mb.Value, true
		}
	}
	return nil, false
}

func setMember(members []wire.Member, key string, value json.RawMessage) []wire.Member {
	for i := range members {
		if members[i].Key == key {
			members[i].Value = value
			return members
		}
	}
	return append(members, wire.Member{Key: key, Value: value})
}

func rawTruthy(raw json.RawMessage) bool {
	var v any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return false
	}
	return truthy(v)
}

func (m *Messenger) send(ctx context.Context, origin tool.Origin, args tool.Args) (string, error) {
	target, message := stringArg(args, "target"), stringArg(args, "message")
	if !truthy(args["target"]) || !truthy(args["message"]) {
		return toolError("Both 'target' and 'message' are required when action='send'"), nil
	}
	message = scrubSurrogates(message)
	platform, chatID, threadID, problem, err := m.resolveToolTarget(ctx, target, false)
	if err != nil {
		return "", err
	}
	if problem != nil {
		return toolError(problem.Error()), nil
	}
	if ctx.Err() != nil {
		return toolError("Interrupted"), nil
	}
	dest, refusal, err := m.s.Destination(ctx, platform)
	if err != nil {
		return m.errorJSON("Failed to load gateway config: " + err.Error())
	}
	if refusal != "" {
		return toolError(refusal), nil
	}
	entry, registered := m.s.Platform(ctx, platform)
	force := strings.Contains(message, "[[as_document]]")
	media, cleaned, dropped := m.s.ExtractMedia(ctx, message)
	mirrorText := strip(cleaned)
	if mirrorText == "" {
		mirrorText = describeMedia(media)
	}
	usedHome := chatID == ""
	if usedHome {
		if dest.Home == "" {
			env, ok := homeChannelEnvOverrides[platform]
			if !ok {
				env = strings.ToUpper(platform) + "_HOME_CHANNEL"
			}
			return toolError(fmt.Sprintf("No home channel set for %s to determine where to send the message. "+
				"Either specify a channel directly with '%s:CHANNEL_NAME', "+
				"or set a home channel via: icebath config set %s <channel_id>", platform, platform, env)), nil
		}
		chatID = dest.Home
	}
	threadSet := threadID != ""
	if skip := cronDuplicate(origin, platform, chatID, threadID, threadSet); skip != nil {
		return asciiJSON(skip)
	}
	if platform == "slack" && chatID != "" {
		dm := chatID
		if t := strip(chatID); strings.HasPrefix(chatID, "U") && slackUserID(t) {
			dm = "user:" + chatID
		}
		if strings.HasPrefix(dm, "user:") || strings.HasPrefix(dm, "user_name:") {
			resolved, failure := m.s.ResolveSlackUser(ctx, dest, dm)
			if failure != nil {
				return asciiJSON(failure)
			}
			chatID = resolved
		}
	}
	if denial := m.s.AuthorizeRelay(ctx, platform, chatID, threadID, &dest); denial != "" {
		return toolError(denial), nil
	}
	out := Outbound{ChatID: chatID, ThreadID: threadID, Message: cleaned, Media: media, ForceDocument: force}
	if registered && entry.CustomHandler {
		out.Args = args
	}
	if mentions := args["mentions"]; truthy(mentions) && registered && entry.StandaloneMentions {
		switch t := mentions.(type) {
		case string:
			out.Mentions = []string{t}
		case []any:
			for _, v := range t {
				out.Mentions = append(out.Mentions, pyStr(v))
			}
		}
	}
	result, err := m.s.Send(ctx, dest, out)
	if err != nil {
		return m.errorJSON("Send failed: " + err.Error())
	}
	if v, ok := member(result, "success"); ok && rawTruthy(v) {
		if usedHome {
			result = setMember(result, "note", wire.Quote(fmt.Sprintf("Sent to %s home channel (chat_id: %s)", platform, chatID)))
		}
		if mirrorText != "" && m.s.Mirror(ctx, platform, chatID, mirrorText, threadID, mirrorSource(origin), origin.UserID) {
			result = setMember(result, "mirrored", boolJSON(true))
		}
		if len(dropped) > 0 {
			result = setMember(result, "success", boolJSON(false))
			result = setMember(result, "partial_success", boolJSON(true))
			result = setMember(result, "error", wire.Quote(fmt.Sprintf("Delivery incomplete: %d requested MEDIA attachment(s) "+
				"dropped before delivery (see media_dropped)", len(dropped))))
		}
	}
	if len(dropped) > 0 {
		list, err := json.Marshal(dropped)
		if err != nil {
			return "", fmt.Errorf("media_dropped: %w", err)
		}
		result = setMember(result, "media_dropped", list)
	}
	if v, ok := member(result, "error"); ok {
		var text any
		if err := json.Unmarshal(v, &text); err != nil {
			return "", fmt.Errorf("send result error: %w", err)
		}
		result = setMember(result, "error", wire.Quote(m.s.SanitizeError(pyStr(text))))
	}
	if result == nil {
		result = []wire.Member{}
	}
	return asciiJSON(result)
}

func mirrorSource(origin tool.Origin) string {
	if origin.Platform != "" {
		return origin.Platform
	}
	return "cli"
}

func slackUserID(t string) bool {
	if len(t) < 9 || t[0] != 'U' {
		return false
	}
	for i := 1; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
