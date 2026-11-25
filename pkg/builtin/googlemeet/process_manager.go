package googlemeet

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/proc"
	"github.com/nickheyer/icebath/pkg/provider"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// Meetings workspace under the profile home, swapped by tests
var pmRoot = func(ctx context.Context) string {
	return filepath.Join(home.Dir(ctx), "workspace", "meetings")
}

// Pointer file naming the one active meeting
func activeFile(ctx context.Context) string { return filepath.Join(pmRoot(ctx), ".active.json") }

// Active meeting record, nil when missing or unparsable
func readActive(ctx context.Context) any {
	p := activeFile(ctx)
	info, err := os.Stat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	text, err := textutil.DecodeUTF8(textutil.TrimBOM(raw))
	if err != nil {
		return nil
	}
	v, err := jsonutil.Decode(text)
	if err != nil {
		return nil
	}
	return v
}

// Active record as a dict, nil when falsy and the AttributeError .get raises for a truthy non dict
func activeRecord(ctx context.Context) (*jsonutil.Object, error) {
	v := readActive(ctx)
	if !anyutil.IsTrue(v) {
		return nil, nil
	}
	obj, ok := v.(*jsonutil.Object)
	if !ok {
		return nil, fmt.Errorf("'%s' object has no attribute 'get'", typeName(v))
	}
	return obj, nil
}

// Writes the active pointer atomically
func writeActive(ctx context.Context, data *jsonutil.Object) error {
	return config.AtomicJSONWrite(ctx, activeFile(ctx), data)
}

// Whether pid names a live process, swapped by tests
var pidAlive = func(pid int) bool { return pid != 0 && proc.Exists(pid) }

// Signal sender, swapped by tests
var killProcess = killPID

// Sleep between stop polls, swapped by tests
var pmSleep = botSleep

// Record value under key with a default for a missing key
func getOr(o *jsonutil.Object, key string, def any) any {
	if o == nil {
		return def
	}
	if v, ok := o.Get(key); ok {
		return v
	}
	return def
}

// Result of every call made while no meeting is active
func noActive() *jsonutil.Object {
	return jsonutil.ObjectOf("ok", false, "reason", "no active meeting")
}

// Options of one bot start, defaults from newStartOptions
type startOptions struct {
	OutDir               string
	Headed               bool
	AuthState            string
	GuestName            string
	Duration             string
	SessionID            any
	Mode                 string
	RealtimeModel        string
	RealtimeVoice        string
	RealtimeInstructions string
	RealtimeAPIKey       string
}

// Start options with the default guest name and transcribe mode
func newStartOptions() startOptions {
	return startOptions{GuestName: "Icebath Agent", Mode: "transcribe"}
}

// Spawns the bot child with argv, env and the log file as stdout and stderr, returning its pid
var botPopen = func(argv, env []string, log *os.File) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Stdout, cmd.Stderr = env, log, log
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()
	return pid, nil
}

