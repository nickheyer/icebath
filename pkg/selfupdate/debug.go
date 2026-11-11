package selfupdate

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pysys"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

const (
	debugRedactionBanner   = "[icebath debug share: log content redacted at upload time. run with --no-redact to disable]\n"
	debugUserAgent         = "icebath-agent/debug-share"
	debugMaxLogBytes       = 512_000
	debugAutoDeleteSeconds = 21600
	debugNousBundleFormat  = "hermes-debug-share/1"
)

var (
	debugPasteRSURL             = "https://paste.rs/"
	debugDpasteComURL           = "https://dpaste.com/api/"
	debugLog                    = slog.Default().With("logger", "hermes_cli.debug")
	debugStderr       io.Writer = pysys.Stderr
	debugConsole                = func(w io.Writer) *cliui.Console { return &cliui.Console{In: os.Stdin, Out: w} }
	debugNow                    = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }
)

const debugPrivacyNotice = `⚠️  This will upload system info + logs to a PUBLIC paste service.

Cryptographic secrets (API keys, tokens, passwords) are redacted before
upload, but the following personal data is NOT redacted and will be public:
  • Your display name and persistent platform user ID
  • Verbatim content of your recent messages (prompts, responses, tool output)
  • Local filesystem paths
  • Any other PII present in the logs

The resulting URL is public to anyone who has the link. Pastes auto-delete
after 6 hours, but may be archived by third parties in the meantime.

If paste.rs is unreachable, uploads fall back to dpaste.com: those pastes
stay public for the --expire window (default: 1 day) and CANNOT be deleted
with ` + "`icebath debug delete`" + `.

Use --local to view the report without uploading.
`

// Gateway variant of the privacy notice the debug slash command prints before it uploads
const DebugGatewayPrivacyNotice = "⚠️ **Privacy notice:** This uploads system info + recent log tails " +
	"(may contain conversation fragments) to a public paste service. " +
	"Full logs are NOT included from the gateway — use `icebath debug share` " +
	"from the CLI for full log uploads.\n" +
	"Pastes auto-delete after 6 hours (dpaste.com fallback pastes: kept for " +
	"1 day, cannot be deleted)."

const debugNousPrivacyNotice = `⚠️  --nous: This uploads your debug bundle to Nous-INTERNAL storage (AWS S3),
    NOT a public paste service. The following is included:
  • System info (OS, Python/Icebath version, provider, which API keys are
    configured — NOT the actual keys)
  • Full agent.log, gateway.log, and desktop.log (up to 512 KB each — likely
    contains conversation content, tool outputs, and file paths), plus
    update.log and desktop-update-handoff.log when present (update/hand-off
    output — the root cause of update failures)

  • The bundle is viewable only by Nous staff (and allowlisted Discord mods)
    via a Google-login-gated viewer.
  • It is NOT a public paste — there is no public URL to the contents.
  • It auto-deletes after 14 days.
`

const debugUsage = `Usage: icebath debug <command>

Commands:
  share    Upload debug report to a paste service and print URL
  delete   Delete a previously uploaded paste

Options (share):
  --lines N    Number of log lines to include (default: 200)
  --expire N   dpaste.com fallback retention in days (default: 1)
  --local      Print report locally instead of uploading
  --nous       Upload to Nous-internal storage (private, staff-only,
               auto-deletes in 14 days) instead of a public paste
  --no-redact  Disable upload-time secret redaction (default: redact)

Options (delete):
  <url> ...    One or more paste URLs to delete`

// Pending paste deletion list of the home
func debugPendingFile(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "pastes", "pending.json")
}

// Pending paste entries holding a url and an expire_at, empty when the file is unreadable
func debugLoadPending(ctx context.Context) []*pyjson.Object {
	raw, err := os.ReadFile(debugPendingFile(ctx))
	if err != nil {
		return nil
	}
	text, err := pystr.DecodeUTF8Sig(raw)
	if err != nil {
		return nil
	}
	data, err := pyjson.Loads(text)
	if err != nil {
		return nil
	}
	list, ok := data.([]any)
	if !ok {
		return nil
	}
	out := []*pyjson.Object{}
	for _, item := range list {
		if e, ok := pyjson.AsObject(item); ok {
			_, hasURL := e.Vals["url"]
			_, hasExpire := e.Vals["expire_at"]
			if hasURL && hasExpire {
				out = append(out, e)
			}
		}
	}
	return out
}

