package googlemeet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Hidden first argument that routes a binary to the Meet bot through DispatchMeetBotEntry
const MeetBotEntry = "__icebath_google_meet_bot__"

var meetBotDispatched atomic.Bool

// DispatchMeetBotEntry runs the bot and exits when argv carries the entry, otherwise records that this binary handles it
func DispatchMeetBotEntry() {
	meetBotDispatched.Store(true)
	if len(os.Args) > 1 && os.Args[1] == MeetBotEntry {
		os.Exit(runBot())
	}
}

var meetURLRe = regexp.MustCompile(`^https://meet\.google\.com/([a-z0-9]{3,}-[a-z0-9]{3,}-[a-z0-9]{3,}|lookup/[^/?#]+|new)(?:[/?#](?s:.*))?$`)

var meetingCodeRe = regexp.MustCompile(`meet\.google\.com/([a-z0-9]{3,}-[a-z0-9]{3,}-[a-z0-9]{3,})`)

const ffmpegMissing = "ffmpeg not found — install via `brew install ffmpeg` for realtime on macOS"

// True for a Google Meet URL the bot is willing to navigate to
func isSafeMeetURL(url string) bool {
	return meetURLRe.MatchString(pystr.Strip(url))
}

// Three segment meeting code, or a timestamped id for lookup and new URLs
func meetingIDFromURL(url string) string {
	if m := meetingCodeRe.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return fmt.Sprintf("meet-%d", time.Now().Unix())
}

// One status.json key with its initial value, in file order
type statusField struct {
	key string
	def any
}

var statusFields = []statusField{
	{"meetingId", nil}, {"url", nil}, {"inCall", false}, {"captioning", false}, {"captionsEnabledAttempted", false},
	{"lobbyWaiting", false}, {"joinAttemptedAt", nil}, {"joinedAt", nil}, {"lastCaptionAt", nil},
	{"transcriptLines", 0}, {"transcriptPath", nil}, {"error", nil}, {"exited", false}, {"pid", nil},
	{"realtime", false}, {"realtimeReady", false}, {"realtimeDevice", nil}, {"audioBytesOut", 0},
	{"lastAudioOutAt", nil}, {"lastBargeInAt", nil}, {"leaveReason", nil}, {"micState", nil},
}

// Bot state shared by the drain loop and the speaker, flushed to status.json on each change
type botState struct {
	mu             sync.Mutex
	vals           map[string]any
	seen           map[string]bool
	outDir         string
	transcriptPath string
	statusPath     string
}

// Creates the output directory and writes the initial status file
func newBotState(outDir, meetingID, url string) (*botState, error) {
	s := &botState{vals: map[string]any{}, seen: map[string]bool{}, outDir: outDir,
		transcriptPath: filepath.Join(outDir, "transcript.txt"), statusPath: filepath.Join(outDir, "status.json")}
	for _, f := range statusFields {
		s.vals[f.key] = f.def
	}
	s.vals["meetingId"], s.vals["url"] = meetingID, url
	if err := os.MkdirAll(outDir, 0o777); err != nil {
		return nil, err
	}
	return s, s.flushLocked()
}

// Value of one status key
func (s *botState) get(key string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vals[key]
}

// Appends a caption line unless this exact speaker and text pair was already seen
func (s *botState) recordCaption(speaker, text string) error {
	speaker, text = pystr.Strip(speaker), pystr.Strip(text)
	if speaker == "" {
		speaker = "Unknown"
	}
	key := speaker + "|" + text
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == "" || s.seen[key] {
		return nil
	}
	s.seen[key] = true
	s.vals["transcriptLines"] = s.vals["transcriptLines"].(int) + 1
	now := time.Now()
	s.vals["lastCaptionAt"] = float64(now.UnixNano()) / 1e9
	f, err := os.OpenFile(s.transcriptPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "[%s] %s: %s\n", now.Format("15:04:05"), speaker, text)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return s.flushLocked()
}

