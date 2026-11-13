package matrix

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dlclark/regexp2/v2"
	"maunium.net/go/mautrix"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pymimetypes"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyre"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/skills"
)

const matrixVoiceWaveformBins = 30

// Result of one ffmpeg or ffprobe run
type mediaToolResult struct {
	returnCode int
	stdout     []byte
}

// Runs ffmpeg or ffprobe with captured output, no stdin and a timeout
var runMediaTool = func(cmd []string, timeout time.Duration) (mediaToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Stdin = nil
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &bytes.Buffer{}
	err := c.Run()
	if ctx.Err() != nil {
		return mediaToolResult{}, fmt.Errorf("Command %s timed out after %v seconds", pyrepr.Repr(cmd), timeout.Seconds())
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return mediaToolResult{returnCode: exitErr.ExitCode(), stdout: out.Bytes()}, nil
	}
	if err != nil {
		return mediaToolResult{}, err
	}
	return mediaToolResult{stdout: out.Bytes()}, nil
}

// Resolves an executable on PATH, empty when absent
var whichFn = func(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}

// Duration in milliseconds and a thirty bin MSC1767 waveform, each omitted when its tool is missing or fails
func matrixVoiceMetadataForFile(path string) map[string]any {
	metadata := map[string]any{}
	if ffprobe := whichFn("ffprobe"); ffprobe != "" {
		result, err := runMediaTool([]string{ffprobe, "-v", "error", "-show_entries", "format=duration", "-of",
			"default=noprint_wrappers=1:nokey=1", path}, 10*time.Second)
		if err == nil && result.returnCode == 0 {
			text := pystr.Strip(pystr.DecodeUTF8Replace(result.stdout))
			if text == "" {
				text = "0"
			}
			duration, perr := pyint.ParseFloat(text)
			if perr != nil {
				err = perr
			} else if duration > 0 {
				metadata["duration"] = int(duration * 1000)
			}
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("Matrix: failed to probe voice duration for %s", path))
		}
	}
	if ffmpeg := whichFn("ffmpeg"); ffmpeg != "" {
		result, err := runMediaTool([]string{ffmpeg, "-v", "error", "-i", path, "-ac", "1", "-ar", "8000", "-f", "s16le", "-"}, 15*time.Second)
		if err == nil && result.returnCode == 0 && len(result.stdout) > 0 {
			if len(result.stdout)%2 != 0 {
				err = errors.New("bytes length not a multiple of item size")
			} else {
				count := len(result.stdout) / 2
				samples := make([]int, count)
				for i := range count {
					samples[i] = int(int16(binary.LittleEndian.Uint16(result.stdout[2*i:])))
				}
				waveform := make([]int, 0, matrixVoiceWaveformBins)
				for idx := range matrixVoiceWaveformBins {
					start := idx * count / matrixVoiceWaveformBins
					end := max(start+1, (idx+1)*count/matrixVoiceWaveformBins)
					peak := 0
					for _, v := range samples[start:min(end, count)] {
						peak = max(peak, abs(v))
					}
					waveform = append(waveform, min(1024, int(float64(peak)/32767*1024)))
				}
				metadata["waveform"] = waveform
			}
		}
		if err != nil {
			slog.Debug(fmt.Sprintf("Matrix: failed to build voice waveform for %s", path))
		}
	}
	return metadata
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

var matrixBangCommandRE = pyre.MustCompile(`^!([A-Za-z][A-Za-z0-9_-]*)(?=$|\s)(.*)$`)

// Whether a Python pattern matches at the start of the text like re.match, with the match
func reMatch(re *regexp2.Regexp, s string) (*regexp2.Match, bool) {
	m, err := re.FindStringMatch(s)
	if err != nil || m == nil || m.RuneIndex != 0 {
		return nil, false
	}
	return m, true
}

// Whether a Python pattern matches anywhere in the text like re.search
func reSearch(re *regexp2.Regexp, s string) bool {
	m, err := re.FindStringMatch(s)
	return err == nil && m != nil
}

// Every match of the pattern replaced by the function result like re.sub
func reSub(re *regexp2.Regexp, s string, fn func(m regexp2.Match) string) string {
	out, err := re.ReplaceFunc(s, fn, -1, -1)
	if err != nil {
		return s
	}
	return out
}

// Text of a numbered group, empty when the group did not take part
func group(m regexp2.Match, n int) string {
	g := m.GroupByNumber(n)
	if g == nil || len(g.Captures) == 0 {
		return ""
	}
	return g.String()
}