// Writes the pending list, a failed write left for a manual debug delete
func debugSavePending(ctx context.Context, entries []*pyjson.Object) {
	list := make([]any, len(entries))
	for i, e := range entries {
		list[i] = e
	}
	_ = config.AtomicJSONWrite(ctx, debugPendingFile(ctx), list)
}

// Holds the cross process lock of the pending list read modify write
func debugPendingLock(ctx context.Context) (func(), error) {
	path := debugPendingFile(ctx)
	return provider.FileLock(path+".lock", 10*time.Second, fmt.Sprintf("Timed out waiting for the pending paste list lock (%s)", path))
}

// Float expire_at of a pending entry, false when it is malformed
func debugExpireAt(e *pyjson.Object) (float64, bool) {
	raw, has := e.Vals["expire_at"]
	if !has {
		raw = 0
	}
	v, err := pyint.Float(raw)
	return v, err == nil
}

func debugEntryURL(e *pyjson.Object) string {
	if raw, has := e.Vals["url"]; has {
		return pyrepr.Str(raw)
	}
	return ""
}

// Deletes pending pastes whose expiry passed, returning how many were reaped and how many remain
func debugSweepExpiredPastes(ctx context.Context, now *float64) (int, int, error) {
	entries := debugLoadPending(ctx)
	if len(entries) == 0 {
		return 0, 0, nil
	}
	current := debugNow()
	if now != nil {
		current = *now
	}
	reaped := map[string]bool{}
	for _, entry := range entries {
		expireAt, ok := debugExpireAt(entry)
		if !ok || expireAt > current {
			continue
		}
		gone, err := debugDeletePaste(ctx, debugEntryURL(entry))
		if err != nil {
			gone = false
		}
		if gone || expireAt+86400 <= current {
			reaped[debugEntryURL(entry)] = true
		}
	}
	release, err := debugPendingLock(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	latest := debugLoadPending(ctx)
	remaining := []*pyjson.Object{}
	for _, e := range latest {
		if debugKeepAfterSweep(e, reaped, current) {
			remaining = append(remaining, e)
		}
	}
	if len(remaining) != len(latest) {
		debugSavePending(ctx, remaining)
	}
	return len(reaped), len(remaining), nil
}

// Whether an entry survives a sweep, a url rescheduled during the sweep staying
func debugKeepAfterSweep(e *pyjson.Object, reaped map[string]bool, current float64) bool {
	expireAt, ok := debugExpireAt(e)
	if !ok {
		return false
	}
	return !(reaped[debugEntryURL(e)] && expireAt <= current)
}

// Pending paste cleanup that never lets the debug command fail
var debugBestEffortSweep = func(ctx context.Context) {
	defer func() { _ = recover() }()
	_, _, _ = debugSweepExpiredPastes(ctx, nil)
}

// Paste id of a paste.rs url, empty for any other service
func debugExtractPasteID(raw string) string {
	trimmed := strings.TrimRight(pystr.Strip(raw), "/")
	for _, prefix := range []string{"https://paste.rs/", "http://paste.rs/"} {
		if strings.HasPrefix(trimmed, prefix) {
			return trimmed[len(prefix):]
		}
	}
	return ""
}

// Whether a url is a dpaste.com paste, which has no unauthenticated delete
func debugIsDpasteURL(raw string) bool {
	s := pystr.Strip(raw)
	return strings.HasPrefix(s, "https://dpaste.com/") || strings.HasPrefix(s, "http://dpaste.com/")
}

// Response of a urllib urlopen call, an HTTPError for a non 2xx status and a URLError for a transport failure
func debugURLOpen(ctx context.Context, method, target string, body []byte, headers map[string]string, timeout time.Duration) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, 0, &pm.PyError{Class: "ValueError", Message: "unknown url type: " + pyrepr.StrRepr(target)}
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	proxy := httpproxy.FromEnvironment().ProxyFunc()
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: func(r *http.Request) (*url.URL, error) { return proxy(r.URL) }}}
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) && ue.Timeout() {
			return nil, 0, &pm.PyError{Class: "TimeoutError", Message: "timed out"}
		}
		return nil, 0, &pm.PyError{Class: "URLError", Message: "<urlopen error " + err.Error() + ">"}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		reason := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
		return nil, resp.StatusCode, &pm.PyError{Class: "HTTPError", Message: fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, reason)}
	}
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return data, resp.StatusCode, nil
}