// Argv of the bot child, this binary re entered through its hidden entry argument
func botArgv() ([]string, error) {
	if !meetBotDispatched.Load() {
		panic("google_meet would re-execute a binary that never called googlemeet.DispatchMeetBotEntry, call it first in main or TestMain")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return []string{exe, MeetBotEntry}, nil
}

// Spawns the bot for url after stopping any running one, recording the active meeting
func pmStart(ctx context.Context, url string, o startOptions) (*jsonutil.Object, error) {
	if !isSafeMeetURL(url) {
		return jsonutil.ObjectOf("ok", false, "error", "refusing: only https://meet.google.com/ URLs are allowed. got: "+strconv.Quote(url)), nil
	}
	active, err := activeRecord(ctx)
	if err != nil {
		return nil, err
	}
	n, err := anyutil.ToInt(getOr(active, "pid", 0))
	if err != nil {
		return nil, err
	}
	pid := int(n)
	if pidAlive(pid) {
		if _, err := pmStop(ctx, "replaced by new meet_join"); err != nil {
			return nil, err
		}
	}
	meetingID := meetingIDFromURL(url)
	out := o.OutDir
	if out == "" {
		out = filepath.Join(pmRoot(ctx), meetingID)
	}
	if err := os.MkdirAll(out, 0o777); err != nil {
		return nil, err
	}
	for _, name := range []string{"transcript.txt", "status.json"} {
		_ = os.Remove(filepath.Join(out, name))
	}
	env := map[string]string{}
	var order []string
	put := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			put(k, v)
		}
	}
	put("ICEBATH_MEET_URL", url)
	put("ICEBATH_MEET_OUT_DIR", out)
	put("ICEBATH_MEET_GUEST_NAME", o.GuestName)
	headed := ""
	if o.Headed {
		headed = "1"
	}
	for _, kv := range [][2]string{
		{headed, "ICEBATH_MEET_HEADED"}, {o.AuthState, "ICEBATH_MEET_AUTH_STATE"}, {o.Duration, "ICEBATH_MEET_DURATION"},
		{o.Mode, "ICEBATH_MEET_MODE"}, {o.RealtimeModel, "ICEBATH_MEET_REALTIME_MODEL"},
		{o.RealtimeVoice, "ICEBATH_MEET_REALTIME_VOICE"}, {o.RealtimeInstructions, "ICEBATH_MEET_REALTIME_INSTRUCTIONS"},
	} {
		if kv[0] != "" {
			put(kv[1], kv[0])
		}
	}
	key := o.RealtimeAPIKey
	if key == "" {
		if key, _, err = provider.GetSecret(ctx, "ICEBATH_MEET_REALTIME_KEY"); err != nil {
			return nil, err
		}
		if key == "" {
			if key, _, err = provider.GetSecret(ctx, "OPENAI_API_KEY"); err != nil {
				return nil, err
			}
		}
	}
	if key != "" {
		put("ICEBATH_MEET_REALTIME_KEY", key)
	}
	childEnv := make([]string, 0, len(order))
	for _, k := range order {
		childEnv = append(childEnv, k+"="+env[k])
	}
	logPath := filepath.Join(out, "bot.log")
	logFH, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return nil, err
	}
	argv, err := botArgv()
	if err != nil {
		logFH.Close()
		return nil, err
	}
	childPID, err := botPopen(argv, childEnv, logFH)
	logFH.Close()
	if err != nil {
		return nil, err
	}
	record := jsonutil.ObjectOf("pid", childPID, "meeting_id", meetingID, "out_dir", out, "url", url,
		"started_at", nowSeconds(), "session_id", o.SessionID, "log_path", logPath, "mode", o.Mode)
	if err := writeActive(ctx, record); err != nil {
		return nil, err
	}
	res := jsonutil.ObjectOf("ok", true)
	res.Merge(record)
	return res, nil
}

// Current meeting state merged with the bot status file, or the no active meeting result
func pmStatus(ctx context.Context) (*jsonutil.Object, error) {
	active, err := activeRecord(ctx)
	if err != nil || active == nil {
		return noActive(), err
	}
	n, err := anyutil.ToInt(getOr(active, "pid", 0))
	if err != nil {
		return nil, err
	}
	pid := int(n)
	alive := pid != 0 && pidAlive(pid)
	outDir, _ := getOr(active, "out_dir", "").(string)
	res := jsonutil.ObjectOf("ok", true, "alive", alive, "pid", pid, "meetingId", getOr(active, "meeting_id", nil),
		"url", getOr(active, "url", nil), "startedAt", getOr(active, "started_at", nil), "outDir", getOr(active, "out_dir", nil))
	switch st := readJSON(filepath.Join(outDir, "status.json")).(type) {
	case *jsonutil.Object:
		res.Merge(st)
	case nil:
	default:
		if anyutil.IsTrue(st) {
			return nil, fmt.Errorf("'%s' object is not a mapping", typeName(st))
		}
	}
	return res, nil
}

// Python slice all[-last:] of the transcript lines
func tailLines(all []string, last int) []string {
	start := len(all) - last
	if last < 0 {
		start = -last
	}
	start = max(0, min(start, len(all)))
	return all[start:]
}

