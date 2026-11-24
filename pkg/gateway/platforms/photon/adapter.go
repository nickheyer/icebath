package photon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dlclark/regexp2/v2"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/netpref"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

const (
	defaultSidecarPort         = 8789
	defaultSidecarBind         = "127.0.0.1"
	maxMessageLength           = 8000
	runtimeRecordName          = "photon-sidecar.json"
	dedupMaxSize               = 4000
	dedupWindowSeconds         = 48 * 3600
	fffcWait                   = 15 * time.Second
	npmReinstallTimeout        = 600 * time.Second
	richlinkPreviewSuppress    = 30 * time.Second
	richlinkPreviewSuffix      = ".pluginpayloadattachment"
	typingCooldown             = 5 * time.Second
	sentIDsMax                 = 1000
	lastInboundChatsMax        = 200
	sidecarTokenHeader         = "X-Icebath-Sidecar-Token"
	sidecarHealthInterval      = 15 * time.Second
	targetNotAllowedMessage    = "shared/free-tier Photon lines cannot initiate outbound sends to new targets — upgrade to a dedicated line or use another delivery channel"
	sidecarReadyTimeout        = 15 * time.Second
	sidecarCallTimeout         = 30 * time.Second
	defaultProbeIntervalSecond = 600.0
	defaultProbeTimeoutSecond  = 10.0
	defaultProbeMaxFailures    = 3
)

// Photon, Envoy and spectrum-ts wording meaning transient upstream overload
var photonRetryablePatterns = []string{"internal sidecar error", "upstream connect error", "upstream unavailable", "connection dropped",
	"reset reason: overflow", "upstream_overflow", "upstream_unavailable"}

// Group chat wake words when none are configured
var defaultMentionPatterns = []string{`(?<![\w@])@?icebath\s+agent\b[,:\-]?`, `(?<![\w@])@?icebath\b[,:\-]?`}

var (
	sidecarURLRE  = regexp.MustCompile(`(?i)https?://[^\s)'"<>]+`)
	urlOnlyRE     = regexp.MustCompile(`(?i)^https?://\S+$`)
	dmChatGUIDRE  = regexp.MustCompile(`^any;-;(\+\d{6,})$`)
	sendDMGUIDPat = `^any;-;\+\d{6,}$`
)

// Image and audio extensions by MIME for cached inbound bytes
var (
	imageExtByMIME = map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
		"image/heic": ".jpg", "image/heif": ".jpg", "image/tiff": ".jpg"}
	audioExtByMIME = map[string]string{"audio/mp3": ".mp3", "audio/mpeg": ".mp3", "audio/ogg": ".ogg", "audio/wav": ".wav",
		"audio/x-caf": ".caf", "audio/mp4": ".m4a", "audio/aac": ".m4a"}
)

// Runtime record of the live sidecar at the top of the profile home
func runtimeRecordPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), runtimeRecordName)
}

// Record path earlier releases used under runtime, never read because the agent can write there
func legacyRuntimeRecordPath(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "runtime", runtimeRecordName)
}

// Record path with any legacy record deleted unread
func currentRuntimeRecordPath(ctx context.Context) string {
	_ = os.Remove(legacyRuntimeRecordPath(ctx))
	return runtimeRecordPath(ctx)
}

// Persists port, token and pid of a ready sidecar owner only, a failure logged
func writeRuntimeRecord(ctx context.Context, port int, token string, pid int) {
	path := currentRuntimeRecordPath(ctx)
	data, _ := json.Marshal(map[string]any{"port": port, "token": token, "pid": pid})
	tmp := path + ".tmp"
	err := os.WriteFile(tmp, data, 0o600)
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[photon] failed to write sidecar runtime record: %v", err))
	}
}

// Recorded sidecar address, nil when absent or unreadable
func readRuntimeRecord(ctx context.Context) map[string]any {
	raw, err := os.ReadFile(currentRuntimeRecordPath(ctx))
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(text), &out) != nil {
		return nil
	}
	return out
}

// Removes the record and its legacy twin
func deleteRuntimeRecord(ctx context.Context) {
	_ = os.Remove(runtimeRecordPath(ctx))
	_ = os.Remove(legacyRuntimeRecordPath(ctx))
}

// Whether the recorded sidecar pid is running
func sidecarPIDAlive(pid any) bool {
	n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(pid)))
	if f, ok := pid.(float64); ok {
		n, err = int(f), nil
	}
	if err != nil || n <= 0 {
		return false
	}
	return gateway.PidExists(n)
}

// Sidecar start failure, retryable unless it is deterministic like missing deps or node
type sidecarStartupError struct {
	msg       string
	code      string
	retryable bool
}

func (e *sidecarStartupError) Error() string { return e.msg }

// Structured failure answered by the sidecar
type sidecarError struct {
	path       string
	statusCode int
	errText    string
	errorClass string
	retryable  bool
}

func (e *sidecarError) Error() string {
	return fmt.Sprintf("Photon sidecar %s returned %d (%s, retryable=%s): %s", e.path, e.statusCode, e.errorClass,
		pyrepr.Repr(e.retryable), e.errText)
}

// Structured sidecar error of a non success answer, target_not_allowed carrying the canonical explanation
func sidecarErrorFromResponse(path string, status int, text string, data map[string]any) *sidecarError {
	if data == nil {
		_ = json.Unmarshal([]byte(text), &data)
	}
	errText := pyrepr.Str(data["error"])
	if !pyrepr.Truthy(data["error"]) {
		errText = textutil.First(text, 200)
		if errText == "" {
			errText = "sidecar error"
		}
	}
	errorClass := "sidecar_error"
	if pyrepr.Truthy(data["error_class"]) {
		errorClass = pyrepr.Str(data["error_class"])
	}
	retryable := pyrepr.Truthy(data["retryable"])
	if errorClass == "target_not_allowed" {
		errText, retryable = targetNotAllowedMessage, false
	}
	return &sidecarError{path: path, statusCode: status, errText: errText, errorClass: errorClass, retryable: retryable}
}

// Whether spectrum-ts itself sits under node_modules, not just a half created node_modules
func sidecarDepsInstalled(ctx context.Context) bool {
	_, err := os.Stat(filepath.Join(SidecarDir(ctx), "node_modules", "spectrum-ts"))
	return err == nil
}

// Managed node or npm binary installed through the package manager, empty when absent
func managedBinary(ctx context.Context, name string) string {
	installed, err := pm.Installed(ctx, name, false)
	if err != nil || installed == nil {
		return ""
	}
	return installed.Binary
}

// Managed binary, installed through the package manager first when missing
func ensureManagedBinary(ctx context.Context, name string, explicit bool) (string, map[string]string, error) {
	if bin := managedBinary(ctx, name); bin != "" {
		return bin, pm.WithIcebathNodePath(nil), nil
	}
	runner, err := pm.Ensure(ctx, name, pm.EnsureOptions{Explicit: explicit})
	if err != nil {
		return "", nil, err
	}
	bin := managedBinary(ctx, name)
	if bin == "" {
		return "", nil, fmt.Errorf("%s: ensured but no selected binary was recorded", name)
	}
	return bin, runner.Env, nil
}