// Deletes a paste.rs paste, the only service with an unauthenticated delete, true on success
var debugDeletePaste = func(ctx context.Context, raw string) (bool, error) {
	pasteID := debugExtractPasteID(raw)
	if pasteID == "" {
		if debugIsDpasteURL(raw) {
			return false, &pm.PyError{Class: "ValueError", Message: "Cannot delete: dpaste.com pastes cannot be deleted (anonymous " +
				"uploads have no owner token); they expire on their own.  Got: " + raw}
		}
		return false, &pm.PyError{Class: "ValueError", Message: "Cannot delete: only paste.rs URLs are supported.  Got: " + raw}
	}
	_, status, err := debugURLOpen(ctx, http.MethodDelete, debugPasteRSURL+pasteID, nil, map[string]string{"User-Agent": debugUserAgent}, 30*time.Second)
	if err != nil {
		return false, err
	}
	return status >= 200 && status < 300, nil
}

// Records paste.rs urls for deletion after the delay, merged into the pending list
var debugScheduleAutoDelete = func(ctx context.Context, urls []string, delaySeconds int) error {
	pasteRS := []string{}
	for _, u := range urls {
		if debugExtractPasteID(u) != "" {
			pasteRS = append(pasteRS, u)
		}
	}
	if len(pasteRS) == 0 {
		return nil
	}
	release, err := debugPendingLock(ctx)
	if err != nil {
		debugLog.Debug(fmt.Sprintf("could not schedule auto-delete for %s", pyrepr.Repr(pasteRS)), "err", err)
		return nil
	}
	defer release()
	order := []string{}
	byURL := map[string]float64{}
	for _, e := range debugLoadPending(ctx) {
		u := debugEntryURL(e)
		v, err := pyint.Float(e.Vals["expire_at"])
		if err != nil {
			return err
		}
		if _, seen := byURL[u]; !seen {
			order = append(order, u)
		}
		byURL[u] = v
	}
	expireAt := debugNow() + float64(delaySeconds)
	for _, u := range pasteRS {
		prev, seen := byURL[u]
		if !seen {
			order = append(order, u)
		}
		byURL[u] = max(expireAt, prev)
	}
	entries := make([]*pyjson.Object, len(order))
	for i, u := range order {
		entries[i] = pyjson.ObjectOf("url", u, "expire_at", byURL[u])
	}
	debugSavePending(ctx, entries)
	return nil
}

// Posts a body to a paste service and returns the paste url it echoes back
func debugPostPaste(ctx context.Context, service, endpoint string, body []byte, contentType string) (string, error) {
	data, _, err := debugURLOpen(ctx, http.MethodPost, endpoint, body,
		map[string]string{"Content-Type": contentType, "User-Agent": debugUserAgent}, 30*time.Second)
	if err != nil {
		return "", err
	}
	text, err := pystr.DecodeUTF8(data)
	if err != nil {
		return "", &pm.PyError{Class: "UnicodeDecodeError", Message: err.Error()}
	}
	got := pystr.Strip(text)
	if !strings.HasPrefix(got, "http") {
		return "", &pm.PyError{Class: "ValueError", Message: "Unexpected response from " + service + ": " + RedactDebugSupportTextCapped(got, 200)}
	}
	return got, nil
}

var debugUploadPasteRS = func(ctx context.Context, content string) (string, error) {
	return debugPostPaste(ctx, "paste.rs", debugPasteRSURL, []byte(content), "text/plain; charset=utf-8")
}

var debugUploadDpasteCom = func(ctx context.Context, content string, expiryDays int) (string, error) {
	boundary := "----IcebathDebugBoundary9f3c"
	var b strings.Builder
	for _, f := range [][2]string{{"content", content}, {"syntax", "text"}, {"expiry_days", strconv.Itoa(expiryDays)}} {
		fmt.Fprintf(&b, "--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n", boundary, f[0], f[1])
	}
	b.WriteString("--" + boundary + "--\r\n")
	return debugPostPaste(ctx, "dpaste.com", debugDpasteComURL, []byte(b.String()), "multipart/form-data; boundary="+boundary)
}

// Uploads content to paste.rs then dpaste.com, a RuntimeError naming every failure when both refuse
var debugUploadToPastebin = func(ctx context.Context, content string, expiryDays int) (string, error) {
	var failures []string
	for _, service := range []string{"paste.rs", "dpaste.com"} {
		var got string
		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("%v", r)
				}
			}()
			if service == "paste.rs" {
				got, err = debugUploadPasteRS(ctx, content)
			} else {
				got, err = debugUploadDpasteCom(ctx, content, expiryDays)
			}
		}()
		if err == nil {
			return got, nil
		}
		failures = append(failures, service+": "+err.Error())
	}
	return "", &pm.PyError{Class: "RuntimeError", Message: "Failed to upload to any paste service:\n  " + strings.Join(failures, "\n  ")}
}