// Writes status.json in table key order with the transcript path and this process id
func (s *botState) flushLocked() error {
	data := pyjson.NewObject()
	for _, f := range statusFields {
		data.Set(f.key, s.vals[f.key])
	}
	data.Set("transcriptPath", s.transcriptPath)
	data.Set("pid", os.Getpid())
	return config.AtomicJSONWrite(s.statusPath, data)
}

// Updates status keys from alternating key and value arguments and flushes
func (s *botState) set(kv ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		s.vals[kv[i].(string)] = kv[i+1]
	}
	if err := s.flushLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "google_meet bot: status write failed: %v\n", err)
	}
}

const captionObserverJS = `
(() => {
  if (window.__icebathMeetInstalled) return;
  window.__icebathMeetInstalled = true;
  window.__icebathMeetQueue = [];

  const captionSelector = '[role="region"][aria-label*="aption" i], ' +
                          'div[jsname="YSxPC"], ' +  // legacy
                          'div[jsname="tgaKEf"]';    // current (Apr 2026)

  function pushEntry(speaker, text) {
    if (!text || !text.trim()) return;
    window.__icebathMeetQueue.push({
      ts: Date.now(),
      speaker: (speaker || '').trim(),
      text: text.trim(),
    });
  }

  function scan(root) {
    // Meet captions render as rows of speaker label + text block. Selectors
    // vary across Meet rewrites; try a few shapes and fall back to raw text.
    const rows = root.querySelectorAll('div[jsname="dsyhDe"], div.CNusmb, div.TBMuR');
    if (rows.length) {
      rows.forEach((row) => {
        const spkEl = row.querySelector('div.KcIKyf, div.zs7s8d, span[jsname="YSxPC"]');
        const txtEl = row.querySelector('div.bh44bd, span[jsname="tgaKEf"], div.iTTPOb');
        pushEntry(spkEl ? spkEl.innerText : '', txtEl ? txtEl.innerText : row.innerText);
      });
      return;
    }
    // Fallback: treat the whole region's innerText as one anonymous line.
    pushEntry('', (root.innerText || '').split('\n').filter(Boolean).pop());
  }

  function attach() {
    const el = document.querySelector(captionSelector);
    if (!el) return false;
    new MutationObserver(() => scan(el)).observe(el, { childList: true, subtree: true, characterData: true });
    scan(el);
    return true;
  }

  // Retry on interval — the caption region only appears after captions are
  // enabled and someone speaks.
  if (!attach()) {
    const iv = setInterval(() => { if (attach()) clearInterval(iv); }, 1500);
  }

  window.__icebathMeetDrain = () => {
    const out = window.__icebathMeetQueue.slice();
    window.__icebathMeetQueue = [];
    return out;
  };
})();
`

const enableCaptionsJS = "(() => { document.body.dispatchEvent(new KeyboardEvent('keydown', " +
	"{ key: 'c', code: 'KeyC', keyCode: 67, which: 67, bubbles: true })); return true; })();"

const leaveCallJS = "() => { const b = document.querySelector('button[aria-label*=\"eave call\"]');" +
	" if (b) b.click(); }"

const admissionProbeJS = `
    (() => {
      if (document.querySelector('button[aria-label*="eave call" i]')) return true;
      if (window.__icebathMeetInstalled && document.querySelector(
          '[role="region"][aria-label*="aption" i], div[jsname="YSxPC"], div[jsname="tgaKEf"]')) return true;
      return !!document.querySelector('[aria-label*="articipants" i]');
    })();
    `

const deniedProbeJS = `
    (() => {
      const text = document.body ? document.body.innerText || '' : '';
      return /You can't join this video call|You were removed from the meeting|No one responded to your request to join/i.test(text);
    })();
    `

const drainJS = "window.__icebathMeetDrain && window.__icebathMeetDrain()"

// Python truthiness of a value a page evaluation returned
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0
	case int:
		return x != 0
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	return true
}

// Boolean JS probe, false on any evaluation error
func probe(page meetPage, js string) bool {
	v, err := page.Evaluate(js)
	return err == nil && truthy(v)
}

