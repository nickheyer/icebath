package selfupdate

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
)

const dbgRedactToken = "sk-proj-A1B2C3D4E5F6G7H8I9J0aA"

// Isolated home holding the five sample logs, every debug seam restored after the test
func dbgHome(t *testing.T) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	logs := filepath.Join(h, "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", h)
	for name, body := range map[string]string{
		"agent.log": "2026-04-12 17:00:00 INFO agent: session started\n2026-04-12 17:00:01 INFO tools.terminal: running ls\n" +
			"2026-04-12 17:00:02 WARNING agent: high token usage\n",
		"errors.log":  "2026-04-12 17:00:05 ERROR gateway.run: connection lost\n",
		"gateway.log": "2026-04-12 17:00:10 INFO gateway.run: started\n",
		"gui.log":     "2026-04-12 17:00:12 INFO hermes_cli.web_server: dashboard request\n",
		"desktop.log": "2026-04-12 17:00:15 INFO desktop: backend spawned\n",
	} {
		dbgWrite(t, filepath.Join(logs, name), body)
	}
	dbgRestoreSeams(t)
	return h
}

// Home whose agent log carries a vendor token, with empty errors and gateway logs
func dbgHomeWithSecret(t *testing.T, gatewayLine string) string {
	t.Helper()
	h := filepath.Join(t.TempDir(), ".icebath")
	t.Setenv("ICEBATH_HOME", h)
	os.Unsetenv("ICEBATH_REDACT_SECRETS")
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "2026-04-12 17:00:00 INFO config: api_key="+dbgRedactToken+" loaded\n")
	dbgWrite(t, filepath.Join(h, "logs", "errors.log"), "")
	dbgWrite(t, filepath.Join(h, "logs", "gateway.log"), gatewayLine)
	dbgRestoreSeams(t)
	return h
}

func dbgWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func dbgRestoreSeams(t *testing.T) {
	t.Helper()
	pasteRS, dpaste, stderr, console, now := debugPasteRSURL, debugDpasteComURL, debugStderr, debugConsole, debugNow
	sweep, del, sched, upRS, upDp, upload := debugBestEffortSweep, debugDeletePaste, debugScheduleAutoDelete, debugUploadPasteRS, debugUploadDpasteCom, debugUploadToPastebin
	resolve, dump, report, bundle, nous, runDump := debugResolveLogPath, debugCaptureDump, debugCollectDebugReport, debugCollectShareBundle, debugShareToNous, debugRunDump
	t.Cleanup(func() {
		debugPasteRSURL, debugDpasteComURL, debugStderr, debugConsole, debugNow = pasteRS, dpaste, stderr, console, now
		debugBestEffortSweep, debugDeletePaste, debugScheduleAutoDelete, debugUploadPasteRS, debugUploadDpasteCom, debugUploadToPastebin = sweep, del, sched, upRS, upDp, upload
		debugResolveLogPath, debugCaptureDump, debugCollectDebugReport, debugCollectShareBundle, debugShareToNous, debugRunDump = resolve, dump, report, bundle, nous, runDump
	})
}

// Replaces the dump with one printing the given text
func dbgPinDump(text string) {
	debugRunDump = func(_ context.Context, _ string, _ map[string]any, w io.Writer) (int, error) {
		if text != "" {
			fmt.Fprintln(w, text)
		}
		return 0, nil
	}
}

// Records every upload and answers with a numbered paste.rs url
func dbgRecordUploads() *[]string {
	var mu sync.Mutex
	captured := &[]string{}
	debugUploadToPastebin = func(_ context.Context, content string, _ int) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		*captured = append(*captured, content)
		return fmt.Sprintf("https://paste.rs/paste%d", len(*captured)), nil
	}
	return captured
}

func dbgNoSweep() { debugBestEffortSweep = func(context.Context) {} }

func dbgRun(t *testing.T, ns map[string]any) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	debugStderr = &errOut
	code, err := runDebugShare(context.Background(), ns, &out)
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), errOut.String(), code
}

func TestUploadToPastebinFallsBackToDpasteCom(t *testing.T) {
	dbgRestoreSeams(t)
	calls := 0
	debugUploadPasteRS = func(context.Context, string) (string, error) { return "", errors.New("down") }
	debugUploadDpasteCom = func(context.Context, string, int) (string, error) { calls++; return "https://dpaste.com/TEST", nil }
	got, err := debugUploadToPastebin(context.Background(), "content", 1)
	if err != nil || got != "https://dpaste.com/TEST" || calls != 1 {
		t.Fatal(got, err, calls)
	}
}