// Single read snapshot of a log file, the summary tail and the full upload text
type LogSnapshot struct {
	Path     string
	TailText string
	FullText *string
}

// Where a log would live, empty for an unknown log name
func debugPrimaryLogPath(ctx context.Context, name string) string {
	path, _ := logging.LogFilePath(ctx, name)
	return path
}

var debugClientSideLogs = map[string]string{
	"desktop": "written by Icebath Desktop on the machine running the app, not by this " +
		"backend. If the desktop connects to a remote/docker/SSH backend, collect " +
		"it on that client machine",
}

// Explanation of a missing log instead of a bare absence
func debugMissingLogNote(ctx context.Context, name string) string {
	reason, ok := debugClientSideLogs[name]
	if !ok {
		return "(file not found)"
	}
	where := ""
	if primary := debugPrimaryLogPath(ctx, name); primary != "" {
		where = " — expected at " + primary
	}
	return "(not on this host: " + reason + where + ")"
}

// First nonempty candidate of a log, the primary then its first rotation
var debugResolveLogPath = func(ctx context.Context, name string) string {
	primary := debugPrimaryLogPath(ctx, name)
	if primary == "" {
		return ""
	}
	for _, candidate := range []string{primary, primary + ".1"} {
		if st, err := os.Stat(candidate); err == nil && st.Size() > 0 {
			return candidate
		}
	}
	return ""
}

// Bounded tail of a log, its file offset and whether it starts on a line boundary
func debugReadTailBytes(path string, size, maxBytes int64, tailLines int) ([]byte, int64, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer f.Close()
	if size <= maxBytes {
		data, err := io.ReadAll(f)
		return data, 0, true, err
	}
	chunkSize := int64(8192)
	pos := size
	var chunks [][]byte
	var total int64
	newlines := 0
	for pos > 0 && total < maxBytes*2 && (total < maxBytes || newlines <= tailLines+1) {
		readSize := min(chunkSize, pos)
		pos -= readSize
		chunk := make([]byte, readSize)
		n, err := f.ReadAt(chunk, pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, false, err
		}
		chunk = chunk[:n]
		chunks = append([][]byte{chunk}, chunks...)
		total += int64(len(chunk))
		newlines += bytes.Count(chunk, []byte("\n"))
		chunkSize = min(chunkSize*2, 65536)
	}
	onBoundary := true
	if pos > 0 {
		one := make([]byte, 1)
		n, err := f.ReadAt(one, pos-1)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, 0, false, err
		}
		onBoundary = n == 1 && one[0] == '\n'
	}
	return bytes.Join(chunks, nil), pos, onBoundary, nil
}

// At most the last maxBytes of text without a partial first line, and whether anything was cut
func debugTruncateCompleteLines(text string, maxBytes int) (string, bool) {
	raw := []byte(text)
	if len(raw) <= maxBytes {
		return text, false
	}
	cut := len(raw) - maxBytes
	onBoundary := raw[cut-1] == '\n'
	kept := raw[cut:]
	if !onBoundary {
		if i := bytes.IndexByte(kept, '\n'); i >= 0 {
			kept = kept[i+1:]
		} else {
			kept = nil
		}
	}
	return pystr.DecodeUTF8Replace(kept), true
}

