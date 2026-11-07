package googlemeet

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pyplatform"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

const pluginDescription = "Join a Google Meet call, transcribe live captions, speak in realtime, and follow up afterwards. v1 transcribe-only is the default; v2 realtime duplex audio via OpenAI Realtime + BlackHole/PulseAudio ships with mode='realtime'; v3 remote node host lets the bot run on a different machine than the gateway (gateway on Linux, Chrome+signed-in profile on the user's Mac). Explicit-by-design: only joins meet.google.com URLs passed in — no calendar scanning, no auto-dial."

func init() { plugins.RegisterBundled(Plugin{}) }

// Compiled-in google_meet plugin
type Plugin struct{}

// Name, schema, handler and emoji of each tool in registration order
var meetTools = []struct {
	name    string
	schema  map[string]any
	handler tools.Handler
	emoji   string
}{
	{"meet_join", meetJoinSchema, handleMeetJoin, "\U0001F4DE"},
	{"meet_status", meetStatusSchema, handleMeetStatus, "\U0001F7E2"},
	{"meet_transcript", meetTranscriptSchema, handleMeetTranscript, "\U0001F4DD"},
	{"meet_leave", meetLeaveSchema, handleMeetLeave, "\U0001F44B"},
	{"meet_say", meetSaySchema, handleMeetSay, "\U0001F5E3️"},
}

// Manifest of the google_meet plugin from its plugin.yaml
func (Plugin) Manifest() plugins.Manifest {
	return plugins.Manifest{
		Name:          "google_meet",
		Version:       "0.2.0",
		Description:   pluginDescription,
		Author:        "NousResearch",
		Kind:          "standalone",
		ProvidesTools: []string{"meet_join", "meet_leave", "meet_status", "meet_transcript", "meet_say"},
		ProvidesHooks: []string{"on_session_end"},
		Raw: map[string]any{"name": "google_meet", "version": "0.2.0", "description": pluginDescription,
			"author": "NousResearch", "kind": "standalone", "platforms": []any{"linux", "macos"},
			"provides_tools": []any{"meet_join", "meet_leave", "meet_status", "meet_transcript", "meet_say"},
			"provides_hooks": []any{"on_session_end"}},
	}
}

// Registers the five tools, the meet CLI and the session end hook on Linux and macOS
func (Plugin) Register(ctx *plugins.Context) error {
	system := strings.ToLower(pyplatform.System())
	if system != "linux" && system != "darwin" {
		slog.Info(fmt.Sprintf("google_meet plugin: platform=%s not supported (linux/macos only)", system))
		return nil
	}
	for _, t := range meetTools {
		if _, err := ctx.RegisterTool(context.Background(), plugins.ToolSpec{Name: t.name, Toolset: "google_meet",
			Schema: t.schema, Handler: t.handler, Check: checkMeetRequirements, Emoji: t.emoji}); err != nil {
			return err
		}
	}
	ctx.RegisterCLICommand("meet", "Google Meet bot (join, transcribe, follow up)", registerMeetCLI, meetHandler,
		"Let the icebath agent join a Google Meet call and scrape live "+
			"captions into a transcript. See: icebath meet setup")
	ctx.RegisterHook("on_session_end", onSessionEnd, false)
	return nil
}

// Stop the session end hook calls, swapped by tests
var sessionEndStop = pmStop

// Leaves a still running call so no headless Chromium is orphaned, logging any failure at debug
func onSessionEnd(ctx context.Context, _ map[string]any) (any, error) {
	status, err := pmStatus(ctx)
	if err == nil && pyrepr.Truthy(getOr(status, "ok", nil)) && pyrepr.Truthy(getOr(status, "alive", nil)) {
		_, err = sessionEndStop(ctx, "session ended")
	}
	if err != nil {
		slog.Debug(fmt.Sprintf("google_meet on_session_end cleanup failed: %v", err))
	}
	return nil, nil
}
