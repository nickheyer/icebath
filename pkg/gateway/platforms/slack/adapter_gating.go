package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mediacache"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Audio MIME type of each cached voice clip extension
var slackExtToAudioMime = map[string]string{
	".mp4": "audio/mp4", ".m4a": "audio/mp4", ".mp3": "audio/mpeg", ".mpeg": "audio/mpeg",
	".mpga": "audio/mpeg", ".wav": "audio/wav", ".webm": "audio/webm", ".ogg": "audio/ogg",
	".aac": "audio/aac", ".flac": "audio/flac"}

// Method factory reading one opt in flag through extraOrEnvFlag
func extraOrEnvFlagGetter(key, envVar string, strip bool) func(a *SlackAdapter, ctx context.Context) bool {
	return func(a *SlackAdapter, ctx context.Context) bool { return a.extraOrEnvFlag(ctx, key, envVar, strip) }
}

// Method factory reading one channel id set through extraOrEnvChannelSet
func extraOrEnvChannelSetGetter(key, envVar string, coerceScalar bool) func(a *SlackAdapter, ctx context.Context) map[string]bool {
	return func(a *SlackAdapter, ctx context.Context) map[string]bool {
		return a.extraOrEnvChannelSet(ctx, key, envVar, coerceScalar)
	}
}

var (
	slackThreadFollowupSilenceGetter   = extraOrEnvFlagGetter("thread_followup_silence", "SLACK_THREAD_FOLLOWUP_SILENCE", false)
	slackStrictMentionGetter           = extraOrEnvFlagGetter("strict_mention", "SLACK_STRICT_MENTION", false)
	slackIgnoreOtherUserMentionsGetter = extraOrEnvFlagGetter("ignore_other_user_mentions", "SLACK_IGNORE_OTHER_USER_MENTIONS", false)
	slackThreadRequireMentionGetter    = extraOrEnvFlagGetter("thread_require_mention", "SLACK_THREAD_REQUIRE_MENTION", false)
	slackDisableDMsGetter              = extraOrEnvFlagGetter("disable_dms", "SLACK_DISABLE_DMS", true)
	slackFreeResponseChannelsGetter    = extraOrEnvChannelSetGetter("free_response_channels", "SLACK_FREE_RESPONSE_CHANNELS", true)
	slackAllowedChannelsGetter         = extraOrEnvChannelSetGetter("allowed_channels", "SLACK_ALLOWED_CHANNELS", false)
	slackRequireMentionChannelsGetter  = extraOrEnvChannelSetGetter("require_mention_channels", "SLACK_REQUIRE_MENTION_CHANNELS", false)
	slackIgnoredChannelsGetter         = extraOrEnvChannelSetGetter("ignored_channels", "SLACK_IGNORED_CHANNELS", true)
)

// Whether unaddressed thread follow ups may stay silent
func (a *SlackAdapter) slackThreadFollowupSilence(ctx context.Context) bool {
	return slackThreadFollowupSilenceGetter(a, ctx)
}

// Whether the bot ignores channels until mentioned again
func (a *SlackAdapter) slackStrictMention(ctx context.Context) bool {
	return slackStrictMentionGetter(a, ctx)
}

// Whether messages opening with another user's mention are ignored
func (a *SlackAdapter) slackIgnoreOtherUserMentions(ctx context.Context) bool {
	return slackIgnoreOtherUserMentionsGetter(a, ctx)
}

// Whether thread replies need an explicit mention
func (a *SlackAdapter) slackThreadRequireMention(ctx context.Context) bool {
	return slackThreadRequireMentionGetter(a, ctx)
}

// Whether Slack DMs are refused
func (a *SlackAdapter) slackDisableDMs(ctx context.Context) bool {
	return slackDisableDMsGetter(a, ctx)
}

// Channels answered without a mention
func (a *SlackAdapter) slackFreeResponseChannels(ctx context.Context) map[string]bool {
	return slackFreeResponseChannelsGetter(a, ctx)
}

// Channels the bot may answer in, every channel when empty
func (a *SlackAdapter) slackAllowedChannels(ctx context.Context) map[string]bool {
	return slackAllowedChannelsGetter(a, ctx)
}

// Channels that always need a mention even when mentions are otherwise optional
func (a *SlackAdapter) slackRequireMentionChannels(ctx context.Context) map[string]bool {
	return slackRequireMentionChannelsGetter(a, ctx)
}

var addressedUserRe = mustPyRe(`\A\s*<@([^>|\s]+)(?:\|[^>]*)?>`)

// Whether the text opens with a user mention of someone other than the bot
func slackMessageAddressedToOtherUser(text string, selfUIDs map[string]bool) bool {
	if text == "" {
		return false
	}
	m, ok := search(addressedUserRe, text)
	return ok && !selfUIDs[m.groups[1]]
}