// Whether a name is a gateway command of the registry, installed by the process wiring from pkg/commands
var IsGatewayKnownCommand func(ctx context.Context, name string) bool

// Known gateway or skill command for a bang token, lowercased first then with underscores as dashes
var resolveMatrixBangCommand = func(ctx context.Context, name string) (string, bool) {
	if name == "" {
		return "", false
	}
	candidates := []string{pystr.Lower(name)}
	if dashed := strings.ReplaceAll(pystr.Lower(name), "_", "-"); dashed != candidates[0] {
		candidates = append(candidates, dashed)
	}
	for _, candidate := range candidates {
		if IsGatewayKnownCommand(ctx, candidate) {
			return candidate, true
		}
	}
	skillCommands := skills.NewScope(home.Dir(ctx), nil).GetSkillCommands()
	for _, candidate := range candidates {
		if _, ok := skillCommands["/"+candidate]; ok {
			return candidate, true
		}
	}
	return "", false
}

// Bang command aliases rewritten to slash command text, other text unchanged
func normalizeMatrixBangCommand(ctx context.Context, text string) string {
	if text == "" || !strings.HasPrefix(text, "!") {
		return text
	}
	m, ok := reMatch(matrixBangCommandRE, text)
	if !ok {
		return text
	}
	resolved, found := resolveMatrixBangCommand(ctx, group(*m, 1))
	if !found {
		return text
	}
	return "/" + resolved + group(*m, 2)
}

var matrixReplyFallbackPillRE = pyre.MustCompile(`^>\s*<(@[^>]+)>\s*(.*)$`)

// Quoted text and author mxid of the inline reply fallback, the author from the first line pill
func extractReplyFallback(body string) (quoted, author *string) {
	if body == "" || !strings.HasPrefix(body, "> ") {
		return nil, nil
	}
	var quotedLines []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "> ") {
			break
		}
		content := line[2:]
		if author == nil {
			if m, ok := reMatch(matrixReplyFallbackPillRE, line); ok {
				a := group(*m, 1)
				author = &a
				content = group(*m, 2)
			}
		}
		quotedLines = append(quotedLines, content)
	}
	if text := pystr.Strip(strings.Join(quotedLines, "\n")); text != "" {
		quoted = &text
	}
	return quoted, author
}

// Body with the inline quote and reply fallback prefix removed, unchanged when absent
func stripReplyFallback(body string) string {
	if body == "" || !strings.HasPrefix(body, "> ") {
		return body
	}
	var stripped []string
	pastFallback := false
	for _, line := range strings.Split(body, "\n") {
		if !pastFallback {
			if strings.HasPrefix(line, "> ") || line == ">" {
				continue
			}
			pastFallback = true
			if line == "" {
				continue
			}
		}
		stripped = append(stripped, line)
	}
	if len(stripped) == 0 {
		return body
	}
	return strings.Join(stripped, "\n")
}

var matrixPermanentErrcodes = map[string]bool{"m_unknown_token": true, "m_missing_token": true, "m_forbidden": true}

// Whether the error is a structured auth failure that must stop the sync loop
func isPermanentMatrixAuthError(err error) bool {
	var httpErr mautrix.HTTPError
	var httpErrPtr *mautrix.HTTPError
	switch {
	case errors.As(err, &httpErrPtr):
		httpErr = *httpErrPtr
	case errors.As(err, &httpErr):
	default:
		return false
	}
	if httpErr.RespError != nil && matrixPermanentErrcodes[pystr.Lower(pystr.Strip(httpErr.RespError.ErrCode))] {
		return true
	}
	if httpErr.Response != nil {
		return httpErr.Response.StatusCode == 401 || httpErr.Response.StatusCode == 403
	}
	return false
}

// Quote block and reply text that concatenate back to the body, the blank separator in the quote
func splitReplyFallback(body string) (string, string) {
	if body == "" || !strings.HasPrefix(body, "> ") {
		return "", body
	}
	lines := strings.Split(body, "\n")
	idx := 0
	for idx < len(lines) && (strings.HasPrefix(lines[idx], "> ") || lines[idx] == ">") {
		idx++
	}
	if idx < len(lines) && lines[idx] == "" {
		idx++
	}
	head := strings.Join(lines[:idx], "\n")
	if idx >= len(lines) {
		return head, ""
	}
	return head + "\n", strings.Join(lines[idx:], "\n")
}