// Whether the platform is ready, or may prepare node and the sidecar deps at connect, never provisioning here
func checkRequirements(ctx context.Context) bool {
	canPrepare := pm.LazyInstallsAllowedFor(ctx)
	nodeBin, _ := gateway.GetScopedSecret(ctx, "PHOTON_NODE_BIN", "", false)
	if nodeBin == "" && managedBinary(ctx, "node") == "" && !canPrepare {
		pkgLog().Warn("photon: node binary not found on PATH, in the pm store, or via PHOTON_NODE_BIN")
		return false
	}
	if sidecarDepsInstalled(ctx) {
		return true
	}
	if (managedBinary(ctx, "npm") != "" || canPrepare) && DirWritable(SidecarDir(ctx)) {
		return true
	}
	npmError := ""
	if raw, err := os.ReadFile(NpmErrorLog(ctx)); err == nil {
		npmError = textutil.First(strings.TrimSpace(string(raw)), NpmErrorLogMaxChars)
	}
	if npmError != "" {
		pkgLog().Debug(fmt.Sprintf("photon: spectrum-ts not installed at %s (last npm error: %s) — run: icebath photon setup", SidecarDir(ctx), npmError))
	} else {
		pkgLog().Debug(fmt.Sprintf("photon: spectrum-ts not installed at %s — run: icebath photon setup", SidecarDir(ctx)))
	}
	return false
}

// Reinstalls the sidecar node_modules from the lockfile, npm ci falling back to npm install, failures logged
func reinstallSidecarDeps(ctx context.Context) {
	npm, env, err := ensureManagedBinary(ctx, "npm", false)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[photon] cannot prepare sidecar dependencies: %v", err))
		return
	}
	run := func(verb string) (int, string, error) {
		runCtx, cancel := context.WithTimeout(ctx, npmReinstallTimeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, npm, verb)
		cmd.Dir, cmd.Env = SidecarDir(ctx), envList(env)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		if runCtx.Err() == context.DeadlineExceeded {
			return -1, "", runCtx.Err()
		}
		if cmd.ProcessState == nil {
			return -1, out.String(), err
		}
		return cmd.ProcessState.ExitCode(), out.String(), nil
	}
	code, out, err := run("ci")
	if err == nil && code != 0 {
		pkgLog().Warn("[photon] sidecar `npm ci` failed; falling back to `npm install`")
		code, out, err = run("install")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		pkgLog().Error(fmt.Sprintf("[photon] sidecar dependency reinstall timed out after %.0fs", npmReinstallTimeout.Seconds()))
		return
	}
	if err == nil && code == 0 {
		pkgLog().Info("[photon] sidecar dependencies reinstalled from lockfile")
		return
	}
	detail := strings.TrimSpace(out)
	if err != nil && detail == "" {
		detail = err.Error()
	}
	pkgLog().Error("[photon] sidecar dependency reinstall failed: " + detail)
}

// Env map as KEY=VALUE pairs, the process env when nil
func envList(env map[string]string) []string {
	if env == nil {
		return os.Environ()
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// Whether replies go out as markdown, PHOTON_MARKDOWN=false switching to plain text
func markdownEnabled(ctx context.Context) bool {
	v, _ := gateway.GetScopedSecret(ctx, "PHOTON_MARKDOWN", "true", false)
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "false", "0", "no":
		return false
	}
	return true
}

// Send text, verbatim only when the sidecar will use the markdown builder, stripped with link targets otherwise
func sidecarPayloadText(text string, sendMarkdown bool) string {
	if sendMarkdown && !sidecarURLRE.MatchString(text) {
		return text
	}
	return gateway.StripMarkdown(text, true)
}

// URL of a message that is exactly one http(s) URL
func urlOnlyCandidate(text string) string {
	candidate := strings.TrimSpace(text)
	if !urlOnlyRE.MatchString(candidate) {
		return ""
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Host == "" {
		return ""
	}
	if s := strings.ToLower(parsed.Scheme); s != "http" && s != "https" {
		return ""
	}
	return candidate
}

// URL to send as a rich link when markdown is enabled
func richlinkCandidate(ctx context.Context, text string) string {
	if !markdownEnabled(ctx) {
		return ""
	}
	return urlOnlyCandidate(text)
}

// Title, summary and URL lines of a rich link payload
func formatRichlinkContent(content map[string]any) string {
	get := func(k string) string { return strings.TrimSpace(pyrepr.Str(orEmpty(content[k]))) }
	urlText, title, summary := get("url"), get("title"), get("summary")
	if summary == title {
		summary = ""
	}
	var parts []string
	for _, p := range []string{title, summary, urlText} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "[Photon rich link received with no URL]"
	}
	return strings.Join(parts, "\n")
}

// Nil as empty text
func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Content map of every well formed item of a group payload
func groupItemContents(content map[string]any) []map[string]any {
	items, _ := content["items"].([]any)
	var out []map[string]any
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if c, ok := item["content"].(map[string]any); ok {
			out = append(out, c)
		}
	}
	return out
}

// URL only text or rich link URL of a payload, searching group items
func richlinkURLFromContent(content map[string]any) string {
	switch content["type"] {
	case "text":
		return urlOnlyCandidate(pyrepr.Str(orEmpty(content["text"])))
	case "richlink":
		return urlOnlyCandidate(pyrepr.Str(orEmpty(content["url"])))
	case "group":
		for _, c := range groupItemContents(content) {
			if u := richlinkURLFromContent(c); u != "" {
				return u
			}
		}
	}
	return ""
}

// Whether an attachment is Open Graph preview art by its name or id marker
func isRichlinkPreviewAttachment(payload map[string]any) bool {
	if payload["type"] != "attachment" {
		return false
	}
	for _, k := range []string{"name", "id"} {
		if strings.Contains(strings.ToLower(pyrepr.Str(orEmpty(payload[k]))), richlinkPreviewSuffix) {
			return true
		}
	}
	return false
}

// Label of preview art for the suppression log
func richlinkPreviewLabel(content map[string]any) string {
	label := func(c map[string]any) string {
		for _, k := range []string{"name", "id"} {
			if pyrepr.Truthy(c[k]) {
				return pyrepr.Str(c[k])
			}
		}
		return "(unnamed)"
	}
	switch content["type"] {
	case "attachment":
		return label(content)
	case "group":
		var labels []string
		for _, c := range groupItemContents(content) {
			labels = append(labels, label(c))
		}
		if len(labels) == 0 {
			return "(group)"
		}
		return strings.Join(labels, ", ")
	}
	return "(unknown)"
}

// Whether the payload is preview art or a non empty group made only of it
func isRichlinkPreviewContent(content map[string]any) bool {
	if isRichlinkPreviewAttachment(content) {
		return true
	}
	if content["type"] != "group" {
		return false
	}
	items, _ := content["items"].([]any)
	contents := groupItemContents(content)
	if len(items) == 0 || len(contents) != len(items) {
		return false
	}
	for _, c := range contents {
		if !isRichlinkPreviewAttachment(c) {
			return false
		}
	}
	return true
}