// Whether the channel admits messages without a mention
func (a *SlackAdapter) slackIsFreeChannel(ctx context.Context, channelID string) bool {
	return !a.slackRequireMentionChannels(ctx)[channelID] &&
		(a.slackFreeResponseChannels(ctx)[channelID] || !a.slackRequireMention(ctx))
}

// Reply expectation of an admitted message, false when silence may stand and nil to keep the fallback
func (a *SlackAdapter) slackReplyExpected(ctx context.Context, routingText, botUID, channelID string, addressed, opensOwnSession, channelThreadFollowup bool) *bool {
	yes, no := true, false
	selfUIDs := map[string]bool{}
	for _, u := range []string{botUID, a.primaryBotUID()} {
		if u != "" {
			selfUIDs[u] = true
		}
	}
	if addressed || slackMessageMentionsSelf(routingText, selfUIDs) {
		return &yes
	}
	if slackMessageAddressedToOtherUser(routingText, selfUIDs) {
		return &no
	}
	if channelThreadFollowup && a.slackThreadFollowupSilence(ctx) {
		return &no
	}
	if opensOwnSession && a.slackIsFreeChannel(ctx, channelID) {
		return &no
	}
	return nil
}

// Whether the text mentions this bot anywhere in plain or labelled form
func slackMessageMentionsSelf(text string, selfUIDs map[string]bool) bool {
	if text == "" {
		return false
	}
	for uid := range selfUIDs {
		re := mustPyRe("<@" + regexp.QuoteMeta(uid) + `(?:\|[^>]*)?>`)
		if _, ok := search(re, text); ok {
			return true
		}
	}
	return false
}

// Wake word patterns from extra mention_patterns or SLACK_MENTION_PATTERNS, compiled once
func (a *SlackAdapter) slackMentionPatterns(ctx context.Context) []*regexp2.Regexp {
	a.mu.Lock()
	if a.mentionPatternsLoaded {
		cached := a.compiledMentionPatterns
		a.mu.Unlock()
		return cached
	}
	a.mu.Unlock()
	var patterns any
	if extra := a.extra(); len(extra) > 0 {
		patterns = extra["mention_patterns"]
	}
	if patterns == nil {
		secret, _ := gateway.GetScopedSecret(ctx, "SLACK_MENTION_PATTERNS", "", false)
		if raw := pystr.Strip(secret); raw != "" {
			var decoded any
			if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
				patterns = decoded
			} else {
				var parts []any
				for _, p := range strings.Split(strings.ReplaceAll(raw, "\n", ","), ",") {
					if p = pystr.Strip(p); p != "" {
						parts = append(parts, p)
					}
				}
				patterns = parts
			}
		}
	}
	if s, ok := patterns.(string); ok {
		patterns = []any{s}
	}
	var compiled []*regexp2.Regexp
	if list, ok := patterns.([]any); ok {
		for _, raw := range list {
			pat, ok := raw.(string)
			if !ok || pystr.Strip(pat) == "" {
				continue
			}
			re, err := compileIgnoreCase(pat)
			if err != nil {
				slog.Warn(fmt.Sprintf("[Slack] Invalid mention pattern %s: %v", pyrepr.Repr(pat), err))
				continue
			}
			compiled = append(compiled, re)
		}
	} else if patterns != nil {
		slog.Warn(fmt.Sprintf("[Slack] mention_patterns must be a list or string; got %s", pyrepr.TypeName(patterns)))
	}
	if len(compiled) > 0 {
		slog.Info(fmt.Sprintf("[Slack] Loaded %d mention pattern(s)", len(compiled)))
	}
	a.mu.Lock()
	a.compiledMentionPatterns, a.mentionPatternsLoaded = compiled, true
	a.mu.Unlock()
	return compiled
}

// Python pattern compiled with re.IGNORECASE semantics
func compileIgnoreCase(pattern string) (*regexp2.Regexp, error) {
	translated, err := pyre.Translate(pattern)
	if err != nil {
		return nil, err
	}
	return regexp2.Compile(translated, regexp2.IgnoreCase)
}

// Whether the text matches a configured wake word pattern
func (a *SlackAdapter) slackMessageMatchesMentionPatterns(ctx context.Context, text string) bool {
	if text == "" {
		return false
	}
	for _, re := range a.slackMentionPatterns(ctx) {
		if ok, _ := re.MatchString(text); ok {
			return true
		}
	}
	return false
}

// Whether the URL is https on a Slack CDN host
func isSlackCDNURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.TrimRight(strings.ToLower(parsed.Hostname()), ".")
	if host == "" || parsed.Scheme != "https" {
		return false
	}
	return host == "slack.com" || host == "slack-files.com" || strings.HasSuffix(host, ".slack.com") || strings.HasSuffix(host, ".slack-files.com")
}

var filesPriTeamRe = mustPyRe(`/files-pri/(T[A-Z0-9]+)-`)