// Resolved Matrix room identity for routing and prompt context
type MatrixRoomIdentity struct {
	RoomID              string
	RoomName            *string
	RoomTopic           *string
	CanonicalAlias      *string
	ServerName          *string
	JoinedMemberCount   *int
	IsDirectAccountData bool
	DisplayName         string
	HasExplicitName     bool
	ChatType            string
	Conflict            bool
}

// State every pending reaction prompt carries, the bot reaction event per seeded emoji included
type promptState struct {
	sessionKey        string
	chatID            string
	messageID         string
	resolved          bool
	requesterUserID   *string
	expiresAt         *float64
	botReactionEvents *orderedStrings
}

// Pending reaction based exec approval prompt
type matrixApprovalPrompt struct {
	promptState
}

// Pending reaction based picker whose choices map emoji to a selection
type matrixPickerPrompt struct {
	promptState
	choices    map[string][]string
	onSelected func(ctx context.Context, chatID string, args ...string) (string, error)
}

// String map that iterates in insertion order like a Python dict
type orderedStrings struct {
	keys []string
	vals map[string]string
}

func newOrderedStrings() *orderedStrings { return &orderedStrings{vals: map[string]string{}} }

func (o *orderedStrings) set(k, v string) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

const (
	maxContentBytes                = 45_000
	matrixMaxMessageLengthCeiling  = maxContentBytes / 3
	defaultMaxMessageLength        = matrixMaxMessageLengthCeiling
	startupGraceSeconds            = 5
	matrixDefaultMaxMediaBytes     = 100 * 1024 * 1024
	matrixRoomIdentityCacheMaximum = 256
)

// Length of the text in UTF-8 bytes
func utf8Len(text string) int { return len(text) }

// Size of event content as the homeserver counts it, compact JSON without ASCII escaping
func contentBytes(content map[string]any) int {
	out, err := pyjson.Dumps(content, pyjson.EnsureASCII(false), pyjson.Separators(",", ":"))
	if err != nil {
		return 0
	}
	return len(out)
}

// Outbound chunk size from config, env or the registry entry, clamped to 500 through the ceiling
func resolveMaxMessageLength(ctx context.Context, cfg *gateway.PlatformConfig) int {
	var extra map[string]any
	if cfg != nil {
		extra = cfg.Extra
	}
	raw, _ := gateway.ExtraOrSecret(ctx, extra, "max_message_length", "MATRIX_MAX_MESSAGE_LENGTH", nil, true)
	if raw == nil || pystr.Strip(pyrepr.Str(raw)) == "" {
		if entry := gateway.Registry.Get(ctx, "matrix"); entry != nil && entry.MaxMessageLength != 0 {
			raw = entry.MaxMessageLength
		}
	}
	if raw == nil {
		return defaultMaxMessageLength
	}
	value, err := pyint.Int64(raw)
	if err != nil {
		return defaultMaxMessageLength
	}
	return int(max(500, min(value, matrixMaxMessageLengthCeiling)))
}

var matrixImageFilenameExts = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".webp": true, ".bmp": true,
	".svg": true, ".heic": true, ".heif": true, ".avif": true}

var matrixMediaFilenameExts = map[string]bool{".ogg": true, ".oga": true, ".opus": true, ".m4a": true, ".mp3": true, ".wav": true,
	".flac": true, ".aac": true, ".amr": true, ".mp4": true, ".webm": true, ".mov": true, ".mkv": true}

var matrixModelPickerReactions = []string{"1️⃣", "2️⃣", "3️⃣", "4️⃣", "5️⃣",
	"6️⃣", "7️⃣", "8️⃣", "9️⃣", "\U0001f51f"}

var matrixChoicePickerReactions = append(append([]string{}, matrixModelPickerReactions...), "\U0001f170️", "\U0001f171️")

// Whether an m.image body is only the uploaded filename
func looksLikeMatrixImageFilename(text string) bool {
	return looksLikeTransportFilename(text, []string{"image/"}, matrixImageFilenameExts, false)
}