// Time of an ISO timestamp, now when empty or malformed
func parseTimestamp(ts string) time.Time {
	if ts == "" {
		return time.Now().UTC()
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return time.Now().UTC()
}

// Message type of an attachment MIME, documents for anything that is not image, video or audio
func attachmentMessageType(mime string) gateway.MessageType {
	mime = strings.ToLower(mime)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return gateway.MessageTypePhoto
	case strings.HasPrefix(mime, "video/"):
		return gateway.MessageTypeVideo
	case strings.HasPrefix(mime, "audio/"):
		return gateway.MessageTypeAudio
	}
	return gateway.MessageTypeDocument
}

// Inline base64 attachment bytes decoded into the media cache by MIME, empty when there are none or caching failed
func cacheInboundAttachment(ctx context.Context, content map[string]any, name, mime string, forceAudio bool) string {
	data, _ := content["data"].(string)
	if data == "" {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[photon] failed to decode inbound attachment bytes: %v", err))
		return ""
	}
	mime = strings.ToLower(mime)
	suffix := ""
	if name != "" {
		if i := strings.LastIndex(name, "."); i > 0 && i < len(name)-1 && !strings.ContainsAny(name[i:], `/\`) {
			suffix = name[i:]
		}
	}
	pick := func(m map[string]string, def string) string {
		if suffix != "" {
			return suffix
		}
		if e, ok := m[mime]; ok {
			return e
		}
		return def
	}
	var path string
	switch {
	case strings.HasPrefix(mime, "image/"):
		if path, err = gateway.CacheImageFromBytesAsync(ctx, raw, pick(imageExtByMIME, ".jpg")); err != nil {
			path, err = gateway.CacheDocumentFromBytesAsync(ctx, raw, name)
		}
	case forceAudio || strings.HasPrefix(mime, "audio/"):
		def := ".mp3"
		if forceAudio {
			def = ".m4a"
		}
		path, err = gateway.CacheAudioFromBytesAsync(ctx, raw, pick(audioExtByMIME, def))
	default:
		path, err = gateway.CacheDocumentFromBytesAsync(ctx, raw, name)
	}
	if err != nil {
		pkgLog().Warn(fmt.Sprintf("[photon] failed to cache inbound attachment %s: %v", name, err))
		return ""
	}
	return path
}

// Text, type, media paths and media MIME types of a normalized payload
type normalized struct {
	text       string
	mtype      gateway.MessageType
	mediaURLs  []string
	mediaTypes []string
}

// Cached inline attachment or voice payload, a text marker when it carried no bytes
func normalizeBinaryPayload(ctx context.Context, payload map[string]any) normalized {
	isVoice := payload["type"] == "voice"
	name := pyrepr.Str(orEmpty(payload["name"]))
	if name == "" {
		name = "(unnamed)"
		if isVoice {
			name = "voice"
		}
	}
	mime := pyrepr.Str(orEmpty(payload["mimeType"]))
	isVoice = isVoice || strings.HasSuffix(strings.ToLower(name), ".caf") || mime == "audio/x-caf"
	mtype := attachmentMessageType(mime)
	label := "attachment"
	if isVoice {
		mtype, label = gateway.MessageTypeVoice, "voice"
	}
	if cached := cacheInboundAttachment(ctx, payload, name, mime, isVoice); cached != "" {
		shown := mime
		if shown == "" {
			shown = "application/octet-stream"
			if isVoice {
				shown = "audio/mp4"
			}
		}
		return normalized{"(" + label + ")", mtype, []string{cached}, []string{shown}}
	}
	durationText := ""
	switch d := payload["duration"].(type) {
	case float64:
		durationText = ", duration: " + strconv.FormatFloat(d, 'f', -1, 64) + "s"
	}
	shownMime := mime
	if shownMime == "" {
		shownMime = "unknown MIME"
	}
	return normalized{text: fmt.Sprintf("[Photon %s received: %s (%s%s)]", label, name, shownMime, durationText), mtype: mtype}
}

// Group payload of text, rich links and attachments
func normalizeGroupContent(ctx context.Context, content map[string]any) normalized {
	var textParts []string
	out := normalized{mtype: gateway.MessageTypeText}
	for _, item := range groupItemContents(content) {
		switch t := item["type"]; t {
		case "attachment", "voice":
			n := normalizeBinaryPayload(ctx, item)
			if out.mtype == gateway.MessageTypeText {
				out.mtype = n.mtype
			}
			out.mediaURLs, out.mediaTypes = append(out.mediaURLs, n.mediaURLs...), append(out.mediaTypes, n.mediaTypes...)
			if len(n.mediaURLs) == 0 {
				textParts = append(textParts, n.text)
			}
		case "text":
			textParts = append(textParts, pyrepr.Str(orEmpty(item["text"])))
		case "richlink":
			textParts = append(textParts, formatRichlinkContent(item))
		default:
			if pyrepr.Truthy(t) {
				textParts = append(textParts, fmt.Sprintf("[Photon content type not handled: %v]", t))
			}
		}
	}
	if len(out.mediaURLs) > 0 && out.mtype == gateway.MessageTypeText {
		out.mtype = gateway.MessageTypeDocument
	}
	var kept []string
	for _, p := range textParts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	out.text = strings.TrimSpace(strings.Join(kept, "\n"))
	if out.text == "" {
		out.text = "[Photon empty group received]"
		if len(out.mediaURLs) > 0 {
			out.text = "(attachment)"
		}
	}
	return out
}

// User typed text of a payload without decoding any attachment bytes, for the group mention gate
func mentionGateText(content map[string]any) string {
	switch content["type"] {
	case "text":
		return pyrepr.Str(orEmpty(content["text"]))
	case "richlink":
		return formatRichlinkContent(content)
	case "group":
		var parts []string
		for _, c := range groupItemContents(content) {
			if p := mentionGateText(c); p != "" {
				parts = append(parts, p)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// Normalized text, type and media of a sidecar content payload
func normalizeContent(ctx context.Context, content map[string]any) normalized {
	switch content["type"] {
	case "text":
		return normalized{text: pyrepr.Str(orEmpty(content["text"])), mtype: gateway.MessageTypeText}
	case "attachment", "voice":
		return normalizeBinaryPayload(ctx, content)
	case "richlink":
		return normalized{text: formatRichlinkContent(content), mtype: gateway.MessageTypeText}
	case "group":
		return normalizeGroupContent(ctx, content)
	}
	return normalized{text: fmt.Sprintf("[Photon content type not handled: %s]", pyrepr.Repr(content["type"])), mtype: gateway.MessageTypeText}
}

// Bare phone of a DM chat GUID, any other id unchanged
func normalizeChatKey(chatID string) string {
	if m := dmChatGUIDRE.FindStringSubmatch(chatID); m != nil {
		return m[1]
	}
	return chatID
}

// Label quoted when a later reply targets a sent attachment
func attachmentLabel(kind, name string) string { return "[" + kind + ": " + name + "]" }

// /send-attachment body with the optional keys only when supplied
func attachmentBody(spaceID, safePath, kind, name, mimeType, caption string) map[string]any {
	k := "attachment"
	if kind == "voice" {
		k = "voice"
	}
	body := map[string]any{"spaceId": spaceID, "path": safePath, "kind": k}
	for _, kv := range [][2]string{{"name", name}, {"mimeType", mimeType}, {"caption", caption}} {
		if kv[1] != "" {
			body[kv[0]] = kv[1]
		}
	}
	return body
}

// MIME type guessed from a path extension, empty when unknown
func guessMime(path string) string {
	t, _, _ := pymimetypes.GuessType(path, true)
	return t
}

// Insertion ordered map that drops its oldest key beyond the capacity
type boundedMap[V any] struct {
	keys []string
	vals map[string]V
	cap  int
}

// Empty bounded map of the capacity
func newBoundedMap[V any](capacity int) *boundedMap[V] {
	return &boundedMap[V]{vals: map[string]V{}, cap: capacity}
}

// Stores the value as the newest entry
func (m *boundedMap[V]) put(key string, value V) {
	if _, ok := m.vals[key]; ok {
		for i, k := range m.keys {
			if k == key {
				m.keys = append(m.keys[:i], m.keys[i+1:]...)
				break
			}
		}
	}
	m.vals[key] = value
	m.keys = append(m.keys, key)
	for len(m.keys) > m.cap {
		delete(m.vals, m.keys[0])
		m.keys = m.keys[1:]
	}
}

// Removes a key
func (m *boundedMap[V]) del(key string) {
	if _, ok := m.vals[key]; !ok {
		return
	}
	delete(m.vals, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			return
		}
	}
}

// Photon adapter, a supervised Node sidecar carrying the spectrum-ts gRPC stream both ways
type PhotonAdapter struct {
	*gateway.BasePlatformAdapter

	projectID       string
	projectSecret   string
	sidecarPort     int
	sidecarBind     string
	sidecarToken    string
	autostart       bool
	probeInterval   float64
	probeTimeout    float64
	probeMaxFailure int
	requireMention  bool
	mentionPatterns []*regexp2.Regexp
	dedup           *gateway.MessageDeduplicator
	httpClient      *http.Client

	mu              sync.Mutex
	nodeBin         string
	proc            *exec.Cmd
	procStdin       io.WriteCloser
	procDone        chan struct{}
	inboundRunning  bool
	inboundTask     *gateway.AdapterTask
	healthTask      *gateway.AdapterTask
	watchdogTask    *gateway.AdapterTask
	respawnMu       sync.Mutex
	probeFailures   int
	lastUpstream    time.Time
	sentMessageIDs  *boundedMap[time.Time]
	lastInbound     *boundedMap[string]
	recentRichlinks *boundedMap[time.Time]
	typingLastSent  map[string]time.Time
	pendingFFFC     map[string]context.CancelFunc
}

// Class attributes the Photon adapter declares over the base defaults
func photonAdapterTraits() gateway.AdapterTraits {
	traits := gateway.DefaultAdapterTraits()
	traits.MaxMessageLength = maxMessageLength
	traits.SupportsMessageEditing = false
	traits.SerialReceiveLoop = true
	traits.SupportsCodeBlocks = false
	traits.OkEmoji, traits.FailEmoji = "\U0001f44d", "\U0001f44e"
	return traits
}

// Random hex of n bytes
func tokenHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Adapter for a platform config, project credentials from env then extra then auth.json
func NewPhotonAdapter(ctx context.Context, config *gateway.PlatformConfig) (*PhotonAdapter, error) {
	if config == nil {
		config = gateway.NewPlatformConfig()
	}
	extra := config.Extra
	if extra == nil {
		extra = map[string]any{}
	}
	a := &PhotonAdapter{sidecarBind: defaultSidecarBind, dedup: gateway.NewMessageDeduplicator(dedupMaxSize, dedupWindowSeconds),
		httpClient: &http.Client{Transport: &http.Transport{DialContext: netpref.DialContext((&net.Dialer{}).DialContext), Proxy: nil}}, sentMessageIDs: newBoundedMap[time.Time](sentIDsMax),
		lastInbound: newBoundedMap[string](lastInboundChatsMax), recentRichlinks: newBoundedMap[time.Time](lastInboundChatsMax),
		typingLastSent: map[string]time.Time{}, pendingFFFC: map[string]context.CancelFunc{}}
	a.BasePlatformAdapter = gateway.NewBasePlatformAdapter(config, gateway.Platform("photon"), a)
	a.Traits = photonAdapterTraits()
	storedID, storedSecret, err := LoadProjectCredentials(ctx)
	if err != nil {
		return nil, err
	}
	secret := func(name string) string { v, _ := gateway.GetScopedSecret(ctx, name, "", false); return v }
	a.projectID = firstNonEmpty(secret("PHOTON_PROJECT_ID"), pyrepr.Str(orEmpty(extra["project_id"])), storedID)
	a.projectSecret = firstNonEmpty(secret("PHOTON_PROJECT_SECRET"), pyrepr.Str(orEmpty(extra["project_secret"])), storedSecret)
	var portRaw any = extra["sidecar_port"]
	if !pyrepr.Truthy(portRaw) {
		portRaw = secret("PHOTON_SIDECAR_PORT")
	}
	a.sidecarPort = gateway.CoercePort(portRaw, defaultSidecarPort)
	a.sidecarToken = firstNonEmpty(secret("PHOTON_SIDECAR_TOKEN"), tokenHex(16))
	autostart, _ := gateway.GetScopedSecret(ctx, "PHOTON_SIDECAR_AUTOSTART", "true", false)
	switch strings.ToLower(autostart) {
	case "0", "false", "no":
	default:
		a.autostart = true
	}
	a.nodeBin = firstNonEmpty(secret("PHOTON_NODE_BIN"), managedBinary(ctx, "node"))
	setting := func(key, env string) any {
		v, err := gateway.ExtraOrSecret(ctx, extra, key, env, nil, true)
		if err != nil {
			return nil
		}
		return v
	}
	a.probeInterval = floatSetting(setting("probe_interval_seconds", "PHOTON_PROBE_INTERVAL_SECONDS"), defaultProbeIntervalSecond)
	a.probeTimeout = floatSetting(setting("probe_timeout_seconds", "PHOTON_PROBE_TIMEOUT_SECONDS"), defaultProbeTimeoutSecond)
	a.probeMaxFailure = int(floatSetting(setting("probe_max_failures", "PHOTON_PROBE_MAX_FAILURES"), defaultProbeMaxFailures))
	var requireMention any = extra["require_mention"]
	if requireMention == nil {
		requireMention = secret("PHOTON_REQUIRE_MENTION")
	}
	switch strings.ToLower(strings.TrimSpace(pyrepr.Str(orEmpty(requireMention)))) {
	case "true", "1", "yes", "on":
		a.requireMention = true
	}
	var raw any
	if v, ok := extra["mention_patterns"]; ok {
		raw = v
	} else if v := secret("PHOTON_MENTION_PATTERNS"); v != "" {
		raw = v
	}
	a.mentionPatterns = gateway.CompileMentionPatterns(raw, gateway.MentionPatternOptions{LogPrefix: "photon", Defaults: defaultMentionPatterns})
	return a, nil
}

// First non empty value
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// Float of a setting, the default when nil or unparseable
func floatSetting(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f
		}
	}
	return def
}

// Whether any wake word pattern matches the text
func (a *PhotonAdapter) messageMatchesMentionPatterns(text string) bool {
	if text == "" {
		return false
	}
	for _, p := range a.mentionPatterns {
		if ok, err := p.MatchString(text); err == nil && ok {
			return true
		}
	}
	return false
}

// Text without a leading wake word, later words untouched
func (a *PhotonAdapter) cleanMentionText(text string) string {
	stripped := strings.TrimLeft(text, " \t\n\r\x0b\x0c")
	for _, p := range a.mentionPatterns {
		m, err := p.FindStringMatch(stripped)
		if err != nil || m == nil {
			continue
		}
		start, length := m.ByteRange()
		if start != 0 {
			continue
		}
		rest := strings.TrimLeft(stripped[length:], " ,:-")
		if rest == "" {
			return text
		}
		return rest
	}
	return text
}

// Loopback URL of a sidecar path
func (a *PhotonAdapter) sidecarURL(path string) string {
	return fmt.Sprintf("http://%s:%d%s", a.sidecarBind, a.sidecarPort, path)
}

// Connects by starting the sidecar, then the inbound stream, health monitor and presence watchdog
func (a *PhotonAdapter) Connect(ctx context.Context, isReconnect bool) (bool, error) {
	if a.projectID == "" || a.projectSecret == "" {
		a.SetFatalError(ctx, "MISSING_CREDENTIALS", "PHOTON_PROJECT_ID and PHOTON_PROJECT_SECRET are required. Run: icebath photon setup", false)
		return false, nil
	}
	if a.autostart {
		if err := a.startSidecar(ctx); err != nil {
			var startup *sidecarStartupError
			if errors.As(err, &startup) {
				a.SetFatalError(ctx, startup.code, startup.msg, startup.retryable)
			} else {
				a.SetFatalError(ctx, "SIDECAR_FAILED", "failed to start Photon sidecar: "+err.Error(), true)
			}
			deleteRuntimeRecord(ctx)
			return false, nil
		}
	} else {
		pkgLog().Warn("[photon] sidecar autostart disabled — inbound + outbound will fail")
	}
	a.mu.Lock()
	a.inboundRunning = true
	a.lastUpstream = time.Now()
	a.mu.Unlock()
	inbound := gateway.SpawnAdapterTask(ctx, a.inboundLoop)
	health := gateway.SpawnAdapterTask(ctx, a.monitorSidecarHealth)
	var watchdog *gateway.AdapterTask
	if a.probeInterval > 0 && a.autostart {
		watchdog = gateway.SpawnAdapterTask(ctx, a.presenceWatchdog)
	}
	a.mu.Lock()
	a.inboundTask, a.healthTask, a.watchdogTask = inbound, health, watchdog
	a.mu.Unlock()
	a.MarkConnected(ctx, "")
	pkgLog().Info(fmt.Sprintf("[photon] connected — sidecar on %s:%d, streaming inbound over gRPC", a.sidecarBind, a.sidecarPort))
	a.WirePluginHandlers(ctx, nil)
	return true, nil
}

// Stops the watchdog, health and inbound tasks, pending placeholders and the sidecar
func (a *PhotonAdapter) Disconnect(ctx context.Context) error {
	a.mu.Lock()
	a.inboundRunning = false
	watchdog, health, inbound := a.watchdogTask, a.healthTask, a.inboundTask
	a.watchdogTask, a.healthTask, a.inboundTask = nil, nil, nil
	for key, cancel := range a.pendingFFFC {
		cancel()
		delete(a.pendingFFFC, key)
	}
	a.mu.Unlock()
	gateway.CancelTask(ctx, watchdog)
	gateway.CancelTask(ctx, health)
	gateway.CancelTask(ctx, inbound)
	a.stopSidecar(ctx)
	a.httpClient.CloseIdleConnections()
	a.MarkDisconnected(ctx)
	return nil
}

// Notifies the gateway of a fatal error from a detached goroutine the disconnect it triggers cannot cancel
func (a *PhotonAdapter) dispatchFatalNotification(ctx context.Context) {
	go func() {
		if err := a.NotifyFatalError(context.WithoutCancel(ctx)); err != nil {
			pkgLog().Warn(fmt.Sprintf("[photon] fatal-error notification failed: %v", err))
		}
	}()
}

// Whether the inbound side is still meant to run
func (a *PhotonAdapter) running() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inboundRunning
}

// Consumes the sidecar NDJSON inbound stream, reopening it with backoff when it drops
func (a *PhotonAdapter) inboundLoop(ctx context.Context) error {
	backoff := 1.0
	for a.running() {
		err := a.consumeInbound(ctx, &backoff)
		if ctx.Err() != nil || !a.running() {
			return nil
		}
		pkgLog().Warn(fmt.Sprintf("[photon] inbound stream dropped (%v); reconnecting in %.1fs", err, backoff))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Duration(backoff * float64(time.Second))):
		}
		backoff = min(backoff*2, 30.0)
	}
	return nil
}

// One /inbound stream until it ends, LF the only line delimiter and blank lines heartbeats
func (a *PhotonAdapter) consumeInbound(ctx context.Context, backoff *float64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.sidecarURL("/inbound"), nil)
	if err != nil {
		return err
	}
	req.Header.Set(sidecarTokenHeader, a.sidecarToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/inbound returned %d", resp.StatusCode)
	}
	*backoff = 1.0
	reader := bufio.NewReaderSize(resp.Body, 64*1024)
	for a.running() {
		line, err := reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			a.onInboundLine(ctx, trimmed)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return errors.New("stream ended")
			}
			return err
		}
	}
	return nil
}

// Promotes a degraded upstream stream reported by /healthz into a fatal reconnect
func (a *PhotonAdapter) monitorSidecarHealth(ctx context.Context) error {
	for a.running() {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(sidecarHealthInterval):
		}
		if !a.running() {
			return nil
		}
		data, err := a.sidecarCall(ctx, "/healthz", map[string]any{})
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			pkgLog().Debug(fmt.Sprintf("[photon] sidecar health check failed: %v", err))
			continue
		}
		stream, ok := data["stream"].(map[string]any)
		if !ok {
			continue
		}
		if staleness, ok := stream["staleness"].(map[string]any); ok && staleness["zombieSuspected"] == true {
			pkgLog().Warn(fmt.Sprintf("[photon] sidecar reports suspected zombie stream (silentForMs=%v, lastProbeOutcome=%v)",
				staleness["silentForMs"], staleness["lastProbeOutcome"]))
		}
		if stream["ok"] != false {
			continue
		}
		state := pyrepr.Str(orEmpty(stream["state"]))
		if state == "" {
			state = "unknown"
		}
		issue := pyrepr.Str(orEmpty(stream["lastIssue"]))
		if issue == "" {
			issue = "unknown stream issue"
		}
		message := fmt.Sprintf("Photon upstream stream degraded (state=%s, degradedForMs=%v): %s", state, stream["degradedForMs"], issue)
		pkgLog().Error("[photon] " + message)
		a.SetFatalError(ctx, "UPSTREAM_STREAM_DEGRADED", message, true)
		a.dispatchFatalNotification(ctx)
		return nil
	}
	return nil
}

// Handles one inbound line, de-duplicated by message id
func (a *PhotonAdapter) onInboundLine(ctx context.Context, line string) {
	a.noteUpstreamActivity()
	var event map[string]any
	if err := json.Unmarshal([]byte(line), &event); err != nil {
		pkgLog().Debug("[photon] skipping non-JSON inbound line")
		return
	}
	if id := pyrepr.Str(orEmpty(event["messageId"])); id != "" && a.dedup.IsDuplicate(id) {
		return
	}
	if err := a.dispatchInbound(ctx, event); err != nil {
		pkgLog().Error("[photon] inbound dispatch failed", "error", err)
	}
}

// Cancels a pending placeholder wait of the chat, true when one was live
func (a *PhotonAdapter) cancelPendingFFFC(chatKey string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cancel, ok := a.pendingFFFC[chatKey]
	if ok {
		cancel()
		delete(a.pendingFFFC, chatKey)
	}
	return ok
}

// Normalizes and dispatches one sidecar inbound event
func (a *PhotonAdapter) dispatchInbound(ctx context.Context, event map[string]any) error {
	space, _ := event["space"].(map[string]any)
	sender, _ := event["sender"].(map[string]any)
	content, _ := event["content"].(map[string]any)
	if content == nil {
		content = map[string]any{}
	}
	spaceID := pyrepr.Str(orEmpty(space["id"]))
	if spaceID == "" {
		pkgLog().Warn("[photon] inbound missing space.id")
		return nil
	}
	chatType := "dm"
	if space["type"] == "group" {
		chatType = "group"
	}
	senderID := firstNonEmpty(pyrepr.Str(orEmpty(sender["id"])), pyrepr.Str(orEmpty(space["phone"])), spaceID)
	timestamp := parseTimestamp(pyrepr.Str(orEmpty(event["timestamp"])))
	messageID := pyrepr.Str(orEmpty(event["messageId"]))
	var replyTo, replyText *string
	replyOwn := false
	if inner, ok := content["content"].(map[string]any); ok && content["type"] == "reply" && inner["type"] != nil && inner["type"] != "unknown" {
		targetID := pyrepr.Str(orEmpty(content["targetMessageId"]))
		targetText := pyrepr.Str(orEmpty(content["targetText"]))
		stored := ""
		storedOK := false
		if targetID != "" && targetText == "" {
			stored, storedOK = gateway.RichSentLookup(ctx, normalizeChatKey(spaceID), targetID)
		}
		a.mu.Lock()
		_, sent := a.sentMessageIDs.vals[targetID]
		a.mu.Unlock()
		replyOwn = content["targetDirection"] == "outbound" || (targetID != "" && sent) || storedOK
		if targetID != "" {
			replyTo = &targetID
		}
		if t := firstNonEmpty(targetText, stored); t != "" {
			replyText = &t
		}
		content = inner
	}
	ctype := content["type"]
	newEvent := func(text string, mtype gateway.MessageType) *gateway.MessageEvent {
		fields := gateway.SourceFields{ChatName: &spaceID, ChatType: chatType, UserID: &senderID}
		if senderID != "" {
			fields.UserName = &senderID
		}
		if messageID != "" {
			fields.MessageID = &messageID
		}
		ev := gateway.NewMessageEvent(text)
		ev.MessageType, ev.Source, ev.RawMessage, ev.Timestamp = mtype, a.BuildSource(ctx, spaceID, fields), event, timestamp
		if messageID != "" {
			ev.MessageID = &messageID
		}
		ev.ReplyToMessageID, ev.ReplyToText, ev.ReplyToIsOwnMessage = replyTo, replyText, replyOwn
		return ev
	}
	switch ctype {
	case "read", "read_receipt":
		pkgLog().Debug(fmt.Sprintf("[photon] outbound message read: %v", orDefault(content["targetMessageId"], "unknown")))
		return nil
	case "reaction":
		targetID := pyrepr.Str(orEmpty(content["targetMessageId"]))
		a.mu.Lock()
		_, sent := a.sentMessageIDs.vals[targetID]
		a.mu.Unlock()
		if content["targetDirection"] != "outbound" && !(targetID != "" && sent) {
			pkgLog().Debug("[photon] ignoring reaction on a message we didn't send")
			return nil
		}
		ev := newEvent("reaction:added:"+pyrepr.Str(orEmpty(content["emoji"])), gateway.MessageTypeText)
		ev.ReplyToMessageID, ev.ReplyToIsOwnMessage = nil, true
		if targetID != "" {
			ev.ReplyToMessageID = &targetID
		}
		ev.ReplyToText = nil
		if t := pyrepr.Str(orEmpty(content["targetText"])); t != "" {
			ev.ReplyToText = &t
		}
		return a.HandleMessage(ctx, ev)
	}
	if ctype == "text" && strings.TrimSpace(pyrepr.Str(orEmpty(content["text"]))) == "￼" {
		a.cancelPendingFFFC(spaceID)
		waitCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		a.mu.Lock()
		a.pendingFFFC[spaceID] = cancel
		a.mu.Unlock()
		go func() {
			select {
			case <-waitCtx.Done():
				return
			case <-time.After(fffcWait):
			}
			a.mu.Lock()
			_, still := a.pendingFFFC[spaceID]
			delete(a.pendingFFFC, spaceID)
			a.mu.Unlock()
			if still {
				pkgLog().Warn(fmt.Sprintf("[photon] wait for attachment was too long, can't retrieve attachment data (message %s, chat %s)", messageID, spaceID))
			}
		}()
		pkgLog().Debug("[photon] U+FFFC placeholder received — waiting for attachment")
		return nil
	}
	if (ctype == "attachment" || ctype == "voice") && a.cancelPendingFFFC(spaceID) {
		pkgLog().Debug("[photon] attachment arrived — cancelling U+FFFC timeout")
	}
	if a.isRecentRichlinkPreview(spaceID, content) {
		pkgLog().Info("[photon] suppressing rich-link preview attachment: " + richlinkPreviewLabel(content))
		return nil
	}
	a.recordLastInbound(spaceID, messageID)
	if ctype == "poll_option" {
		if content["selected"] == false {
			pkgLog().Debug("[photon] ignoring poll deselection")
			return nil
		}
		choice := strings.TrimSpace(pyrepr.Str(orEmpty(content["title"])))
		if choice == "" {
			pkgLog().Debug("[photon] ignoring poll vote with empty title")
			return nil
		}
		return a.HandleMessage(ctx, newEvent(choice, gateway.MessageTypeText))
	}
	gated := chatType == "group" && a.requireMention
	if gated && !a.messageMatchesMentionPatterns(mentionGateText(content)) {
		pkgLog().Debug("[photon] ignoring group message (require_mention=true, no mention pattern matched)")
		return nil
	}
	n := normalizeContent(ctx, content)
	if gated {
		n.text = a.cleanMentionText(n.text)
	}
	link := richlinkURLFromContent(content)
	if link == "" {
		link = n.text
	}
	a.recordRecentRichlink(spaceID, link)
	ev := newEvent(n.text, n.mtype)
	ev.MediaURLs, ev.MediaTypes = n.mediaURLs, n.mediaTypes
	return a.HandleMessage(ctx, ev)
}

// Value or the default for nil
func orDefault(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

// Remembers the latest inbound message of a chat as the default reaction target
func (a *PhotonAdapter) recordLastInbound(chatID, messageID string) {
	if chatID == "" || messageID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastInbound.put(normalizeChatKey(chatID), messageID)
}

// Notes a URL only message so its preview art that follows is suppressed
func (a *PhotonAdapter) recordRecentRichlink(chatID, text string) {
	if chatID == "" || urlOnlyCandidate(text) == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recentRichlinks.put(normalizeChatKey(chatID), time.Now())
}

// Whether the payload is preview art of a URL received in the chat within the suppression window
func (a *PhotonAdapter) isRecentRichlinkPreview(chatID string, content map[string]any) bool {
	if chatID == "" || !isRichlinkPreviewContent(content) {
		return false
	}
	key := normalizeChatKey(chatID)
	a.mu.Lock()
	defer a.mu.Unlock()
	last, ok := a.recentRichlinks.vals[key]
	if !ok {
		return false
	}
	if time.Since(last) > richlinkPreviewSuppress {
		a.recentRichlinks.del(key)
		return false
	}
	return true
}

// Short command output, nil when it could not run
func quickStdout(argv ...string) *string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return nil
	}
	s := string(out)
	return &s
}

// PIDs listening on a local TCP port
func findListenerPIDs(port int) []int {
	out := quickStdout("lsof", "-ti", fmt.Sprintf("tcp:%d", port), "-sTCP:LISTEN")
	if out == nil {
		return nil
	}
	var pids []int
	for _, tok := range strings.Fields(*out) {
		if n, err := strconv.Atoi(tok); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// Whether a pid's command line is a Photon sidecar
func pidIsSidecar(pid int) bool {
	out := quickStdout("ps", "-p", strconv.Itoa(pid), "-o", "command=")
	return out != nil && strings.Contains(*out, "photon/sidecar/index.mjs")
}

// Whether a pid is alive
func pidAlive(pid int) bool { return gateway.PidExists(pid) }

// Kills an orphaned sidecar squatting the port, refusing to touch any other listener
func (a *PhotonAdapter) reapStaleSidecar(ctx context.Context) error {
	if !reapSupported {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	req, _ := http.NewRequestWithContext(probeCtx, http.MethodPost, a.sidecarURL("/healthz"), nil)
	req.Header.Set(sidecarTokenHeader, a.sidecarToken)
	resp, err := a.httpClient.Do(req)
	cancel()
	if err != nil {
		return nil
	}
	resp.Body.Close()
	found := findListenerPIDs(a.sidecarPort)
	var stale, foreign []int
	for _, pid := range found {
		if pidIsSidecar(pid) {
			stale = append(stale, pid)
		} else {
			foreign = append(foreign, pid)
		}
	}
	fix := "free it or set PHOTON_SIDECAR_PORT to a different port"
	if len(stale) == 0 {
		shown := any(foreign)
		if len(foreign) == 0 {
			shown = "unknown"
		}
		return fmt.Errorf("port %d is in use by another process (pids: %v, not a Photon sidecar) — %s", a.sidecarPort, shown, fix)
	}
	for _, pid := range stale {
		pkgLog().Warn(fmt.Sprintf("[photon] reaping orphaned sidecar (pid %d) on port %d", pid, a.sidecarPort))
		signalPID(pid, false)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for _, p := range stale {
			alive = alive || pidAlive(p)
		}
		if !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, pid := range stale {
		if pidAlive(pid) {
			signalPID(pid, true)
		}
	}
	time.Sleep(200 * time.Millisecond)
	if len(foreign) > 0 {
		return fmt.Errorf("port %d is also held by non-sidecar processes (pids: %v) — %s", a.sidecarPort, foreign, fix)
	}
	return nil
}

// Installs missing sidecar deps or refreshes stale ones before spawning
func (a *PhotonAdapter) ensureSidecarDeps(ctx context.Context) error {
	if !sidecarDepsInstalled(ctx) {
		pkgLog().Info("[photon] sidecar deps not installed; installing into " + SidecarDir(ctx))
		reinstallSidecarDeps(ctx)
		if !sidecarDepsInstalled(ctx) {
			dir := SidecarDir(ctx)
			return &sidecarStartupError{msg: fmt.Sprintf("Photon sidecar deps could not be installed into %s (see log for the npm error). Run: cd %s && npm ci   (or `icebath photon setup`)", dir, dir),
				code: "SIDECAR_DEPS_MISSING", retryable: false}
		}
	}
	if lockNewerThanInstall(SidecarDir(ctx)) {
		pkgLog().Warn("[photon] sidecar deps are stale (lockfile newer than install); reinstalling before start")
		reinstallSidecarDeps(ctx)
	}
	return nil
}

// Runs the mixed attachment patch script, a failure only logged
func (a *PhotonAdapter) applySpectrumPatch(ctx context.Context, nodeBin string) {
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	dir := SidecarDir(ctx)
	cmd := exec.CommandContext(runCtx, nodeBin, filepath.Join(dir, "patch-spectrum-mixed-attachments.mjs"), dir)
	cmd.Env = envList(pm.WithIcebathNodePath(nil))
	hideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		detail := strings.TrimSpace(firstNonEmpty(stderr.String(), stdout.String()))
		if detail == "" {
			detail = err.Error()
		}
		pkgLog().Warn("[photon] failed to apply Spectrum mixed attachment patch: " + detail)
		return
	}
	if s := strings.TrimSpace(stderr.String()); s != "" {
		pkgLog().Debug("[photon] " + s)
	}
}

// Resolves node, prepares deps, spawns the sidecar and waits for /healthz, recording its address once ready
func (a *PhotonAdapter) startSidecar(ctx context.Context) error {
	a.mu.Lock()
	nodeBin := a.nodeBin
	a.mu.Unlock()
	if nodeBin == "" {
		bin, _, err := ensureManagedBinary(ctx, "node", false)
		if err != nil {
			return &sidecarStartupError{msg: err.Error(), code: "SIDECAR_NODE_MISSING", retryable: false}
		}
		nodeBin = bin
		a.mu.Lock()
		a.nodeBin = bin
		a.mu.Unlock()
	}
	if err := a.ensureSidecarDeps(ctx); err != nil {
		return err
	}
	if err := a.reapStaleSidecar(ctx); err != nil {
		return err
	}
	env := pm.WithIcebathNodePath(nil)
	env["PHOTON_PROJECT_ID"], env["PHOTON_PROJECT_SECRET"] = a.projectID, a.projectSecret
	env["PHOTON_SIDECAR_PORT"], env["PHOTON_SIDECAR_BIND"] = strconv.Itoa(a.sidecarPort), a.sidecarBind
	env["PHOTON_SIDECAR_TOKEN"], env["PHOTON_SIDECAR_WATCH_STDIN"] = a.sidecarToken, "1"
	a.applySpectrumPatch(ctx, nodeBin)
	cmd := exec.Command(nodeBin, filepath.Join(SidecarDir(ctx), "index.mjs"))
	cmd.Env = envList(env)
	hideWindow(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	pr, pw := io.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := tools.SpawnOwned(ctx, cmd, "photon-sidecar"); err != nil {
		pw.Close()
		var execErr *exec.Error
		if errors.As(err, &execErr) || errors.Is(err, os.ErrNotExist) {
			return &sidecarStartupError{msg: fmt.Sprintf("node binary not found (%s) — install Node.js 18+ or run `icebath pm install`: %v", pyrepr.Repr(nodeBin), err),
				code: "SIDECAR_NODE_MISSING", retryable: false}
		}
		return err
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		pw.Close()
		close(done)
	}()
	a.mu.Lock()
	a.proc, a.procStdin, a.procDone = cmd, stdin, done
	a.mu.Unlock()
	go a.superviseSidecar(context.WithoutCancel(ctx), cmd, pr, done)
	deadline := time.Now().Add(sidecarReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-done:
			deleteRuntimeRecord(ctx)
			return fmt.Errorf("Photon sidecar exited with code %d before becoming ready", cmd.ProcessState.ExitCode())
		default:
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, _ := http.NewRequestWithContext(probeCtx, http.MethodPost, a.sidecarURL("/healthz"), nil)
		req.Header.Set(sidecarTokenHeader, a.sidecarToken)
		resp, err := a.httpClient.Do(req)
		cancel()
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				writeRuntimeRecord(ctx, a.sidecarPort, a.sidecarToken, cmd.Process.Pid)
				return nil
			}
		} else {
			lastErr = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(200 * time.Millisecond)
	}
	deleteRuntimeRecord(ctx)
	return fmt.Errorf("Photon sidecar did not become ready within 15s: %v", lastErr)
}

// Interface of a runner that knows a signal asked it to stop
type signalStopRunner interface{ StopRequestedBySignal() bool }

// Pumps sidecar output into the log and turns an unexpected exit into a fatal reconnect
func (a *PhotonAdapter) superviseSidecar(ctx context.Context, cmd *exec.Cmd, out io.Reader, done chan struct{}) {
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		pkgLog().Info("[photon-sidecar] " + strings.TrimRight(scanner.Text(), " \t\r\n"))
	}
	<-done
	a.mu.Lock()
	current := a.proc == cmd
	running := a.inboundRunning
	a.mu.Unlock()
	runnerStop := false
	if r, ok := a.GatewayRunner.(signalStopRunner); ok {
		runnerStop = r.StopRequestedBySignal()
	}
	if current && running && !runnerStop {
		code := cmd.ProcessState.ExitCode()
		pkgLog().Error(fmt.Sprintf("[photon] sidecar exited unexpectedly (code %d) — triggering reconnect", code))
		a.SetFatalError(ctx, "SIDECAR_CRASHED", fmt.Sprintf("Photon sidecar exited unexpectedly (code %d)", code), true)
		a.dispatchFatalNotification(ctx)
	}
}

// Stops the sidecar politely by stdin EOF and /shutdown, then by signal, removing its runtime record
func (a *PhotonAdapter) stopSidecar(ctx context.Context) {
	a.mu.Lock()
	cmd, stdin, done := a.proc, a.procStdin, a.procDone
	a.proc, a.procStdin, a.procDone = nil, nil, nil
	a.mu.Unlock()
	defer deleteRuntimeRecord(ctx)
	if cmd == nil {
		return
	}
	if stdin != nil {
		_ = stdin.Close()
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	if req, err := http.NewRequestWithContext(shutdownCtx, http.MethodPost, a.sidecarURL("/shutdown"), nil); err == nil {
		req.Header.Set(sidecarTokenHeader, a.sidecarToken)
		if resp, err := a.httpClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	cancel()
	select {
	case <-done:
		return
	case <-time.After(3 * time.Second):
	}
	terminateTree(cmd)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
	}
}

// Records proof the upstream channel is live
func (a *PhotonAdapter) noteUpstreamActivity() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastUpstream = time.Now()
	a.probeFailures = 0
}

// One /probe round trip, alive on 200, hung on a timeout and inconclusive otherwise
func (a *PhotonAdapter) probeOnce(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(a.probeTimeout*float64(time.Second)))
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, a.sidecarURL("/probe"), nil)
	if err != nil {
		return "inconclusive"
	}
	req.Header.Set(sidecarTokenHeader, a.sidecarToken)
	resp, err := a.httpClient.Do(req)
	if err != nil {
		var netErr net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
			pkgLog().Debug(fmt.Sprintf("[photon] probe HTTP call hung: %v", err))
			return "hung"
		}
		pkgLog().Debug(fmt.Sprintf("[photon] probe transport error (inconclusive): %v", err))
		return "inconclusive"
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return "alive"
	}
	return "inconclusive"
}

// Restarts the sidecar to renew a dead stream, one respawn at a time
func (a *PhotonAdapter) respawnSidecar(ctx context.Context, reason string) {
	if !a.respawnMu.TryLock() {
		pkgLog().Info("[photon] respawn already in progress; skipping")
		return
	}
	defer a.respawnMu.Unlock()
	pkgLog().Warn(fmt.Sprintf("[photon] presence watchdog: %s — respawning sidecar", reason))
	a.stopSidecar(ctx)
	if err := a.startSidecar(ctx); err != nil {
		pkgLog().Error("[photon] failed to respawn sidecar; watchdog will retry", "error", err)
		return
	}
	a.noteUpstreamActivity()
	pkgLog().Info("[photon] presence watchdog: sidecar respawned, gRPC stream renewed")
}

// Probes on a long interval when no traffic proved liveness, respawning after enough hung probes
func (a *PhotonAdapter) presenceWatchdog(ctx context.Context) error {
	interval := time.Duration(a.probeInterval * float64(time.Second))
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	if !sleep(interval) {
		return nil
	}
	for {
		a.mu.Lock()
		idle := time.Since(a.lastUpstream)
		a.mu.Unlock()
		if idle < interval {
			if !sleep(interval - idle) {
				return nil
			}
			continue
		}
		switch a.probeOnce(ctx) {
		case "alive":
			a.noteUpstreamActivity()
		case "hung":
			a.mu.Lock()
			a.probeFailures++
			failures := a.probeFailures
			a.mu.Unlock()
			pkgLog().Warn(fmt.Sprintf("[photon] presence probe hung (%d/%d)", failures, a.probeMaxFailure))
			if failures >= a.probeMaxFailure {
				a.respawnSidecar(ctx, fmt.Sprintf("%d consecutive hung probes", failures))
			}
		default:
			pkgLog().Debug("[photon] presence probe inconclusive; taking no action")
		}
		if !sleep(interval) {
			return nil
		}
	}
}