func TestUploadToPastebinRaisesWhenBothFail(t *testing.T) {
	dbgRestoreSeams(t)
	debugUploadPasteRS = func(context.Context, string) (string, error) { return "", errors.New("err1") }
	debugUploadDpasteCom = func(context.Context, string, int) (string, error) { return "", errors.New("err2") }
	if _, err := debugUploadToPastebin(context.Background(), "content", 1); err == nil || !strings.Contains(err.Error(), "Failed to upload") {
		t.Fatal(err)
	}
}

func TestCaptureLogSnapshotRaceTruncateAfterResolveReportsEmpty(t *testing.T) {
	h := dbgHome(t)
	logPath := filepath.Join(h, "logs", "agent.log")
	debugResolveLogPath = func(context.Context, string) string { return logPath }
	dbgWrite(t, logPath, "")
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, true)
	if snap.Path != logPath || snap.FullText != nil || snap.TailText != "(file empty)" {
		t.Fatalf("%+v", snap)
	}
}

func TestCaptureLogSnapshotKeepsFirstLineWhenTruncationOnBoundary(t *testing.T) {
	h := dbgHome(t)
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), strings.Repeat(strings.Repeat("A", 99)+"\n", 200))
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 5, 1000, true)
	if snap.FullText == nil || !strings.Contains(*snap.FullText, "truncated") {
		t.Fatalf("%+v", snap)
	}
	raw := strings.SplitN(*snap.FullText, "\n", 2)[1]
	kept := 0
	for _, l := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.HasPrefix(l, "A") {
			kept++
		}
	}
	if kept != 10 {
		t.Fatal(kept)
	}
}

func TestMissingLogNoteBackendWrittenLogReportsPlainAbsence(t *testing.T) {
	h := dbgHome(t)
	os.Remove(filepath.Join(h, "logs", "agent.log"))
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, true)
	if snap.FullText != nil || snap.TailText != "(file not found)" {
		t.Fatalf("%+v", snap)
	}
}

func TestMissingLogNoteClientWrittenLogNamesItsWriterAndPath(t *testing.T) {
	h := dbgHome(t)
	os.Remove(filepath.Join(h, "logs", "desktop.log"))
	snap := debugCaptureLogSnapshot(context.Background(), "desktop", 10, debugMaxLogBytes, true)
	if snap.FullText != nil || !strings.Contains(snap.TailText, "not on this host") ||
		!strings.Contains(snap.TailText, filepath.Join(h, "logs", "desktop.log")) {
		t.Fatalf("%+v", snap)
	}
}

func TestMissingLogNotePresentClientLogIsCapturedNormally(t *testing.T) {
	dbgHome(t)
	snap := debugCaptureLogSnapshot(context.Background(), "desktop", 10, debugMaxLogBytes, true)
	if !strings.Contains(snap.TailText, "backend spawned") || strings.Contains(snap.TailText, "not on this host") {
		t.Fatalf("%+v", snap)
	}
}

func TestMissingLogNoteEmptyClientLogIsEmptyNotAbsent(t *testing.T) {
	h := dbgHome(t)
	dbgWrite(t, filepath.Join(h, "logs", "desktop.log"), "")
	if snap := debugCaptureLogSnapshot(context.Background(), "desktop", 10, debugMaxLogBytes, true); snap.TailText != "(file empty)" {
		t.Fatalf("%+v", snap)
	}
}

func TestMissingLogNoteReportCarriesTheNoteForARemoteBackend(t *testing.T) {
	h := dbgHome(t)
	os.Remove(filepath.Join(h, "logs", "desktop.log"))
	report, err := debugCollectDebugReport(context.Background(), 10, "dump\n", nil)
	if err != nil || !strings.Contains(report, "--- desktop.log") || !strings.Contains(report, "not on this host") {
		t.Fatal(report, err)
	}
}

func TestCaptureLogSnapshotRedactionDefaultRedactsTailAndFullText(t *testing.T) {
	dbgHomeWithSecret(t, "")
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, true)
	if strings.Contains(snap.TailText, dbgRedactToken) || snap.FullText == nil || strings.Contains(*snap.FullText, dbgRedactToken) {
		t.Fatalf("%+v", snap)
	}
}

