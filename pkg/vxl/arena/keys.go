package arena

import "github.com/nickheyer/icebath/pkg/ui/keys"

// Keymap context in force while the arena holds the input
const Context = "arena"

// Actions of the arena card
const (
	ActNext        = "arena.next"
	ActPrev        = "arena.prev"
	ActInspect     = "arena.inspect"
	ActSteer       = "arena.steer"
	ActLeave       = "arena.leave"
	ActOpen        = "arena.open"
	ActKill        = "arena.kill"
	ActSidebar     = "arena.dock"
	ActBoard       = "arena.board"
	ActLog         = "arena.log"
	ActPageDown    = "arena.page.down"
	ActPageUp      = "arena.page.up"
	ActView        = "arena.view"
	ActWorld       = "arena.world"
	ActFollow      = "arena.follow"
	ActCamera      = "arena.camera"
	ActReset       = "arena.reset"
	ActOrbitLeft   = "arena.orbit.left"
	ActOrbitRight  = "arena.orbit.right"
	ActOrbitUp     = "arena.orbit.up"
	ActOrbitDown   = "arena.orbit.down"
	ActZoomIn      = "arena.zoom.in"
	ActZoomOut     = "arena.zoom.out"
	ActLabels      = "arena.labels"
	ActPause       = "arena.pause"
	ActStop        = "arena.stop"
	ActRestart     = "arena.restart"
	ActSprintNext  = "arena.sprint.next"
	ActSprintPrev  = "arena.sprint.prev"
	ActReplay      = "arena.replay"
	ActStepBack    = "arena.step.back"
	ActStepForward = "arena.step.forward"
	ActSpeed       = "arena.speed"
	ActLive        = "arena.live"
	ActHelp        = "arena.help"
)

// Every action of the arena in the order the legend lists them
var actions = []keys.Action{
	{ID: ActNext, Label: "next", Doc: "select the next member of the crew", Default: []string{"tab", "j"}},
	{ID: ActPrev, Label: "previous", Doc: "select the previous member of the crew", Default: []string{"shift+tab", "k"}},
	{ID: ActInspect, Label: "inspect", Doc: "show or hide everything the harness reports of the selected member", Default: []string{"i", "enter"}},
	{ID: ActSteer, Label: "steer", Doc: "send guidance to the selected member, a running subagent or the main agent", Default: []string{"s"}},
	{ID: ActLeave, Label: "leave", Doc: "close the legend, else clear the selection", Default: []string{"esc"}},
	{ID: ActOpen, Label: "open", Doc: "read the newest output of the selected subagent, or focus the session of the main agent", Default: []string{"o"}},
	{ID: ActKill, Label: "interrupt", Doc: "interrupt the selected subagent, or the turn when the main agent is selected", Default: []string{"x"}},
	{ID: ActSidebar, Label: "dock", Doc: "show or hide the dock of the crew, the tickets and the log", Default: []string{"d"}},
	{ID: ActBoard, Label: "tickets", Doc: "fold or open the tickets in the dock", Default: []string{"b"}},
	{ID: ActLog, Label: "log", Doc: "fold or open the log in the dock", Default: []string{"l"}},
	{ID: ActPageDown, Label: "page down", Doc: "scroll the inspector, else the log, down a page", Default: []string{"pgdown"}},
	{ID: ActPageUp, Label: "page up", Doc: "scroll the inspector, else the log, up a page", Default: []string{"pgup"}},
	{ID: ActView, Label: "view", Doc: "choose the display, glyphs, world, quality and camera of this arena", Default: []string{"v"}},
	{ID: ActWorld, Label: "world", Doc: "move the platform into the next world", Default: []string{"w"}},
	{ID: ActFollow, Label: "follow", Doc: "follow the selected member with the camera, or return to the preset", Default: []string{"f"}},
	{ID: ActCamera, Label: "camera", Doc: "switch to the next camera preset", Default: []string{"c"}},
	{ID: ActReset, Label: "reset view", Doc: "put the camera back where its preset starts", Default: []string{"0"}},
	{ID: ActOrbitLeft, Label: "turn left", Doc: "turn the camera left", Default: []string{"left"}, Quiet: true},
	{ID: ActOrbitRight, Label: "turn right", Doc: "turn the camera right", Default: []string{"right"}, Quiet: true},
	{ID: ActOrbitUp, Label: "tilt up", Doc: "tilt the camera up", Default: []string{"up"}, Quiet: true},
	{ID: ActOrbitDown, Label: "tilt down", Doc: "tilt the camera down", Default: []string{"down"}, Quiet: true},
	{ID: ActZoomIn, Label: "zoom in", Doc: "move the camera closer", Default: []string{"+", "="}, Quiet: true},
	{ID: ActZoomOut, Label: "zoom out", Doc: "move the camera away", Default: []string{"-"}, Quiet: true},
	{ID: ActLabels, Label: "labels", Doc: "show or hide the names and ticket ids over the floor", Default: []string{"t"}},
	{ID: ActPause, Label: "pause", Doc: "pause or resume spawning of new agents", Default: []string{"p"}},
	{ID: ActStop, Label: "stop", Doc: "interrupt the turn, or every agent still running in it", Default: []string{"X"}},
	{ID: ActRestart, Label: "retry", Doc: "run the prompt of the failed or stopped newest turn again", Default: []string{"R"}},
	{ID: ActSprintNext, Label: "next turn", Doc: "show the next turn of the session, past the newest back to following it", Default: []string{"]"}},
	{ID: ActSprintPrev, Label: "previous turn", Doc: "show the previous turn of the session", Default: []string{"["}},
	{ID: ActReplay, Label: "replay", Doc: "replay the turn shown from its start, or pause and resume the replay", Default: []string{"space"}},
	{ID: ActStepBack, Label: "step back", Doc: "move the replay to the change before", Default: []string{","}},
	{ID: ActStepForward, Label: "step", Doc: "move the replay to the next change", Default: []string{"."}},
	{ID: ActSpeed, Label: "speed", Doc: "play the replay at the next speed", Default: []string{">"}},
	{ID: ActLive, Label: "live", Doc: "leave the replay for the turn as it stands now", Default: []string{"L"}},
	{ID: ActHelp, Label: "keys", Doc: "show or hide every arena key", Default: []string{"?"}},
}

// Adds the arena context and its actions to a keymap
func RegisterKeys(km *keys.Map) {
	if keys.ContextDocs[Context] == "" {
		_ = keys.AddContext(Context, "arena card")
	}
	for _, a := range actions {
		a.Context = Context
		km.Register(a)
	}
}