// Captures a log once, deriving the summary tail and the full upload view from that one read
func debugCaptureLogSnapshot(ctx context.Context, name string, tailLines int, maxBytes int, redact bool) LogSnapshot {
	logPath := debugResolveLogPath(ctx, name)
	if logPath == "" {
		primary := debugPrimaryLogPath(ctx, name)
		tail := debugMissingLogNote(ctx, name)
		if primary != "" {
			if _, err := os.Stat(primary); err == nil {
				tail = "(file empty)"
			}
		}
		return LogSnapshot{TailText: tail}
	}
	snapshot, err := func() (snap LogSnapshot, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		st, err := os.Stat(logPath)
		if err != nil {
			return LogSnapshot{}, err
		}
		size := st.Size()
		if size == 0 {
			return LogSnapshot{Path: logPath, TailText: "(file empty)"}, nil
		}
		raw, startOffset, onBoundary, err := debugReadTailBytes(logPath, size, int64(maxBytes), tailLines)
		if err != nil {
			return LogSnapshot{}, err
		}
		allText := pystr.DecodeUTF8Replace(raw)
		if redact {
			allText = RedactDebugSupportText(allText)
		}
		if startOffset != 0 && !onBoundary {
			if i := strings.IndexByte(allText, '\n'); i >= 0 {
				allText = allText[i+1:]
			} else {
				allText = ""
			}
		}
		lines := pystr.Splitlines(allText, true)
		if tailLines < len(lines) {
			if tailLines > 0 {
				lines = lines[len(lines)-tailLines:]
			} else if tailLines < 0 {
				lines = lines[min(-tailLines, len(lines)):]
			}
		}
		tailText := strings.TrimRight(strings.Join(lines, ""), "\n")
		fullText, capped := debugTruncateCompleteLines(allText, maxBytes)
		if capped || size > int64(maxBytes) {
			fullText = fmt.Sprintf("[... truncated — showing last ~%dKB ...]\n%s", maxBytes/1024, fullText)
		}
		return LogSnapshot{Path: logPath, TailText: tailText, FullText: &fullText}, nil
	}()
	if err != nil {
		return LogSnapshot{Path: logPath, TailText: "(error reading: " + RedactDebugSupportText(debugErrText(err)) + ")"}
	}
	return snapshot
}

// str of an exception a Go error stands for
func debugErrText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pypath.OSErrorText(err)
	}
	return err.Error()
}

var (
	debugReportLogs = []string{"agent", "errors", "gateway", "gui", "desktop", "update", "handoff"}
	debugFullLogs   = []string{"agent", "gateway", "gui", "desktop", "update", "handoff"}
)

// Tail line budget of a log, the full lines budget for agent and at most 100 for the rest
func debugTailBudget(name string, logLines int) int {
	if name == "agent" {
		return logLines
	}
	return min(logLines, 100)
}

// Every report log captured exactly once
func debugCaptureDefaultLogSnapshots(ctx context.Context, logLines int, redact bool) map[string]LogSnapshot {
	out := map[string]LogSnapshot{}
	for _, name := range debugReportLogs {
		out[name] = debugCaptureLogSnapshot(ctx, name, debugTailBudget(name, logLines), debugMaxLogBytes, redact)
	}
	return out
}

var debugRunDump = RunDump

// Output of the dump command, force redacted unless redaction is off
var debugCaptureDump = func(ctx context.Context, redact bool) (string, error) {
	var capture bytes.Buffer
	if _, err := debugRunDump(ctx, home.Dir(ctx), map[string]any{"show_keys": false}, &capture); err != nil {
		return "", err
	}
	text := capture.String()
	if redact {
		return RedactDebugSupportText(text), nil
	}
	return text, nil
}

// Summary report of the dump and the log tails as upload ready text
var debugCollectDebugReport = func(ctx context.Context, logLines int, dumpText string, snapshots map[string]LogSnapshot) (string, error) {
	var buf strings.Builder
	if dumpText == "" {
		text, err := debugCaptureDump(ctx, true)
		if err != nil {
			return "", err
		}
		dumpText = text
	}
	buf.WriteString(dumpText)
	if snapshots == nil {
		snapshots = debugCaptureDefaultLogSnapshots(ctx, logLines, true)
	}
	func() {
		defer func() { _ = recover() }()
		stats := agent.GetSanitizerHealStats()
		if len(stats) == 0 {
			return
		}
		sessions := make([]string, 0, len(stats))
		for s := range stats {
			sessions = append(sessions, s)
		}
		sort.Strings(sessions)
		buf.WriteString("\n\n--- transcript sanitiser heal counters ---\n")
		for _, s := range sessions {
			st := stats[s]
			fmt.Fprintf(&buf, "session %s: %s heal events, %s messages healed, escalated=%s\n", s, pyrepr.Str(st["heal_events"]),
				pyrepr.Str(st["messages_healed"]), pyrepr.Str(st["escalated"]))
		}
	}()
	buf.WriteString("\n")
	for _, name := range debugReportLogs {
		snap, ok := snapshots[name]
		if !ok {
			continue
		}
		filename, known := logging.LogFiles[name]
		if !known {
			filename = name + ".log"
		}
		fmt.Fprintf(&buf, "\n--- %s (last %d lines) ---\n%s\n", filename, debugTailBudget(name, logLines), snap.TailText)
	}
	return buf.String(), nil
}