func TestCaptureLogSnapshotRedactionRedactFalsePassesThrough(t *testing.T) {
	dbgHomeWithSecret(t, "")
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, false)
	if !strings.Contains(snap.TailText, dbgRedactToken) || snap.FullText == nil || !strings.Contains(*snap.FullText, dbgRedactToken) {
		t.Fatalf("%+v", snap)
	}
}

func TestCaptureLogSnapshotRedactionForceTrueWorksWhenRedactionDisabled(t *testing.T) {
	dbgHomeWithSecret(t, "")
	t.Setenv("ICEBATH_REDACT_SECRETS", "false")
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, true)
	if strings.Contains(snap.TailText, dbgRedactToken) || snap.FullText == nil || strings.Contains(*snap.FullText, dbgRedactToken) {
		t.Fatalf("%+v", snap)
	}
}

func TestCaptureLogSnapshotRedactionDefaultRedactsEmailAddressesForPublicShare(t *testing.T) {
	h := dbgHomeWithSecret(t, "")
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "2026-04-12 17:00:00 INFO gateway.run: inbound message: platform=bluebubbles "+
		"user=person@example.com chat=iMessage;-;person@example.com msg='hello'\n")
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 10, debugMaxLogBytes, true)
	if strings.Contains(snap.TailText, "person@example.com") || !strings.Contains(snap.TailText, "[REDACTED_EMAIL]") ||
		snap.FullText == nil || strings.Contains(*snap.FullText, "person@example.com") {
		t.Fatalf("%+v", snap)
	}
}

func TestCaptureLogSnapshotRedactionCaptureDefaultLogSnapshotsThreadsRedact(t *testing.T) {
	dbgHomeWithSecret(t, "")
	snaps := debugCaptureDefaultLogSnapshots(context.Background(), 50, true)
	full := ""
	if snaps["agent"].FullText != nil {
		full = *snaps["agent"].FullText
	}
	if strings.Contains(snaps["agent"].TailText, dbgRedactToken) || strings.Contains(full, dbgRedactToken) {
		t.Fatalf("%+v", snaps["agent"])
	}
}