// First match when it exists and is visible, otherwise nil with errors swallowed
func visible(loc meetLocator) meetLocator {
	first := loc.First()
	if n, err := first.Count(); err != nil || n == 0 {
		return nil
	}
	if ok, err := first.IsVisible(); err != nil || !ok {
		return nil
	}
	return first
}

// Running pcm pump whose stdin receives the speaker audio
type pumpProc interface {
	Alive() bool
	Stdin() io.WriteCloser
}

// Pump child started through os/exec with a reaper goroutine
type execPump struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	done  chan struct{}
}

func (p *execPump) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *execPump) Stdin() io.WriteCloser { return p.stdin }

// Sends SIGTERM to the pump
func (p *execPump) terminate() error { return p.cmd.Process.Signal(syscall.SIGTERM) }

// Waits up to timeout for the pump to exit
func (p *execPump) wait(timeout time.Duration) bool {
	select {
	case <-p.done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Command a pump argv runs as, swapped by tests for a stand in
var pumpCommand = func(argv []string) *exec.Cmd { return exec.Command(argv[0], argv[1:]...) }

// Starts a pump with piped stdin and null stdout and stderr
func startPump(argv []string) (*execPump, error) {
	cmd := pumpCommand(argv)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &execPump{cmd: cmd, stdin: stdin, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Whether err is the closed pipe or OS error a tail write to an exited pump raises
func isPipeError(err error) bool {
	var pathErr *fs.PathError
	var sysErr *os.SyscallError
	var errno syscall.Errno
	return errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, syscall.EPIPE) ||
		errors.As(err, &pathErr) || errors.As(err, &sysErr) || errors.As(err, &errno)
}

// Follows speaker.pcm as it grows and forwards each appended chunk to the pump until stop or pump exit
func pcmTailLoop(proc pumpProc, pcmPath string, stop *atomic.Bool, poll time.Duration) (err error) {
	defer func() { _ = proc.Stdin().Close() }()
	f, err := os.Open(pcmPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 65536)
	for !stop.Load() && proc.Alive() {
		n, rerr := f.Read(buf)
		if n == 0 {
			if rerr != nil && rerr != io.EOF {
				if isPipeError(rerr) {
					return nil
				}
				return rerr
			}
			time.Sleep(poll)
			continue
		}
		if _, werr := proc.Stdin().Write(buf[:n]); werr != nil {
			if isPipeError(werr) {
				return nil
			}
			return werr
		}
	}
	return nil
}

// Realtime resources kept together so teardown works however the bot exits
type realtimeRes struct {
	enabled     bool
	bridge      *audioBridge
	bridgeInfo  map[string]any
	session     *realtimeSession
	speakerDone chan struct{}
	pcmPump     *execPump
	pcmTailDone chan struct{}
}

// Streams the growing speaker.pcm into the device Chrome's fake mic reads through paplay or ffmpeg
func startPCMPump(rt *realtimeRes, bridgeInfo map[string]any, pcmPath string, state *botState, stop *atomic.Bool) error {
	platformTag, _ := bridgeInfo["platform"].(string)
	target, _ := bridgeInfo["write_target"].(string)
	var argv []string
	var missing string
	switch platformTag {
	case "linux":
		if target == "" {
			target = "icebath_meet_sink"
		}
		argv = []string{"paplay", "--raw", "--rate=24000", "--format=s16le", "--channels=1", "--device=" + target, "-"}
		missing = "paplay not found — install pulseaudio-utils for realtime on Linux"
	case "darwin":
		if _, ok := pyshutil.Which("ffmpeg", nil); !ok {
			state.set("error", ffmpegMissing)
			return nil
		}
		if target == "" {
			target = "BlackHole 2ch"
		}
		argv = []string{"ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "s16le", "-ar", "24000", "-ac", "1",
			"-i", "-", "-f", "audiotoolbox", "-audio_device_index", macAudioDeviceIndex(target), "-"}
		missing = ffmpegMissing
	default:
		return nil
	}
	pump, err := startPump(argv)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			state.set("error", missing)
			return nil
		}
		if platformTag != "darwin" {
			return err
		}
		state.set("error", fmt.Sprintf("macOS pcm pump failed to start: %v", err))
		return nil
	}
	rt.pcmPump = pump
	rt.pcmTailDone = make(chan struct{})
	go func() {
		defer close(rt.pcmTailDone)
		if err := pcmTailLoop(pump, pcmPath, stop, 50*time.Millisecond); err != nil {
			fmt.Fprintf(os.Stderr, "Exception in thread meet-pcm-tail: %v\n", err)
		}
	}()
	return nil
}

// Wires the realtime session, the say queue speaker goroutine and the pcm pump
func startRealtimeSpeaker(rt *realtimeRes, cfg botConfig, stop *atomic.Bool, state *botState) error {
	pcmPath, queuePath := filepath.Join(cfg.OutDir, "speaker.pcm"), filepath.Join(cfg.OutDir, "say_queue.jsonl")
	if err := os.WriteFile(pcmPath, nil, 0o666); err != nil {
		return err
	}
	q, err := os.OpenFile(queuePath, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return err
	}
	q.Close()
	now := time.Now()
	_ = os.Chtimes(queuePath, now, now)
	session := newRealtimeSession(cfg.RealtimeAPIKey, cfg.RealtimeModel, cfg.RealtimeVoice, cfg.RealtimeInstructions, pcmPath, 24000)
	if err := session.connect(); err != nil {
		state.set("error", fmt.Sprintf("realtime connect failed: %v", err))
		return nil
	}
	rt.session = session
	speaker := newRealtimeSpeaker(session, queuePath, filepath.Join(cfg.OutDir, "say_processed.jsonl"))
	rt.speakerDone = make(chan struct{})
	go func() {
		defer close(rt.speakerDone)
		if err := speaker.runUntilStopped(stop.Load, 500*time.Millisecond); err != nil {
			state.set("error", fmt.Sprintf("realtime speaker crashed: %v", err))
		}
	}()
	if err := startPCMPump(rt, rt.bridgeInfo, pcmPath, state, stop); err != nil {
		return err
	}
	state.set("realtimeReady", true)
	return nil
}

var avfoundationDeviceRe = regexp.MustCompile(`\[(\p{Nd}+)\][\s\x1c-\x1f\x85\p{Z}]+(.+)$`)

// ffmpeg audio device index for a device name ignoring case, 0 when ffmpeg lists none such
func macAudioDeviceIndex(deviceName string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-f", "avfoundation", "-list_devices", "true", "-i", "")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) || ctx.Err() != nil {
		return "0"
	}
	needle := pystr.Lower(pystr.Strip(deviceName))
	for _, line := range pystr.Splitlines(strings.ToValidUTF8(stderr.String(), "�"), false) {
		if m := avfoundationDeviceRe.FindStringSubmatch(line); m != nil && pystr.Lower(pystr.Strip(m[2])) == needle {
			return m[1]
		}
	}
	return "0"
}