// Label to text bundle of the report and the full logs, the dump header and redaction banner prepended
type debugBundle struct {
	keys []string
	vals map[string]string
}

func (b *debugBundle) set(k, v string) {
	if _, ok := b.vals[k]; !ok {
		b.keys = append(b.keys, k)
	}
	b.vals[k] = v
}

// Debug report and full logs collected as a label to text mapping
var debugCollectShareBundle = func(ctx context.Context, logLines int, redact bool) (*debugBundle, error) {
	dumpText, err := debugCaptureDump(ctx, redact)
	if err != nil {
		return nil, err
	}
	snapshots := debugCaptureDefaultLogSnapshots(ctx, logLines, redact)
	report, err := debugCollectDebugReport(ctx, logLines, dumpText, snapshots)
	if err != nil {
		return nil, err
	}
	banner := ""
	if redact {
		banner = debugRedactionBanner
	}
	bundle := &debugBundle{vals: map[string]string{}}
	bundle.set("report", banner+report)
	for _, name := range debugFullLogs {
		if full := snapshots[name].FullText; full != nil && *full != "" {
			filename, known := logging.LogFiles[name]
			if !known {
				filename = name + ".log"
			}
			bundle.set(filename, banner+dumpText+"\n\n--- full "+filename+" ---\n"+*full)
		}
	}
	return bundle, nil
}