// Download token of the explicit team, else the team in the files-pri path, else the primary token
func (a *SlackAdapter) resolveDownloadToken(rawURL, teamID string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if teamID != "" {
		if client, ok := a.teamClients.Get(teamID); ok {
			return client.token
		}
	}
	if m, ok := search(filesPriTeamRe, rawURL); ok {
		if client, ok := a.teamClients.Get(m.groups[1]); ok {
			return client.token
		}
	}
	return strOrEmpty(a.Config.Token)
}

// SSRF safe client for Slack file downloads, a package var the way hermes tests patch the client factory
var newDownloadClient = func(env *tools.Env, timeout time.Duration) *http.Client {
	return tools.NewSSRFSafeClient(env, timeout, false)
}

// Whether a download failure is a timeout worth retrying
func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Slack file bytes fetched with the bot token, every hop CDN and SSRF checked, retried on 429, 5xx, timeouts
func (a *SlackAdapter) downloadSlackFileBytes(ctx context.Context, rawURL, teamID, htmlLabel string) ([]byte, error) {
	env := gateway.URLSafetyEnv(ctx)
	if !tools.IsSafeURL(env, rawURL) {
		return nil, &mediacache.ValueError{Msg: "Blocked unsafe Slack file URL (SSRF protection): " + gateway.SafeURLForLog(rawURL, 80)}
	}
	if !isSlackCDNURL(rawURL) {
		return nil, &mediacache.ValueError{Msg: "Blocked non-Slack-CDN file URL (token-exfiltration protection): " + gateway.SafeURLForLog(rawURL, 80)}
	}
	token := a.resolveDownloadToken(rawURL, teamID)
	client := newDownloadClient(env, 30*time.Second)
	defer client.CloseIdleConnections()
	for attempt := 0; ; attempt++ {
		data, err := downloadOnce(ctx, client, env, rawURL, token, htmlLabel)
		if err == nil {
			return data, nil
		}
		var statusErr *httpStatusError
		isStatus := errors.As(err, &statusErr)
		if !isStatus && !isTimeoutError(err) {
			return nil, err
		}
		if isStatus && statusErr.StatusCode < 429 {
			return nil, err
		}
		if attempt < 2 {
			slog.Debug(fmt.Sprintf("Slack file download retry %d/2 for %s: %v", attempt+1, pystr.Slice(rawURL, 0, 80), err))
			if serr := sleepCtx(ctx, time.Duration(1.5*float64(attempt+1)*float64(time.Second))); serr != nil {
				return nil, serr
			}
			continue
		}
		return nil, err
	}
}

// One download attempt following at most three vetted redirects
func downloadOnce(ctx context.Context, client *http.Client, env *tools.Env, rawURL, token, htmlLabel string) ([]byte, error) {
	downloadURL := rawURL
	redirects := 0
	var resp *http.Response
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err = client.Do(req)
		if err != nil {
			return nil, err
		}
		target := gateway.RedirectTargetFromResponse(resp)
		if gerr := gateway.SSRFRedirectGuard(env, target); gerr != nil {
			resp.Body.Close()
			return nil, gerr
		}
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
		default:
			goto done
		}
		if target == "" {
			goto done
		}
		resp.Body.Close()
		if !tools.IsSafeURL(env, target) {
			return nil, &mediacache.ValueError{Msg: "Blocked unsafe Slack file redirect (SSRF protection): " + gateway.SafeURLForLog(target, 80)}
		}
		if !isSlackCDNURL(target) {
			return nil, &mediacache.ValueError{Msg: "Blocked non-Slack-CDN file redirect (token-exfiltration protection): " + gateway.SafeURLForLog(target, 80)}
		}
		if redirects == 3 {
			return nil, &mediacache.ValueError{Msg: "Too many Slack file redirects (maximum 3)"}
		}
		downloadURL = target
		redirects++
	}
done:
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpStatusError{StatusCode: resp.StatusCode, URL: downloadURL, Location: resp.Header.Get("Location")}
	}
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") {
		return nil, &mediacache.ValueError{Msg: fmt.Sprintf("Slack returned HTML instead of %s (content-type: %s); "+
			"check bot token scopes and file permissions, or a cross-origin "+
			"redirect dropped the token (Enterprise Grid files-origin)", htmlLabel, ct)}
	}
	return io.ReadAll(resp.Body)
}

// Downloads a Slack image or audio file and caches it, returning the cached path
func (a *SlackAdapter) downloadSlackFile(ctx context.Context, rawURL, ext string, audio bool, teamID string) (string, error) {
	data, err := a.downloadSlackFileBytes(ctx, rawURL, teamID, "media")
	if err != nil {
		return "", err
	}
	if audio {
		return gateway.CacheAudioFromBytesAsync(ctx, data, ext)
	}
	return gateway.CacheImageFromBytesAsync(ctx, data, ext)
}
