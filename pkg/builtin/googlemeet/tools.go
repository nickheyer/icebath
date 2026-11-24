package googlemeet

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// True when the bot driver this binary links can be re entered as the bot child
func meetDriverAvailable() bool {
	if !meetBotDispatched.Load() {
		return false
	}
	_, err := os.Executable()
	return err == nil
}

// True when the plugin can run locally on Linux or macOS with the bot driver available
func checkMeetRequirements(env *tools.Env) bool {
	return (runtime.GOOS == "linux" || runtime.GOOS == "darwin") && meetDriverAvailable()
}

// Client and name of the addressed node, auto meaning the sole registered node, nil when none matches
func resolveNode(ctx context.Context, node any) (*nodeClient, any, error) {
	reg := newNodeRegistry(ctx, "")
	var entry *jsonutil.Object
	var err error
	switch v := node.(type) {
	case string:
		if v == "auto" {
			v = ""
		}
		entry, err = reg.resolve(v)
	case []any, map[string]any, *jsonutil.Object:
		if anyutil.IsTrue(v) {
			return nil, nil, &TypeError{fmt.Sprintf("unhashable type: '%s'", pyTypeName(v))}
		}
		entry, err = reg.resolve("")
	default:
		if !anyutil.IsTrue(v) {
			entry, err = reg.resolve("")
		}
	}
	if err != nil || entry == nil {
		return nil, nil, err
	}
	field := func(key string) (string, error) {
		v, ok := entry.Get(key)
		if !ok {
			return "", &KeyError{key}
		}
		s, ok := v.(string)
		if !ok {
			return "", &ValueError{key + " must be a non-empty string"}
		}
		return s, nil
	}
	url, err := field("url")
	if err != nil {
		return nil, nil, err
	}
	token, err := field("token")
	if err != nil {
		return nil, nil, err
	}
	client, err := newNodeClient(url, token, nodeClientTimeout)
	if err != nil {
		return nil, nil, err
	}
	name, _ := entry.Get("name")
	return client, name, nil
}

const nodeClientTimeout = 10 * time.Second

func strProp(description string) *jsonutil.Object {
	return jsonutil.ObjectOf("type", "string", "description", description)
}

// Tool schema with an object parameter block closed to extra properties
func meetSchema(name, description string, properties *jsonutil.Object, required ...any) *jsonutil.Object {
	params := jsonutil.ObjectOf("type", "object", "properties", properties)
	if len(required) > 0 {
		params.Set("required", required)
	}
	params.Set("additionalProperties", false)
	return jsonutil.ObjectOf("name", name, "description", description, "parameters", params)
}

func nodeProp() map[string]any { return map[string]any{"type": "string"} }

var meetJoinSchema = meetSchema("meet_join",
	"Join a Google Meet call and start scraping live captions into a transcript file. Only "+
		"meet.google.com URLs are accepted; no calendar scanning, no auto-dial. Spawns a headless "+
		"Chromium subprocess that runs in parallel with the agent loop — returns immediately. Poll "+
		"with meet_status and read captions with meet_transcript. Reminder to the agent: you "+
		"should announce yourself in the meeting (there is no automatic consent announcement).",
	jsonutil.ObjectOf(
		"url", strProp("Full https://meet.google.com/... URL. Required."),
		"mode", jsonutil.ObjectOf("type", "string", "enum", []any{"transcribe", "realtime"},
			"description", "transcribe (default): listen-only, scrape captions. "+
				"realtime: also enable agent speech via meet_say "+
				"(requires OpenAI Realtime key + platform audio bridge)."),
		"guest_name", strProp("Display name to use when joining as guest. Defaults to 'Icebath Agent'."),
		"duration", strProp("Optional max duration before auto-leave (e.g. '30m', "+
			"'2h', '90s'). Omit to stay until meet_leave is called."),
		"headed", jsonutil.ObjectOf("type", "boolean",
			"description", "Run Chromium headed instead of headless (debug only). Default false."),
		"node", strProp("Name of a registered remote node to run the bot on (useful when the gateway "+
			"runs on a headless Linux box but the user's Chrome with a signed-in Google "+
			"profile lives on their Mac). Pass 'auto' to use the single registered node. "+
			"Default: run locally. Nodes are approved via `icebath meet node approve`."),
	), "url")

var meetStatusSchema = meetSchema("meet_status",
	"Report the current Meet session state — whether the bot is alive, has joined, is sitting "+
		"in the lobby, number of transcript lines captured, and last-caption timestamp.",
	jsonutil.ObjectOf("node", nodeProp()))

var meetTranscriptSchema = meetSchema("meet_transcript",
	"Read the scraped transcript for the active Meet session. Returns "+
		"full transcript unless 'last' is set, in which case returns the last N lines only.",
	jsonutil.ObjectOf(
		"last", jsonutil.ObjectOf("type", "integer",
			"description", "Optional: return only the last N caption lines. Useful "+
				"for polling during a meeting without re-reading the whole transcript.",
			"minimum", 1),
		"node", nodeProp(),
	))

var meetLeaveSchema = meetSchema("meet_leave",
	"Leave the active Meet call cleanly, stop caption scraping, and finalize the transcript "+
		"file. Safe to call when no meeting is active — returns ok=false with a reason.",
	jsonutil.ObjectOf("node", nodeProp()))

var meetSaySchema = meetSchema("meet_say",
	"Speak text into the active Meet call. Requires the active meeting to have been joined "+
		"with mode='realtime'. The text is queued to the bot's OpenAI Realtime session; the "+
		"generated audio is streamed into Chrome's fake microphone via a virtual audio device "+
		"(PulseAudio null-sink on Linux, BlackHole on macOS). Returns immediately — the actual "+
		"speech lags by a couple of seconds.",
	jsonutil.ObjectOf("text", strProp("Text to speak."), "node", nodeProp()), "text")