// Gzipped Nous envelope of a share bundle in the shape the support viewer parses
func debugBuildNousBundle(bundle *debugBundle, redact bool, now time.Time) ([]byte, error) {
	files := pyjson.NewObject()
	for _, k := range bundle.keys {
		files.Set(k, bundle.vals[k])
	}
	envelope := pyjson.ObjectOf("format", debugNousBundleFormat, "redacted", redact,
		"created", pydatetime.ISOFormatUTC(now), "files", files)
	text, err := pyjson.Dumps(envelope)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write([]byte(text)); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Outcome of a debug share upload, the label to url map, optional upload failures and the redaction flag
type DebugShareResult struct {
	URLKeys           []string
	URLs              map[string]string
	Failures          []string
	Redacted          bool
	AutoDeleteSeconds int
	Report            string
}

// Collects the report and full logs, uploads each and returns their urls, the report upload being required
func BuildDebugShare(ctx context.Context, logLines, expiry int, redact bool) (*DebugShareResult, error) {
	debugBestEffortSweep(ctx)
	bundle, err := debugCollectShareBundle(ctx, logLines, redact)
	if err != nil {
		return nil, err
	}
	if redact {
		debugLog.Info("icebath debug share: applied force-mode redaction to log snapshots before upload")
	}
	report := bundle.vals["report"]
	res := &DebugShareResult{URLs: map[string]string{}, Redacted: redact, AutoDeleteSeconds: debugAutoDeleteSeconds, Report: report}
	reportURL, err := debugUploadToPastebin(ctx, report, expiry)
	if err != nil {
		return nil, err
	}
	res.URLKeys, res.URLs["Report"] = append(res.URLKeys, "Report"), reportURL
	for _, label := range []string{"agent.log", "gateway.log", "gui.log", "desktop.log", "update.log", "desktop-update-handoff.log"} {
		content := bundle.vals[label]
		if content == "" {
			continue
		}
		u, err := debugUploadToPastebin(ctx, content, expiry)
		if err != nil {
			res.Failures = append(res.Failures, label+": "+RedactDebugSupportText(err.Error()))
			continue
		}
		res.URLKeys, res.URLs[label] = append(res.URLKeys, label), u
	}
	urls := make([]string, len(res.URLKeys))
	for i, k := range res.URLKeys {
		urls[i] = res.URLs[k]
	}
	if err := debugScheduleAutoDelete(ctx, urls, debugAutoDeleteSeconds); err != nil {
		return nil, err
	}
	return res, nil
}

// Upload gate, the yes flag proceeding unprompted and otherwise an interactive confirmation, exit 1 without a terminal
func debugConfirmUpload(ns map[string]any, w io.Writer) (bool, int) {
	if pyrepr.Truthy(ns["yes"]) {
		return true, 0
	}
	c := debugConsole(w)
	if !c.StdinIsTTY() {
		fmt.Fprintln(debugStderr, "ERROR: Non-interactive mode requires --yes to confirm upload.\n"+
			"       This prevents accidental exposure of personal data.\n"+
			"       Use --local to view the report without uploading.")
		return false, 1
	}
	answer, err := c.Input("Upload debug report? [y/N] ")
	if err != nil {
		answer = ""
	}
	answer = strings.ToLower(pystr.Strip(answer))
	if answer == "y" || answer == "yes" {
		return true, 0
	}
	fmt.Fprintln(w, "Aborted.")
	return false, 0
}

// Namespace integer with its parser default
func debugIntArg(ns map[string]any, key string, def int) (int, error) {
	raw, ok := ns[key]
	if !ok || raw == nil {
		return def, nil
	}
	n, err := pyint.Of(raw)
	if err != nil {
		return 0, err
	}
	return int(n.Int64()), nil
}

// Collects the debug report and full logs, uploads each and prints their urls
func runDebugShare(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	logLines, err := debugIntArg(ns, "lines", 200)
	if err != nil {
		return 1, err
	}
	expiry, err := debugIntArg(ns, "expire", 1)
	if err != nil {
		return 1, err
	}
	redact := !pyrepr.Truthy(ns["no_redact"])
	if pyrepr.Truthy(ns["local"]) {
		debugBestEffortSweep(ctx)
		fmt.Fprintln(w, "Collecting debug report...")
		bundle, err := debugCollectShareBundle(ctx, logLines, redact)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(w, bundle.vals["report"])
		for _, label := range []string{"agent.log", "gateway.log", "gui.log", "desktop.log", "update.log", "desktop-update-handoff.log"} {
			if body := bundle.vals[label]; body != "" {
				rule := strings.Repeat("=", 60)
				fmt.Fprintf(w, "\n\n%s\nFULL %s\n%s\n\n%s\n", rule, label, rule, body)
			}
		}
		return 0, nil
	}
	if pyrepr.Truthy(ns["nous"]) {
		return runDebugShareNous(ctx, ns, w, logLines, redact)
	}
	fmt.Fprint(w, debugPrivacyNotice+"\n")
	if ok, code := debugConfirmUpload(ns, w); !ok {
		return code, nil
	}
	fmt.Fprintln(w, "Collecting debug report...\nUploading...")
	result, err := BuildDebugShare(ctx, logLines, expiry, redact)
	if err != nil {
		var pe *pm.PyError
		if !errors.As(err, &pe) || pe.Class != "RuntimeError" {
			return 1, err
		}
		fmt.Fprintln(debugStderr, "\nUpload failed: "+RedactDebugSupportText(err.Error()))
		fmt.Fprint(w, "\nRun `icebath debug share --local` to print the report instead.\n\n")
		return 1, nil
	}
	width := 0
	for _, k := range result.URLKeys {
		width = max(width, len([]rune(k)))
	}
	fmt.Fprintln(w, "\nDebug report uploaded:")
	for _, k := range result.URLKeys {
		fmt.Fprintf(w, "  %s  %s\n", pystr.Ljust(k, width), result.URLs[k])
	}
	if len(result.Failures) > 0 {
		fmt.Fprintf(w, "\n  (failed to upload: %s)\n", strings.Join(result.Failures, ", "))
	}
	dpaste := 0
	for _, k := range result.URLKeys {
		if debugIsDpasteURL(result.URLs[k]) {
			dpaste++
		}
	}
	if dpaste > 0 {
		fmt.Fprintf(w, "\n⏱  paste.rs pastes will auto-delete in %d hours.\n", result.AutoDeleteSeconds/3600)
		fmt.Fprintf(w, "⚠️  %d of %d upload(s) fell back to dpaste.com: those pastes stay public for %d day(s) and CANNOT be "+
			"deleted with `icebath debug delete`.\n\nShare these links with the Icebath team for support.\n", dpaste, len(result.URLKeys), expiry)
	} else {
		fmt.Fprintf(w, "\n⏱  Pastes will auto-delete in %d hours.\nTo delete now:  icebath debug delete <url>\n"+
			"\nShare these links with the Icebath team for support.\n", result.AutoDeleteSeconds/3600)
	}
	return 0, nil
}

// Uploads the gzipped bundle envelope to Nous internal storage and prints its private view link
func runDebugShareNous(ctx context.Context, ns map[string]any, w io.Writer, logLines int, redact bool) (int, error) {
	fmt.Fprint(w, debugNousPrivacyNotice+"\n")
	if ok, code := debugConfirmUpload(ns, w); !ok {
		return code, nil
	}
	if !redact {
		fmt.Fprint(w, "⚠️  --no-redact is set: secrets in your logs will NOT be redacted before upload.\n\n")
	}
	fmt.Fprintln(w, "Collecting debug report...")
	debugBestEffortSweep(ctx)
	bundle, err := debugCollectShareBundle(ctx, logLines, redact)
	if err != nil {
		return 1, err
	}
	if redact {
		debugLog.Info("icebath debug share --nous: applied force-mode redaction before upload")
	}
	fmt.Fprintln(w, "Uploading to Nous diagnostics storage...")
	res, err := func() (out *pyjson.Object, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("%v", r)
			}
		}()
		data, err := debugBuildNousBundle(bundle, redact, time.Now())
		if err != nil {
			return nil, err
		}
		return debugShareToNous(ctx, data)
	}()
	if err != nil {
		cause := pm.EdgeBlock(err)
		if cause == "" {
			cause = "The Nous diagnostics service may be unavailable or not yet provisioned"
		}
		fmt.Fprintf(debugStderr, "\nNous upload failed: %s\n\n%s.\nRun `icebath debug share --local` to print the report instead, "+
			"or `icebath debug share` to upload to a public paste service.\n\n", RedactDebugSupportText(err.Error()), cause)
		return 1, nil
	}
	viewURL := statusOr(res.Vals["viewUrl"], res.Vals["view_url"])
	expiresAt := statusOr(res.Vals["expiresAt"], res.Vals["expires_at"])
	fmt.Fprintln(w, "\nDebug bundle uploaded to Nous (private):")
	if pyrepr.Truthy(viewURL) {
		fmt.Fprintln(w, "  View URL  "+pyrepr.Str(viewURL))
	} else {
		id, has := res.Vals["id"]
		if !has {
			id = "?"
		}
		fmt.Fprintln(w, "  (no view URL returned; upload id: "+pyrepr.Str(id)+")")
	}
	if pyrepr.Truthy(expiresAt) {
		fmt.Fprintln(w, "\n⏱  Auto-deletes at "+pyrepr.Str(expiresAt)+" (14-day retention).")
	} else {
		fmt.Fprintln(w, "\n⏱  Auto-deletes after 14 days.")
	}
	fmt.Fprintln(w, "\nShare this private link with the Nous team — only Nous staff "+
		"(via Google login) can open it.\n"+
		"\nPick up the discussion in:\n"+
		"  GitHub Issues        https://github.com/NousResearch/hermes-agent/issues\n"+
		"  Nous Portal Support  https://portal.nousresearch.com/help\n"+
		"  Discord              https://discord.gg/NousResearch")
	return 0, nil
}