// Whether the text is a bare single token filename with a known media extension or matching guessed type
func looksLikeTransportFilename(text string, mimePrefixes []string, exts map[string]bool, rejectSpaces bool) bool {
	candidate := pystr.Strip(text)
	if candidate == "" || strings.Contains(candidate, "\n") || strings.HasSuffix(candidate, "/") {
		return false
	}
	if rejectSpaces && strings.ContainsFunc(candidate, pystr.IsSpace) {
		return false
	}
	if pypath.PathName(candidate) != candidate {
		return false
	}
	suffix := pystr.Lower(pypath.PathSuffix(candidate))
	if suffix == "" {
		return false
	}
	guessed, _, _ := pymimetypes.GuessType(candidate, true)
	if guessed != "" {
		for _, p := range mimePrefixes {
			if strings.HasPrefix(guessed, p) {
				return true
			}
		}
	}
	return exts[suffix]
}

// Whether an m.audio, m.file or m.video body is only the uploaded filename
func looksLikeMatrixMediaFilename(text string) bool {
	return looksLikeTransportFilename(text, []string{"audio/", "video/"}, matrixMediaFilenameExts, true)
}

// Whether a media event body is only the uploaded filename for its msgtype
func isBareMediaFilename(msgtype, body string) bool {
	if msgtype == "m.image" {
		return looksLikeMatrixImageFilename(body)
	}
	return (msgtype == "m.audio" || msgtype == "m.file" || msgtype == "m.video") && looksLikeMatrixMediaFilename(body)
}

// Event timestamp in seconds, accepting milliseconds or seconds
func matrixEventTimestampSeconds(ts int64) float64 {
	f := float64(ts)
	if f > 10_000_000_000 {
		return f / 1000.0
	}
	return f
}

// Canonical E2EE mode of a configured value, required, optional or off
func normalizeE2EEMode(value any) string {
	raw := ""
	if pyTruthy(value) {
		raw = pystr.Lower(pystr.Strip(pyrepr.Str(value)))
	}
	switch raw {
	case "required", "require", "true", "1", "yes", "on":
		return "required"
	case "optional", "prefer", "preferred":
		return "optional"
	}
	return "off"
}

// E2EE mode from config or env, the legacy encryption flag meaning required
func resolveE2EEMode(ctx context.Context, extra map[string]any) string {
	explicit := extra["e2ee_mode"]
	if !pyTruthy(explicit) {
		secret, _ := gateway.GetScopedSecret(ctx, "MATRIX_E2EE_MODE", "", false)
		explicit = secret
	}
	if pyTruthy(explicit) {
		return normalizeE2EEMode(explicit)
	}
	legacy, ok := extra["encryption"]
	if !ok {
		legacy = envTruthy(ctx, "MATRIX_ENCRYPTION", "")
	}
	if pyTruthy(legacy) {
		return "required"
	}
	return "off"
}

// Whether the scoped env value is true, 1 or yes
func envTruthy(ctx context.Context, name, def string) bool {
	v, _ := gateway.GetScopedSecret(ctx, name, def, false)
	switch pystr.Lower(v) {
	case "true", "1", "yes":
		return true
	}
	return false
}

// Scoped env value as a float, the default when it does not parse
func envFloat(ctx context.Context, name string, def float64) float64 {
	v, _ := gateway.GetScopedSecret(ctx, name, pyrepr.Str(def), false)
	f, err := pyint.ParseFloat(v)
	if err != nil {
		return def
	}
	return f
}

// Scoped env value as an int, the default when it does not parse
func envInt(ctx context.Context, name string, def int64) int64 {
	v, _ := gateway.GetScopedSecret(ctx, name, pyrepr.Str(def), false)
	n, err := pyint.Int64(v)
	if err != nil {
		return def
	}
	return n
}

// Stripped non empty tokens of a comma separated string or a list
func csvSet(raw any) map[string]bool {
	out := map[string]bool{}
	if list, ok := raw.([]any); ok {
		for _, r := range list {
			if s := pystr.Strip(pyrepr.Str(r)); s != "" {
				out[s] = true
			}
		}
		return out
	}
	for _, r := range strings.Split(pyrepr.Str(raw), ",") {
		if s := pystr.Strip(r); s != "" {
			out[s] = true
		}
	}
	return out
}

// Thread root event id an event belongs to, empty when it is not in a thread
func threadRoot(relatesTo map[string]any) string {
	if relatesTo["rel_type"] == "m.thread" {
		if s, ok := relatesTo["event_id"].(string); ok {
			return s
		}
		if v := relatesTo["event_id"]; v != nil {
			return pyrepr.Str(v)
		}
	}
	return ""
}