// JSON text of a tool result with non ASCII kept
func toolJSON(obj any) string {
	out, err := jsonutil.Encode(obj, jsonutil.Spaced())
	if err != nil {
		panic(err)
	}
	return out
}

// Failure result carrying the message and extra key value pairs
func toolErr(msg string, extra ...any) string {
	return toolJSON(jsonutil.ObjectOf(append([]any{"success", false, "error", msg}, extra...)...))
}

// Result with success from its ok field followed by the result keys
func withSuccess(res *jsonutil.Object, pairs ...any) *jsonutil.Object {
	out := jsonutil.ObjectOf(append([]any{"success", anyutil.IsTrue(getOr(res, "ok", nil))}, pairs...)...)
	out.Merge(res)
	return out
}

// Runs remote on the addressed node, else local, wrapping either outcome as a tool result
func dispatchMeet(ctx context.Context, node any, op string, remote func(*nodeClient) (*jsonutil.Object, error), local func() (*jsonutil.Object, error)) string {
	if !anyutil.IsTrue(node) {
		res, err := local()
		if err != nil {
			panic(err)
		}
		return toolJSON(withSuccess(res))
	}
	client, name, err := resolveNode(ctx, node)
	if err != nil {
		panic(err)
	}
	if client == nil {
		return toolErr(fmt.Sprintf("no registered meet node matches %s — "+
			"run `icebath meet node approve <name> <url> <token>` first", anyutil.Quote(node)))
	}
	res, err := remote(client)
	if err != nil {
		return toolErr(fmt.Sprintf("remote node %s failed: %v", op, err), "node", name)
	}
	return toolJSON(withSuccess(res, "node", name))
}

// Stripped text of an argument or its default when falsy, an AttributeError for a truthy non string
func argText(args map[string]any, key, def string) string {
	v := args[key]
	if !anyutil.IsTrue(v) {
		return strings.TrimSpace(def)
	}
	s, ok := v.(string)
	if !ok {
		panic(&AttributeError{fmt.Sprintf("'%s' object has no attribute 'strip'", pyTypeName(v))})
	}
	return strings.TrimSpace(s)
}

// Joins a Meet URL locally or on a node after the url and mode checks
func handleMeetJoin(ctx context.Context, call *tools.Call) string {
	args := call.Args
	url := argText(args, "url", "")
	if url == "" {
		return toolErr("url is required")
	}
	mode := strings.ToLower(argText(args, "mode", "transcribe"))
	if mode != "transcribe" && mode != "realtime" {
		return toolErr(fmt.Sprintf("mode must be 'transcribe' or 'realtime' (got %s)", strconv.Quote(mode)))
	}
	o := newStartOptions()
	if v := args["guest_name"]; anyutil.IsTrue(v) {
		o.GuestName = anyutil.String(v)
	}
	var duration *string
	if v := args["duration"]; anyutil.IsTrue(v) {
		s := anyutil.String(v)
		duration, o.Duration = &s, s
	}
	o.Headed, o.Mode = anyutil.IsTrue(args["headed"]), mode
	local := func() (*jsonutil.Object, error) {
		if !checkMeetRequirements(call.Env) {
			return jsonutil.ObjectOf("ok", false, "error", "google_meet plugin prerequisites missing — install with "+
				"`icebath meet install`. Plugin is supported on Linux and macOS only."), nil
		}
		return pmStart(ctx, url, o)
	}
	return dispatchMeet(ctx, args["node"], "start_bot", func(c *nodeClient) (*jsonutil.Object, error) {
		return c.startBot(url, o.GuestName, duration, o.Headed, mode)
	}, local)
}

// Reports bot liveness and transcript progress
func handleMeetStatus(ctx context.Context, call *tools.Call) string {
	return dispatchMeet(ctx, call.Args["node"], "status", (*nodeClient).status,
		func() (*jsonutil.Object, error) { return pmStatus(ctx) })
}

// Last n of a transcript argument, nil when absent, unparsable or below one, a number past the int range held at its bound
func transcriptLastArg(v any) *int {
	if v == nil {
		return nil
	}
	n, err := anyutil.ToInt(v)
	if (err != nil && !errors.Is(err, anyutil.ErrRange)) || n < 1 {
		return nil
	}
	last := int(min(n, math.MaxInt))
	return &last
}

// Reads the transcript, the last n lines when last is set
func handleMeetTranscript(ctx context.Context, call *tools.Call) string {
	last := transcriptLastArg(call.Args["last"])
	return dispatchMeet(ctx, call.Args["node"], "transcript", func(c *nodeClient) (*jsonutil.Object, error) {
		return c.transcript(last)
	}, func() (*jsonutil.Object, error) {
		n := 0
		if last != nil {
			n = *last
		}
		return pmTranscript(ctx, n)
	})
}

// Signals the bot to leave cleanly
func handleMeetLeave(ctx context.Context, call *tools.Call) string {
	return dispatchMeet(ctx, call.Args["node"], "stop", (*nodeClient).stop,
		func() (*jsonutil.Object, error) { return pmStop(ctx, "agent called meet_leave") })
}

// Queues text for the realtime bridge to speak
func handleMeetSay(ctx context.Context, call *tools.Call) string {
	text := argText(call.Args, "text", "")
	if text == "" {
		return toolErr("text is required")
	}
	return dispatchMeet(ctx, call.Args["node"], "say", func(c *nodeClient) (*jsonutil.Object, error) {
		return c.say(text)
	}, func() (*jsonutil.Object, error) { return pmEnqueueSay(ctx, text) })
}