// Deletes the paste urls a debug share uploaded
func runDebugDelete(ctx context.Context, ns map[string]any, w io.Writer) (int, error) {
	var urls []string
	switch v := ns["urls"].(type) {
	case []string:
		urls = v
	case []any:
		for _, u := range v {
			urls = append(urls, pyrepr.Str(u))
		}
	}
	if len(urls) == 0 {
		fmt.Fprintln(w, "Usage: icebath debug delete <url> [<url> ...]\n"+
			"  Deletes paste.rs pastes uploaded by 'icebath debug share'.")
		return 0, nil
	}
	for _, u := range urls {
		gone, err := debugDeletePaste(ctx, u)
		var pe *pm.PyError
		switch {
		case err == nil && gone:
			fmt.Fprintln(w, "  ✓ Deleted: "+u)
		case err == nil:
			fmt.Fprintln(w, "  ✗ Failed to delete: "+RedactDebugSupportText(u)+" (unexpected response)")
		case errors.As(err, &pe) && pe.Class == "ValueError":
			fmt.Fprintln(w, "  ✗ "+RedactDebugSupportText(err.Error()))
		default:
			fmt.Fprintln(w, "  ✗ Could not delete "+RedactDebugSupportText(u)+": "+RedactDebugSupportText(err.Error()))
		}
	}
	return 0, nil
}

// Routes the debug subcommands, sweeping expired pastes on every call
func RunDebug(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	if h != "" && h != home.Dir(ctx) {
		ctx = home.WithOverride(ctx, h)
	}
	debugBestEffortSweep(ctx)
	switch ns["debug_command"] {
	case "share":
		return runDebugShare(ctx, ns, w)
	case "delete":
		return runDebugDelete(ctx, ns, w)
	}
	fmt.Fprintln(w, debugUsage)
	return 0, nil
}