// Room or user list from the scoped env, then config extra, then empty
func extraCSVSet(ctx context.Context, cfg *gateway.PlatformConfig, key, envName string) map[string]bool {
	raw, _ := gateway.ExtraOrSecret(ctx, cfg.Extra, key, envName, "", false)
	return csvSet(raw)
}

// Configured recovery key output file through the profile scoped reader, empty when unset
func recoveryKeyOutputPath(ctx context.Context) string {
	v, _ := gateway.GetScopedSecret(ctx, "MATRIX_RECOVERY_KEY_OUTPUT_FILE", "", false)
	if out := pystr.Strip(v); out != "" {
		return pypath.Expanduser(out)
	}
	return ""
}

// Writes a generated recovery key once with mode 0600 to the configured file, never overwriting
func writeMatrixRecoveryKeyOutputFile(ctx context.Context, recoveryKey string) (string, error) {
	path := recoveryKeyOutputPath(ctx)
	if path == "" {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", err
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := fh.WriteString(recoveryKey + "\n"); err != nil {
		fh.Close()
		return "", err
	}
	if err := fh.Close(); err != nil {
		return "", err
	}
	return path, nil
}

// Usable one time recovery key output path, or the redacted reason it cannot be used
func getMatrixRecoveryKeyOutputTarget(ctx context.Context) (string, string) {
	path := recoveryKeyOutputPath(ctx)
	if path == "" {
		return "", "not_configured"
	}
	if _, err := os.Lstat(path); err == nil {
		return "", "exists"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return "", "unusable: " + pypath.OSErrorText(err)
	}
	return path, ""
}

// Stores or reports a freshly generated recovery key without ever logging the key
func handleGeneratedMatrixRecoveryKey(ctx context.Context, mxid, recoveryKey string) {
	outputPath, err := writeMatrixRecoveryKeyOutputFile(ctx, recoveryKey)
	if errors.Is(err, os.ErrExist) {
		slog.Warn(fmt.Sprintf("Matrix: bootstrapped cross-signing for %s. Recovery key output file "+
			"already exists; refusing to overwrite. Store the generated key "+
			"securely and set MATRIX_RECOVERY_KEY for future restarts.", mxid))
		return
	}
	if err != nil {
		slog.Warn(fmt.Sprintf("Matrix: bootstrapped cross-signing for %s, but failed to write "+
			"MATRIX_RECOVERY_KEY_OUTPUT_FILE: %s. Store the generated key "+
			"securely and set MATRIX_RECOVERY_KEY for future restarts.", mxid, pypath.OSErrorText(err)))
		return
	}
	if outputPath != "" {
		slog.Warn(fmt.Sprintf("Matrix: bootstrapped cross-signing for %s. A new recovery key was written to %s with mode 0600. Move it "+
			"to your secret store and set MATRIX_RECOVERY_KEY for future restarts.", mxid, outputPath))
		return
	}
	slog.Warn(fmt.Sprintf("Matrix: bootstrapped cross-signing for %s. A new recovery key was generated but will "+
		"not be logged. Set MATRIX_RECOVERY_KEY_OUTPUT_FILE to write it once with mode 0600, "+
		"or configure MATRIX_RECOVERY_KEY from your Matrix client before future restarts.", mxid))
}

// Recovery key through the profile scoped reader, stripped
func scopedRecoveryKey(ctx context.Context) string {
	v, _ := gateway.GetScopedSecret(ctx, "MATRIX_RECOVERY_KEY", "", false)
	return pystr.Strip(v)
}

var (
	texTokenRE    = pyre.MustCompile(`HERMESTEX(?:DISPLAY|INLINE)(\d+)HERMESTEXEND`)
	texDisplayRE  = pyre.MustCompile(`\$\$([^\n$]+?)\$\$`)
	texInlineRE   = pyre.MustCompile(`(?<![\\$\w])\$([^\n$]+?)\$(?!\w)`)
	texDisplayFmt = "HERMESTEXDISPLAY%dHERMESTEXEND"
	texInlineFmt  = "HERMESTEXINLINE%dHERMESTEXEND"
)

// Tag and TeX source of one math span, div for display math and span for inline math
type texEntry struct{ tag, tex string }

// Text with paired dollar math swapped for sentinel tokens, plus the ordered math store
func latexToTokens(text string) (string, []texEntry) {
	if text == "" || !strings.Contains(text, "$") {
		return text, nil
	}
	var store []texEntry
	text = reSub(texDisplayRE, text, func(m regexp2.Match) string {
		store = append(store, texEntry{"div", pystr.Strip(group(m, 1))})
		return fmt.Sprintf(texDisplayFmt, len(store)-1)
	})
	text = reSub(texInlineRE, text, func(m regexp2.Match) string {
		store = append(store, texEntry{"span", pystr.Strip(group(m, 1))})
		return fmt.Sprintf(texInlineFmt, len(store)-1)
	})
	return text, store
}

// Sentinel tokens expanded into data-mx-maths markup with the TeX escaped, unknown tokens kept verbatim
func tokensToMxMaths(html string, store []texEntry) string {
	return reSub(texTokenRE, html, func(m regexp2.Match) string {
		idx, err := pyint.Int64(group(m, 1))
		if err != nil || idx >= int64(len(store)) {
			return m.String()
		}
		entry := store[idx]
		escaped := htmlEscape(entry.tex, true)
		return fmt.Sprintf(`<%s data-mx-maths="%s">%s</%s>`, entry.tag, escaped, escaped, entry.tag)
	})
}

// URL with query and fragment removed for logging signed media links
func redactURLForLog(raw string) string {
	parts, err := pyurl.Split(raw)
	if err != nil {
		return "<url>"
	}
	if parts.Scheme == "" && parts.Netloc == "" {
		s, _, _ := strings.Cut(raw, "?")
		s, _, _ = strings.Cut(s, "#")
		return s
	}
	return pyurl.Unsplit(pyurl.URL{Scheme: parts.Scheme, Netloc: parts.Netloc, Path: parts.Path})
}

var (
	preSanitizeScriptRE  = pyre.MustCompile(`(?is)<\s*(script|style)\b[^>]*>.*?<\s*/\s*\1\s*>`)
	preSanitizeHandlerRE = pyre.MustCompile(`(?is)\s+on[a-z0-9_-]+\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	preSanitizeSchemeRE  = pyre.MustCompile(`(?is)\s+(href|src)\s*=\s*("[^"]*(?:javascript|data|vbscript):[^"]*"|'[^']*(?:javascript|data|vbscript):[^']*'|[^\s>]*(?:javascript|data|vbscript):[^\s>]*)`)
)

// Markdown with script and style blocks, event handler attributes and unsafe link schemes removed
func preSanitizeMatrixMarkdown(text string) string {
	result := reSub(preSanitizeScriptRE, text, func(regexp2.Match) string { return "" })
	result = reSub(preSanitizeHandlerRE, result, func(regexp2.Match) string { return "" })
	return reSub(preSanitizeSchemeRE, result, func(regexp2.Match) string { return "" })
}

// Passive registry check, always satisfied since mautrix is compiled into the binary
func matrixDepsPresent() bool { return true }

// Active dependency check, false when E2EE is required but this build carries no olm implementation
func ensureMatrixDeps(ctx context.Context) (bool, error) {
	e2eeMode := resolveE2EEMode(ctx, nil)
	if e2eeMode == "required" && !checkE2EEDeps() {
		slog.Error(fmt.Sprintf("Matrix: E2EE is required but dependencies are missing. %s. Without this, encrypted "+
			"rooms will not work. Set MATRIX_E2EE_MODE=off to disable E2EE.", e2eeInstallHint))
		return false, nil
	}
	if e2eeMode == "optional" && !checkE2EEDeps() {
		slog.Warn(fmt.Sprintf("Matrix: E2EE optional but dependencies are missing. %s", e2eeInstallHint))
	}
	return true, nil
}

// Python truthiness of a config or JSON value
func pyTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Credentials and dependency answer for setup and status callers, credentials never gating the installer
func checkMatrixRequirements(ctx context.Context) bool {
	scoped := func(name string) string {
		v, _ := gateway.GetScopedSecret(ctx, name, "", false)
		return pystr.Strip(v)
	}
	if scoped("MATRIX_ACCESS_TOKEN") == "" && scoped("MATRIX_PASSWORD") == "" {
		slog.Debug("Matrix: neither MATRIX_ACCESS_TOKEN nor MATRIX_PASSWORD set")
		return false
	}
	if scoped("MATRIX_HOMESERVER") == "" {
		slog.Warn("Matrix: MATRIX_HOMESERVER not set")
		return false
	}
	ok, _ := ensureMatrixDeps(ctx)
	return ok
}