func TestCaptureLogSnapshotRedactionRedactsCompleteLogLinesBeforeApplyingByteLimits(t *testing.T) {
	h := dbgHomeWithSecret(t, "")
	maxBytes, rawWindow := 512, 8192
	tailCanary, fullCanary := "r36_REDACTED_tail_boundary_canary_012345", "r36_REDACTED_full_boundary_canary_678901"
	payload := bytes.Repeat([]byte("x"), 12_000)
	for _, pair := range []struct {
		boundary int
		canary   string
	}{{len(payload) - rawWindow, tailCanary}, {len(payload) - maxBytes, fullCanary}} {
		marker := []byte("accessToken=" + pair.canary)
		start := pair.boundary - len("accessToken=")
		copy(payload[start:start+len(marker)], marker)
	}
	if err := os.WriteFile(filepath.Join(h, "logs", "agent.log"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := debugCaptureLogSnapshot(context.Background(), "agent", 1, maxBytes, true)
	if strings.Contains(snap.TailText, tailCanary) || snap.FullText == nil || strings.Contains(*snap.FullText, fullCanary) {
		t.Fatalf("%+v", snap)
	}
}

// Share namespace with a truthy yes and no_redact the way an unset MagicMock attribute reads
func dbgMagicArgs(over map[string]any) map[string]any {
	ns := map[string]any{"lines": 50, "expire": 7, "local": false, "nous": false, "yes": true, "no_redact": true}
	for k, v := range over {
		ns[k] = v
	}
	return ns
}

func TestRunDebugShareShareUploadsFivePastes(t *testing.T) {
	dbgHome(t)
	dbgPinDump("--- icebath dump ---\nversion: test\n--- end dump ---")
	captured := dbgRecordUploads()
	out, _, _ := dbgRun(t, dbgMagicArgs(nil))
	if len(*captured) != 5 {
		t.Fatal(len(*captured))
	}
	for _, want := range []string{"paste.rs/paste1", "paste.rs/paste2", "paste.rs/paste3", "paste.rs/paste4", "paste.rs/paste5",
		"Report", "agent.log", "gateway.log", "gui.log", "desktop.log"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	for i, name := range []string{"agent.log", "gateway.log", "gui.log", "desktop.log"} {
		paste := (*captured)[i+1]
		if !strings.Contains(paste, "--- icebath dump ---") || !strings.Contains(paste, "--- full "+name+" ---") {
			t.Fatalf("%s paste %q", name, paste)
		}
	}
}

func TestRunDebugShareRedactionDefaultShareRedactsUploadedContent(t *testing.T) {
	dbgHomeWithSecret(t, "2026-04-12 17:00:01 INFO gateway.run: token "+dbgRedactToken+"\n")
	dbgPinDump("")
	dbgNoSweep()
	captured := dbgRecordUploads()
	dbgRun(t, dbgMagicArgs(map[string]any{"no_redact": false}))
	if len(*captured) < 2 {
		t.Fatal(len(*captured))
	}
	for _, c := range *captured {
		if strings.Contains(c, dbgRedactToken) {
			t.Fatal("raw token leaked into upload-bound content")
		}
	}
}

// Writes a config holding one fallback provider with the given extra yaml lines
func dbgFallbackConfig(t *testing.T, h string, lines ...string) {
	t.Helper()
	dbgWrite(t, filepath.Join(h, "config.yaml"), "fallback_providers:\n  - provider: custom\n    model: backup-model\n"+strings.Join(lines, ""))
}

// The dump text, the share bundle values and the report over a fresh dump
func dbgUploadTexts(t *testing.T, withRawDump bool) []string {
	t.Helper()
	ctx := context.Background()
	var texts []string
	if withRawDump {
		raw, err := debugCaptureDump(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, raw)
	}
	bundle, err := debugCollectShareBundle(ctx, 20, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range bundle.keys {
		texts = append(texts, bundle.vals[k])
	}
	dump, err := debugCaptureDump(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	report, err := debugCollectDebugReport(ctx, 20, dump, nil)
	if err != nil {
		t.Fatal(err)
	}
	return append(texts, report)
}

func dbgLeaks(texts []string, secret string) bool {
	for _, text := range texts {
		if strings.Contains(text, secret) {
			return true
		}
	}
	return false
}

func TestRunDebugShareRedactionFallbackProviderKeyNeverReachesUploadBoundContent(t *testing.T) {
	h := dbgHomeWithSecret(t, "")
	key := "fbk-opaque-0123456789abcdefghij"
	dbgFallbackConfig(t, h, "    base_url: https://backup.example/v1\n", "    api_key: "+key+"\n")
	texts := dbgUploadTexts(t, false)
	if !dbgLeaks(texts, "backup-model") || dbgLeaks(texts, key) {
		t.Fatal(texts)
	}
}

func TestRunDebugShareRedactionFallbackAPIKeyMaskedWhateverItsYamlShape(t *testing.T) {
	for _, c := range [][2]string{{"87419362508741936250", "87419362508741936250"},
		{"'opaque***Fallback0123456789'", "opaque***Fallback0123456789"},
		{"{value: nestedOpaque0123456789abcdef}", "nestedOpaque0123456789abcdef"}} {
		t.Run(c[1], func(t *testing.T) {
			h := dbgHomeWithSecret(t, "")
			dbgFallbackConfig(t, h, "    api_key: "+c[0]+"\n")
			texts := dbgUploadTexts(t, true)
			if !strings.Contains(texts[0], "backup-model") || dbgLeaks(texts, c[1]) {
				t.Fatal(texts)
			}
		})
	}
}

func TestRunDebugShareRedactionFallbackSecretFieldsMaskedByRepoPolicy(t *testing.T) {
	for _, c := range [][3]string{{"token", "87419362508741936250", "87419362508741936250"},
		{"auth_token", "'opaque***AuthToken0123456789'", "opaque***AuthToken0123456789"},
		{"client_secret", "{value: clientSecretOpaque0123456789}", "clientSecretOpaque0123456789"},
		{"password", "98127364509812736450", "98127364509812736450"},
		{"clientApiKey", "'opaque***CamelKey0123456789'", "opaque***CamelKey0123456789"}} {
		t.Run(c[0], func(t *testing.T) {
			h := dbgHomeWithSecret(t, "")
			dbgFallbackConfig(t, h, "    key_env: BACKUP_KEY_ENV_NAME\n", "    api_key_env: BACKUP_API_KEY_ENV_NAME\n",
				"    max_tokens: 4321\n", "    "+c[0]+": "+c[1]+"\n")
			dbgNoSweep()
			debugScheduleAutoDelete = func(context.Context, []string, int) error { return nil }
			uploaded := dbgRecordUploads()
			dbgRun(t, map[string]any{"lines": 20, "expire": 1, "local": false, "nous": false, "no_redact": false, "yes": true})
			if !dbgLeaks(*uploaded, "backup-model") || dbgLeaks(*uploaded, c[2]) {
				t.Fatal(*uploaded)
			}
			raw, err := debugCaptureDump(context.Background(), false)
			if err != nil {
				t.Fatal(err)
			}
			safe, _ := debugCaptureDump(context.Background(), true)
			report, err := debugCollectDebugReport(context.Background(), 20, safe, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{"backup-model", "BACKUP_KEY_ENV_NAME", "BACKUP_API_KEY_ENV_NAME", "4321"} {
				if !strings.Contains(raw, s) {
					t.Fatalf("dump lost %q:\n%s", s, raw)
				}
			}
			if dbgLeaks([]string{raw, report}, c[2]) {
				t.Fatal(raw, report)
			}
		})
	}
}

func TestRunDebugShareRedactionFallbackBaseURLCredentialNeverReachesUploadBoundContent(t *testing.T) {
	secret := "urlCredOpaque0123456789abcdef"
	for _, base := range []string{"https://user:%s@backup.example/v1", "https://backup.example/v1?key=%s",
		"https://backup.example/v1?X-Amz-Signature=%s", "https://backup.example/v1?X-Goog-Signature=%s",
		"https://backup.example/v1?sv=2024-11-04&sig=%s", "https://backup.example/v1#access_token=%s&view=public"} {
		t.Run(base, func(t *testing.T) {
			h := dbgHomeWithSecret(t, "")
			dbgFallbackConfig(t, h, "    base_url: '"+fmt.Sprintf(base, secret)+"'\n")
			texts := dbgUploadTexts(t, true)
			for _, text := range texts[:2] {
				if !strings.Contains(text, "backup-model") || !strings.Contains(text, "backup.example") {
					t.Fatal(text)
				}
			}
			if dbgLeaks(texts, secret) {
				t.Fatal(texts)
			}
		})
	}
}

func TestRunDebugShareRedactionCaptureDumpIsIndependentStrictRedactionBoundary(t *testing.T) {
	dbgRestoreSeams(t)
	key, password, signature := "opaqueBoundaryKeyABC123456789", "boundaryPasswordABC123456789", "boundarySignatureABC123456789"
	dbgPinDump("{'api_key': '" + key + "', 'base_url': 'https://user:" + password + "@example.test/v1?X-Amz-Signature=" + signature + "'}")
	safe, _ := debugCaptureDump(context.Background(), true)
	raw, _ := debugCaptureDump(context.Background(), false)
	for _, s := range []string{key, password, signature} {
		if strings.Contains(safe, s) || !strings.Contains(raw, s) {
			t.Fatalf("%s\nsafe=%s\nraw=%s", s, safe, raw)
		}
	}
}

func TestRunDebugShareRedactionDefaultShareIncludesRedactionBanner(t *testing.T) {
	dbgHomeWithSecret(t, "2026-04-12 17:00:01 INFO gateway.run: token "+dbgRedactToken+"\n")
	dbgPinDump("")
	dbgNoSweep()
	captured := dbgRecordUploads()
	dbgRun(t, dbgMagicArgs(map[string]any{"no_redact": false}))
	for _, c := range *captured {
		if !strings.Contains(c, "redacted at upload time") {
			t.Fatal("redaction banner missing from upload-bound content")
		}
	}
}

func TestRunDebugShareRedactionNoRedactFlagDisablesRedactionAndBanner(t *testing.T) {
	dbgHomeWithSecret(t, "2026-04-12 17:00:01 INFO gateway.run: token "+dbgRedactToken+"\n")
	dbgPinDump("")
	dbgNoSweep()
	captured := dbgRecordUploads()
	dbgRun(t, dbgMagicArgs(map[string]any{"no_redact": true}))
	if !dbgLeaks(*captured, dbgRedactToken) {
		t.Fatal("expected raw token in --no-redact upload")
	}
	if dbgLeaks(*captured, "redacted at upload time") {
		t.Fatal("banner present with --no-redact")
	}
}

func TestDeletePasteDeleteSendsDeleteRequest(t *testing.T) {
	dbgRestoreSeams(t)
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { method, path = r.Method, r.URL.Path }))
	defer srv.Close()
	debugPasteRSURL = srv.URL + "/"
	ok, err := debugDeletePaste(context.Background(), "https://paste.rs/abc123")
	if err != nil || !ok || method != http.MethodDelete || path != "/abc123" {
		t.Fatal(ok, err, method, path)
	}
}

func TestDeletePasteDpasteURLErrorExplainsNoDelete(t *testing.T) {
	dbgRestoreSeams(t)
	_, err := debugDeletePaste(context.Background(), "https://dpaste.com/ABC123")
	if err == nil || !strings.Contains(err.Error(), "dpaste.com pastes cannot be deleted") {
		t.Fatal(err)
	}
}

func dbgPending(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(debugPendingFile(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScheduleAutoDeleteRecordsPendingToJSON(t *testing.T) {
	dbgHome(t)
	if err := debugScheduleAutoDelete(context.Background(), []string{"https://paste.rs/abc", "https://paste.rs/def"}, 10); err != nil {
		t.Fatal(err)
	}
	entries := dbgPending(t)
	if len(entries) != 2 || entries[0]["url"] != "https://paste.rs/abc" || entries[1]["url"] != "https://paste.rs/def" {
		t.Fatal(entries)
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for _, e := range entries {
		if at := e["expire_at"].(float64); at <= now || at > now+15 {
			t.Fatal(e)
		}
	}
}

func TestScheduleAutoDeleteDedupesSameURL(t *testing.T) {
	dbgHome(t)
	ctx := context.Background()
	debugScheduleAutoDelete(ctx, []string{"https://paste.rs/dup"}, 10)
	debugScheduleAutoDelete(ctx, []string{"https://paste.rs/dup"}, 100)
	if entries := debugLoadPending(ctx); len(entries) != 1 || entries[0].Vals["url"] != "https://paste.rs/dup" {
		t.Fatal(entries)
	}
}

func dbgSavePending(t *testing.T, rows ...[2]any) {
	t.Helper()
	entries := []*pyjson.Object{}
	for _, r := range rows {
		entries = append(entries, pyjson.ObjectOf("url", r[0], "expire_at", r[1]))
	}
	debugSavePending(context.Background(), entries)
}

func dbgUnix() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func TestSweepExpiredPastesSweepDeletesExpiredEntries(t *testing.T) {
	dbgHome(t)
	dbgSavePending(t, [2]any{"https://paste.rs/expired", dbgUnix() - 100}, [2]any{"https://paste.rs/future", dbgUnix() + 3600})
	var calls []string
	debugDeletePaste = func(_ context.Context, u string) (bool, error) { calls = append(calls, u); return true, nil }
	deleted, remaining, err := debugSweepExpiredPastes(context.Background(), nil)
	if err != nil || deleted != 1 || remaining != 1 || len(calls) != 1 || calls[0] != "https://paste.rs/expired" {
		t.Fatal(deleted, remaining, err, calls)
	}
	if entries := debugLoadPending(context.Background()); len(entries) != 1 || entries[0].Vals["url"] != "https://paste.rs/future" {
		t.Fatal(entries)
	}
}

func TestSweepExpiredPastesSweepLeavesFutureEntriesAlone(t *testing.T) {
	dbgHome(t)
	dbgSavePending(t, [2]any{"https://paste.rs/future1", dbgUnix() + 3600}, [2]any{"https://paste.rs/future2", dbgUnix() + 7200})
	debugDeletePaste = func(context.Context, string) (bool, error) { t.Fatal("delete called"); return false, nil }
	if deleted, remaining, err := debugSweepExpiredPastes(context.Background(), nil); err != nil || deleted != 0 || remaining != 2 {
		t.Fatal(deleted, remaining, err)
	}
}

func TestSweepExpiredPastesSweepSurvivesNetworkFailure(t *testing.T) {
	dbgHome(t)
	dbgSavePending(t, [2]any{"https://paste.rs/flaky", dbgUnix() - 100})
	debugDeletePaste = func(context.Context, string) (bool, error) { return false, errors.New("network down") }
	deleted, remaining, err := debugSweepExpiredPastes(context.Background(), nil)
	if err != nil || deleted != 0 || remaining != 1 || len(debugLoadPending(context.Background())) != 1 {
		t.Fatal(deleted, remaining, err)
	}
}

func TestSweepExpiredPastesShareScheduledDuringSweepDeletesIsKept(t *testing.T) {
	dbgHome(t)
	dbgSavePending(t, [2]any{"https://paste.rs/old", dbgUnix() - 100})
	inDelete, shareDone := make(chan struct{}), make(chan struct{})
	debugDeletePaste = func(context.Context, string) (bool, error) {
		close(inDelete)
		select {
		case <-shareDone:
		case <-time.After(10 * time.Second):
			t.Error("share never finished")
		}
		return true, nil
	}
	type res struct{ deleted, remaining int }
	result := make(chan res, 1)
	go func() {
		d, r, _ := debugSweepExpiredPastes(context.Background(), nil)
		result <- res{d, r}
	}()
	<-inDelete
	debugScheduleAutoDelete(context.Background(), []string{"https://paste.rs/new"}, debugAutoDeleteSeconds)
	close(shareDone)
	if got := <-result; got != (res{1, 1}) {
		t.Fatal(got)
	}
	if entries := debugLoadPending(context.Background()); len(entries) != 1 || entries[0].Vals["url"] != "https://paste.rs/new" {
		t.Fatal(entries)
	}
}

func TestBuildDebugShareRedactionKeepsSecretsOutOfPayload(t *testing.T) {
	h := dbgHome(t)
	secret := "sk-proj-SUPERSECRETtoken1234567890"
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "line one\nauthorization token="+secret+"\nline three\n")
	dbgPinDump("")
	captured := dbgRecordUploads()
	debugScheduleAutoDelete = func(context.Context, []string, int) error { return nil }
	result, err := BuildDebugShare(context.Background(), 50, 1, true)
	if err != nil || !result.Redacted || dbgLeaks(*captured, secret) {
		t.Fatal(result, err)
	}
}

func TestBuildDebugShareOptionalLogFailureIsCollectedNotRaised(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	count := 0
	debugUploadToPastebin = func(context.Context, string, int) (string, error) {
		count++
		if count == 2 {
			return "", errors.New("paste service hiccup")
		}
		return fmt.Sprintf("https://paste.rs/p%d", count), nil
	}
	debugScheduleAutoDelete = func(context.Context, []string, int) error { return nil }
	result, err := BuildDebugShare(context.Background(), 50, 1, true)
	if err != nil || result.URLs["Report"] == "" || len(result.Failures) != 1 || !strings.Contains(result.Failures[0], "paste service hiccup") {
		t.Fatal(result, err)
	}
}

func dbgBundleText(b *debugBundle) string {
	parts := []string{}
	for _, k := range b.keys {
		parts = append(parts, b.vals[k])
	}
	return strings.Join(parts, "\n")
}

func TestCollectShareBundleRedactionKeepsSecretsOut(t *testing.T) {
	h := dbgHome(t)
	secret := "sk-proj-abcdefghijklmnopqrstuvwxyz1234567890"
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "line one\nOPENAI_API_KEY="+secret+"\nline three\n")
	dbgPinDump("")
	redacted, err := debugCollectShareBundle(context.Background(), 50, true)
	if err != nil {
		t.Fatal(err)
	}
	unredacted, _ := debugCollectShareBundle(context.Background(), 50, false)
	if !strings.Contains(dbgBundleText(unredacted), secret) || strings.Contains(dbgBundleText(redacted), secret) {
		t.Fatal(dbgBundleText(redacted))
	}
}

func TestCollectShareBundleRedactionMasksURLCredentials(t *testing.T) {
	h := dbgHome(t)
	token, password := "Q7fK2mZp9RtX4vLb8NcW1yHs", "Pw7Kq2Lm9Xs4Vb"
	dbgWrite(t, filepath.Join(h, "logs", "agent.log"), "2026-09-29 01:00:00 INFO plugins.web.firecrawl.provider: Firecrawl scraping: "+
		"https://files.example.com/export.csv?token="+token+"&page=2\n2026-09-29 01:00:01 INFO agent: using proxy http://alice:"+password+"@10.0.0.5:3128\n")
	dbgPinDump("")
	b, err := debugCollectShareBundle(context.Background(), 50, true)
	if err != nil {
		t.Fatal(err)
	}
	text := dbgBundleText(b)
	if strings.Contains(text, token) || strings.Contains(text, password) || !strings.Contains(text, "export.csv?token=***&page=2") ||
		!strings.Contains(text, "***:***@10.0.0.5:3128") {
		t.Fatal(text)
	}
}

func dbgEnvelope(t *testing.T, blob []byte) map[string]any {
	t.Helper()
	if len(blob) < 2 || blob[0] != 0x1f || blob[1] != 0x8b {
		t.Fatal("not gzip")
	}
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(zr)
	var env map[string]any
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func TestBuildNousBundleEnvelopeShapeAndGzip(t *testing.T) {
	b := &debugBundle{vals: map[string]string{}}
	b.set("report", "hello")
	b.set("agent.log", "log line")
	blob, err := debugBuildNousBundle(b, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	env := dbgEnvelope(t, blob)
	files, _ := env["files"].(map[string]any)
	if env["format"] != "hermes-debug-share/1" || env["redacted"] != true || len(files) != 2 || files["report"] != "hello" ||
		files["agent.log"] != "log line" || env["created"] == nil {
		t.Fatal(env)
	}
}

func TestBuildNousBundleRedactedFalseRecorded(t *testing.T) {
	b := &debugBundle{vals: map[string]string{}}
	b.set("report", "x")
	blob, _ := debugBuildNousBundle(b, false, time.Now())
	if env := dbgEnvelope(t, blob); env["redacted"] != false {
		t.Fatal(env)
	}
}

func dbgNousArgs() map[string]any {
	return map[string]any{"lines": 50, "expire": 7, "local": false, "nous": true, "no_redact": false, "yes": true}
}

func TestRunDebugShareNousNousSuccessPrintsViewURL(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	var blob []byte
	debugShareToNous = func(_ context.Context, b []byte) (*pyjson.Object, error) {
		blob = b
		return pyjson.ObjectOf("id", "id-1", "viewUrl", "https://support.example.com/diagnostics/id-1", "expiresAt", "2026-06-20T00:00:00Z"), nil
	}
	out, _, _ := dbgRun(t, dbgNousArgs())
	if !strings.Contains(out, "https://support.example.com/diagnostics/id-1") || !strings.Contains(out, "2026-06-20T00:00:00Z") ||
		len(blob) < 2 || blob[0] != 0x1f || blob[1] != 0x8b {
		t.Fatal(out)
	}
}

func TestRunDebugShareNousNousFailureSuggestsLocal(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	debugShareToNous = func(context.Context, []byte) (*pyjson.Object, error) { return nil, errors.New("service down") }
	_, errOut, code := dbgRun(t, dbgNousArgs())
	if code != 1 || !strings.Contains(errOut, "--local") {
		t.Fatal(code, errOut)
	}
}

func TestRunDebugShareNousNousDoesNotTouchPastebin(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	debugShareToNous = func(context.Context, []byte) (*pyjson.Object, error) {
		return pyjson.ObjectOf("id", "id-1", "viewUrl", "https://v"), nil
	}
	captured := dbgRecordUploads()
	dbgRun(t, dbgNousArgs())
	if len(*captured) != 0 {
		t.Fatal(*captured)
	}
}

func dbgConsentArgs(over map[string]any) map[string]any {
	ns := map[string]any{"lines": 50, "expire": 7, "local": false, "nous": false, "no_redact": false, "yes": false}
	for k, v := range over {
		ns[k] = v
	}
	return ns
}

func TestShareConsentGateNonInteractiveRequiresYes(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	debugConsole = func(w io.Writer) *cliui.Console {
		return &cliui.Console{In: strings.NewReader(""), Out: w, StdinTTY: func() bool { return false }}
	}
	captured := dbgRecordUploads()
	_, errOut, code := dbgRun(t, dbgConsentArgs(nil))
	if code != 1 || len(*captured) != 0 || !strings.Contains(errOut, "--yes") {
		t.Fatal(code, errOut)
	}
}

func TestShareConsentGateLocalNeverPrompts(t *testing.T) {
	dbgHome(t)
	dbgPinDump("")
	debugConsole = func(io.Writer) *cliui.Console { t.Fatal("input() must not be called for --local"); return nil }
	captured := dbgRecordUploads()
	out, _, _ := dbgRun(t, dbgConsentArgs(map[string]any{"local": true}))
	if len(*captured) != 0 || strings.Contains(out, "Aborted") {
		t.Fatal(out)
	}
}