// Provisions the virtual audio bridge, falling back to transcribe mode on any failure
func setupRealtime(rt *realtimeRes, apiKey string, state *botState) {
	if apiKey == "" {
		state.set("error", "realtime mode requested but no API key in ICEBATH_MEET_REALTIME_KEY/OPENAI_API_KEY — falling back to transcribe")
		rt.enabled = false
		return
	}
	rt.bridge = newAudioBridge("icebath_meet")
	info, err := rt.bridge.setup()
	if err != nil {
		state.set("error", fmt.Sprintf("audio bridge setup failed: %v — falling back to transcribe", err))
		rt.enabled = false
		return
	}
	rt.bridgeInfo = info
	state.set("realtime", true, "realtimeDevice", info["device_name"])
}

// Waits on a done channel up to timeout
func joinDone(done chan struct{}, timeout time.Duration) {
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// Stops the pump, joins the tail and speaker goroutines, closes the session and releases the bridge
func teardownRealtime(rt *realtimeRes) {
	if rt.pcmPump != nil {
		_ = rt.pcmPump.terminate()
		rt.pcmPump.wait(3 * time.Second)
	}
	joinDone(rt.pcmTailDone, time.Second)
	joinDone(rt.speakerDone, 5*time.Second)
	if rt.session != nil {
		rt.session.close()
	}
	if rt.bridge != nil {
		rt.bridge.teardown()
	}
}

// Everything the bot reads from the ICEBATH_MEET environment variables
type botConfig struct {
	URL                  string
	OutDir               string
	Headed               bool
	AuthState            string
	GuestName            string
	DurationS            float64
	Realtime             bool
	RealtimeAPIKey       string
	RealtimeModel        string
	RealtimeVoice        string
	RealtimeInstructions string
	LobbyTimeout         float64
}

// Environment value with a default for an unset variable
func envOr(name, def string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return def
}

// Bot configuration from the environment, an unparsable lobby timeout being the ValueError float raises
func configFromEnv() (botConfig, error) {
	duration, _ := parseDuration(envOr("ICEBATH_MEET_DURATION", ""))
	key := os.Getenv("ICEBATH_MEET_REALTIME_KEY")
	if key == "" {
		key = envOr("OPENAI_API_KEY", "")
	}
	lobbyRaw := envOr("ICEBATH_MEET_LOBBY_TIMEOUT", "300")
	lobby, err := pyint.ParseFloat(lobbyRaw)
	if err != nil {
		return botConfig{}, fmt.Errorf("ValueError: could not convert string to float: %s", pyrepr.StrRepr(lobbyRaw))
	}
	headed := pystr.Lower(envOr("ICEBATH_MEET_HEADED", ""))
	return botConfig{
		URL:                  pystr.Strip(envOr("ICEBATH_MEET_URL", "")),
		OutDir:               pystr.Strip(envOr("ICEBATH_MEET_OUT_DIR", "")),
		Headed:               headed == "1" || headed == "true" || headed == "yes",
		AuthState:            pystr.Strip(envOr("ICEBATH_MEET_AUTH_STATE", "")),
		GuestName:            envOr("ICEBATH_MEET_GUEST_NAME", "Icebath Agent"),
		DurationS:            duration,
		Realtime:             pystr.Lower(pystr.Strip(envOr("ICEBATH_MEET_MODE", "transcribe"))) == "realtime",
		RealtimeAPIKey:       key,
		RealtimeModel:        envOr("ICEBATH_MEET_REALTIME_MODEL", "gpt-realtime"),
		RealtimeVoice:        envOr("ICEBATH_MEET_REALTIME_VOICE", "alloy"),
		RealtimeInstructions: envOr("ICEBATH_MEET_REALTIME_INSTRUCTIONS", ""),
		LobbyTimeout:         lobby,
	}, nil
}

// Sleep the join and drain loops take between polls, swapped by tests
var botSleep = time.Sleep

// Wall clock seconds like time.time
func nowSeconds() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Fills the guest name and clicks Join now or Ask to join, polling up to timeout for the late buttons
func join(page meetPage, cfg botConfig, state *botState, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for {
		if nameBox := visible(page.Locator(`input[aria-label*="name" i]`)); nameBox != nil {
			_ = nameBox.Fill(cfg.GuestName, 2*time.Second)
		}
		for _, label := range []string{"Join now", "Ask to join"} {
			btn := visible(page.GetByRole("button", label, false))
			if btn != nil && btn.Click(3*time.Second) == nil {
				if label == "Ask to join" {
					state.set("lobbyWaiting", true)
				}
				return
			}
		}
		if !time.Now().Before(deadline) {
			return
		}
		botSleep(500 * time.Millisecond)
	}
}

// Unmutes the admitted bot, reporting unmuted_clicked, unmuted or unknown when no toggle is found
func ensureMicOn(page meetPage) string {
	if muted := visible(page.Locator(`button[aria-label*="Turn on microphone" i]`)); muted != nil && muted.Click(3*time.Second) == nil {
		return "unmuted_clicked"
	}
	if visible(page.Locator(`button[aria-label*="Turn off microphone" i]`)) != nil {
		return "unmuted"
	}
	return "unknown"
}

// Admission and caption drain loop until stop, duration expiry, lobby timeout or denial, or page loss
func drainLoop(page meetPage, cfg botConfig, state *botState, rt *realtimeRes, stop *atomic.Bool) {
	start := nowSeconds()
	deadline := 0.0
	if cfg.DurationS != 0 {
		deadline = start + cfg.DurationS
	}
	lobbyDeadline := start + cfg.LobbyTimeout
	lastAdmissionCheck := 0.0
	for !stop.Load() {
		now := nowSeconds()
		if deadline != 0 && now > deadline {
			state.set("leaveReason", "duration_expired")
			return
		}
		if inCall, _ := state.get("inCall").(bool); !inCall && now-lastAdmissionCheck > 3.0 {
			lastAdmissionCheck = now
			if probe(page, admissionProbeJS) {
				mic := ensureMicOn(page)
				state.set("inCall", true, "lobbyWaiting", false, "joinedAt", now, "micState", mic)
			} else if now > lobbyDeadline {
				waited := 0
				if at, ok := state.get("joinAttemptedAt").(float64); ok && at != 0 {
					waited = int(lobbyDeadline - at)
				}
				state.set("error", fmt.Sprintf("lobby timeout — host never admitted the bot within %ds", waited), "leaveReason", "lobby_timeout")
				return
			} else if probe(page, deniedProbeJS) {
				state.set("error", "host denied admission", "leaveReason", "denied")
				return
			}
		}
		if err := drainCaptions(page, cfg, state, rt, now); err != nil && page.IsClosed() {
			state.set("leaveReason", "page_closed")
			return
		}
		if rt.session != nil {
			out, at := rt.session.counters()
			state.set("audioBytesOut", out, "lastAudioOutAt", at)
		}
		botSleep(time.Second)
	}
}

// Pulls queued captions into the transcript and barges in when a human spoke over realtime audio
func drainCaptions(page meetPage, cfg botConfig, state *botState, rt *realtimeRes, now float64) error {
	queued, err := page.Evaluate(drainJS)
	if err != nil {
		return err
	}
	list, _ := queued.([]any)
	for _, e := range list {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		speaker, text := "", ""
		if v, ok := entry["speaker"]; ok {
			speaker = pyrepr.Str(v)
		}
		if v, ok := entry["text"]; ok {
			text = pyrepr.Str(v)
		}
		if err := state.recordCaption(speaker, text); err != nil {
			return err
		}
		if rt.session != nil && looksLikeHumanSpeaker(speaker, cfg.GuestName) && rt.session.cancelResponse() {
			state.set("lastBargeInAt", now)
		}
	}
	return nil
}

var botContextArgs = contextArgs{
	Width: 1280, Height: 800,
	UserAgent:   "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36",
	Permissions: []string{"microphone", "camera"},
}

// Tool environment of the bot child, its profile home and config
func botEnv() *tools.Env {
	h := home.Dir(context.Background())
	tree, _ := config.Load(h, os.LookupEnv)
	return &tools.Env{Home: h, Config: tree}
}

// Runs the bot from its environment, returning the process exit code
func runBot() int {
	cfg, err := configFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	if !isSafeMeetURL(cfg.URL) {
		fmt.Fprintf(os.Stderr, "google_meet bot: refusing to launch — ICEBATH_MEET_URL must be a meet.google.com URL. got: %s\n", pyrepr.StrRepr(cfg.URL))
		return 2
	}
	if cfg.OutDir == "" {
		fmt.Fprint(os.Stderr, "google_meet bot: ICEBATH_MEET_OUT_DIR is required\n")
		return 2
	}
	state, err := newBotState(cfg.OutDir, meetingIDFromURL(cfg.URL), cfg.URL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	stop := &atomic.Bool{}
	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for range sigs {
			stop.Store(true)
		}
	}()
	rt := &realtimeRes{enabled: cfg.Realtime}
	if rt.enabled {
		setupRealtime(rt, cfg.RealtimeAPIKey, state)
	}
	return runBotBrowser(cfg, state, rt, stop, botEnv())
}

// Browser half of the bot, any failure recorded as unhandled with exit code 1
func runBotBrowser(cfg botConfig, state *botState, rt *realtimeRes, stop *atomic.Bool, env *tools.Env) (code int) {
	defer func() {
		if r := recover(); r != nil {
			state.set("error", fmt.Sprintf("unhandled: %v", r), "exited", true)
			code = 1
		}
	}()
	unhandled := func(err error) int {
		state.set("error", fmt.Sprintf("unhandled: %v", err), "exited", true)
		return 1
	}
	chromeArgs := []string{"--use-fake-ui-for-media-stream", "--disable-blink-features=AutomationControlled"}
	if !rt.enabled {
		chromeArgs = []string{chromeArgs[0], "--use-fake-device-for-media-stream", chromeArgs[1]}
	} else if rt.bridgeInfo != nil && rt.bridgeInfo["platform"] == "linux" {
		device, _ := rt.bridgeInfo["device_name"].(string)
		os.Setenv("PULSE_SOURCE", device)
	}
	args := botContextArgs
	if cfg.AuthState != "" {
		if info, err := os.Stat(cfg.AuthState); err == nil && info.Mode().IsRegular() {
			args.StorageState = cfg.AuthState
		}
	}
	b, err := launchMeetBrowser(env, cfg.Headed, chromeArgs)
	if err != nil {
		return unhandled(err)
	}
	defer b.close()
	bctx, err := b.newContext(args)
	if err != nil {
		return unhandled(err)
	}
	page, err := bctx.newPage()
	if err != nil {
		return unhandled(err)
	}
	if err := page.gotoURL(cfg.URL, 30*time.Second); err != nil {
		state.set("error", fmt.Sprintf("navigate failed: %v", err), "exited", true)
		return 4
	}
	join(page, cfg, state, 30*time.Second)
	if v, err := page.Evaluate(enableCaptionsJS); err == nil && truthy(v) {
		state.set("captionsEnabledAttempted", true)
	}
	if _, err := page.Evaluate(captionObserverJS); err != nil {
		state.set("error", fmt.Sprintf("caption observer install failed: %v", err))
	}
	state.set("captioning", true, "joinAttemptedAt", nowSeconds())
	if rt.enabled {
		if err := startRealtimeSpeaker(rt, cfg, stop, state); err != nil {
			return unhandled(err)
		}
	}
	drainLoop(page, cfg, state, rt, stop)
	_, _ = page.Evaluate(leaveCallJS)
	bctx.close()
	teardownRealtime(rt)
	state.set("inCall", false, "captioning", false, "exited", true)
	return 0
}

// Whether a caption speaker is probably a human rather than the bot's own echo or an unknown label
func looksLikeHumanSpeaker(speaker, botGuestName string) bool {
	s := pystr.Strip(speaker)
	if s == "" {
		return false
	}
	l := pystr.Lower(s)
	return l != "unknown" && l != "you" && l != pystr.Lower(pystr.Strip(botGuestName))
}

var durationUnits = map[string]float64{"h": 3600.0, "m": 60.0, "s": 1.0}

// Seconds of 30m, 2h or a bare number, false when raw is empty or unparsable
func parseDuration(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	raw = pystr.Lower(pystr.Strip(raw))
	r := []rune(raw)
	last := ""
	if len(r) > 0 {
		last = string(r[len(r)-1])
	}
	if mult, ok := durationUnits[last]; ok {
		v, err := pyint.ParseFloat(string(r[:len(r)-1]))
		if err != nil {
			return 0, false
		}
		return v * mult, true
	}
	v, err := pyint.ParseFloat(raw)
	if err != nil {
		return 0, false
	}
	return v, true
}