// Transcript lines of the active meeting, the last n when last is non zero
func pmTranscript(ctx context.Context, last int) (*jsonutil.Object, error) {
	active, err := activeRecord(ctx)
	if err != nil || active == nil {
		return noActive(), err
	}
	outDir, _ := getOr(active, "out_dir", "").(string)
	tp := filepath.Join(outDir, "transcript.txt")
	info, statErr := os.Stat(tp)
	if statErr != nil || !info.Mode().IsRegular() {
		return jsonutil.ObjectOf("ok", true, "meetingId", getOr(active, "meeting_id", nil), "lines", []any{}, "total", 0, "path", tp), nil
	}
	raw, err := os.ReadFile(tp)
	if err != nil {
		return nil, err
	}
	raw = []byte(strings.TrimPrefix(string(raw), "\xef\xbb\xbf"))
	text := strings.ToValidUTF8(string(raw), "�")
	var all []string
	for _, ln := range textutil.Lines(text) {
		if strings.TrimSpace(ln) != "" {
			all = append(all, ln)
		}
	}
	lines := all
	if last != 0 {
		lines = tailLines(all, last)
	}
	items := make([]any, len(lines))
	for i, l := range lines {
		items[i] = l
	}
	return jsonutil.ObjectOf("ok", true, "meetingId", getOr(active, "meeting_id", nil), "lines", items, "total", len(all), "path", tp), nil
}

// Appends a say request to the active realtime meeting's queue under the queue lock
func pmEnqueueSay(ctx context.Context, text string) (*jsonutil.Object, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return jsonutil.ObjectOf("ok", false, "reason", "text is required"), nil
	}
	active, err := activeRecord(ctx)
	if err != nil || active == nil {
		return noActive(), err
	}
	if mode := getOr(active, "mode", nil); mode != "realtime" {
		return jsonutil.ObjectOf("ok", false, "reason", "active meeting is in transcribe mode — pass mode='realtime' to meet_join to enable agent speech"), nil
	}
	outDir, _ := getOr(active, "out_dir", "").(string)
	if info, err := os.Stat(outDir); err != nil || !info.IsDir() {
		shown := outDir
		if shown == "" {
			shown = "."
		}
		return jsonutil.ObjectOf("ok", false, "reason", "out_dir missing: "+shown), nil
	}
	queuePath := filepath.Join(outDir, "say_queue.jsonl")
	id := uuid.New()
	entryID := hex.EncodeToString(id[:])[:12]
	line, err := jsonutil.Encode(jsonutil.ObjectOf("id", entryID, "text", text), jsonutil.Spaced(), jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	release, err := sayQueueLock(queuePath)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(queuePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		release()
		return nil, err
	}
	_, werr := f.WriteString(line + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	release()
	if werr != nil {
		return nil, werr
	}
	return jsonutil.ObjectOf("ok", true, "meetingId", getOr(active, "meeting_id", nil), "enqueued_id", entryID, "queue_path", queuePath), nil
}

// SIGTERMs the active bot, SIGKILL after ten seconds, then clears the active pointer
func pmStop(ctx context.Context, reason string) (*jsonutil.Object, error) {
	active, err := activeRecord(ctx)
	if err != nil || active == nil {
		return noActive(), err
	}
	n, err := anyutil.ToInt(getOr(active, "pid", 0))
	if err != nil {
		return nil, err
	}
	pid := int(n)
	outDir := getOr(active, "out_dir", nil)
	if pidAlive(pid) {
		if err := killProcess(pid, syscall.SIGTERM); err != nil {
			return nil, err
		}
		gone := false
		for range 20 {
			if !pidAlive(pid) {
				gone = true
				break
			}
			pmSleep(500 * 1e6)
		}
		if !gone {
			if err := killProcess(pid, syscall.SIGKILL); err != nil {
				return nil, err
			}
		}
	}
	if err := os.Remove(activeFile(ctx)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var transcriptPath any
	if anyutil.IsTrue(outDir) {
		transcriptPath = filepath.Join(anyutil.String(outDir), "transcript.txt")
	}
	return jsonutil.ObjectOf("ok", true, "reason", reason, "meetingId", getOr(active, "meeting_id", nil), "transcriptPath", transcriptPath), nil
}
