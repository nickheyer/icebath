package app

import (
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

var c0AndDelRe = regexp.MustCompile("[\x00-\x1f\x7f]")

const warpProtocolVersion = 1

// Last Warp release per channel that advertised the protocol variable but could not render structured payloads
var warpLastBroken = map[string]string{"stable": "v0.2026.03.25.08.24.stable_05", "preview": "v0.2026.03.25.08.24.preview_05"}

// Writes raw escapes to the controlling tty, else the console, never failing
var writeTTY = func(seq string) {
	if tty, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		_, werr := io.WriteString(tty, seq)
		if cerr := tty.Close(); werr == nil && cerr == nil {
			return
		}
	}
	_, _ = io.WriteString(console().Out, seq)
}

// OSC 9 sequence with C0 controls and DEL stripped from the body
func osc9(body string) string {
	return "\x1b]9;" + c0AndDelRe.ReplaceAllString(body, "") + "\x07"
}

// Reports a Warp build that can render OSC 777 agent payloads
func warpSupported(getenv func(string) string) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	client := getenv("WARP_CLIENT_VERSION")
	if getenv("TERM_PROGRAM") != "WarpTerminal" || getenv("WARP_CLI_AGENT_PROTOCOL_VERSION") == "" || client == "" {
		return false
	}
	for channel, lastBroken := range warpLastBroken {
		if strings.Contains(client, channel) && client <= lastBroken {
			return false
		}
	}
	return true
}

// OSC 777 warp cli agent notification carrying a stop or permission_request event
func warpOSC777(event, detail, sessionID string) (string, error) {
	advertised := 1
	if raw, ok := os.LookupEnv("WARP_CLI_AGENT_PROTOCOL_VERSION"); ok {
		if n, parsed := pyIntOf(raw); parsed {
			advertised = n
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	payload := pyjson.NewObject()
	payload.Set("v", min(advertised, warpProtocolVersion))
	payload.Set("agent", "icebath")
	payload.Set("event", event)
	payload.Set("session_id", sessionID)
	payload.Set("cwd", cwd)
	payload.Set("project", pypath.Basename(cwd))
	key := "response"
	if event == "permission_request" {
		key = "summary"
	}
	payload.Set(key, runesHead(detail, 200))
	body, err := pyjson.Dumps(payload, pyjson.Separators(",", ":"))
	if err != nil {
		return "", err
	}
	return "\x1b]777;notify;warp://cli-agent;" + body + "\x07", nil
}

// OSC 9 plus the Warp OSC 777 payload when supported, for a blocking prompt or a turn end
func notificationSequence(text string, prompt bool, sessionID, detail string) (string, error) {
	seq := osc9("Icebath: " + text)
	if !warpSupported(nil) {
		return seq, nil
	}
	event := "stop"
	if prompt {
		event = "permission_request"
	}
	if detail == "" {
		detail = text
	}
	warp, err := warpOSC777(event, detail, sessionID)
	if err != nil {
		return "", err
	}
	return seq + warp, nil
}

// First n code points of the text
func runesHead(text string, n int) string {
	r := []rune(text)
	if len(r) <= n {
		return text
	}
	return string(r[:n])
}
