// Sprint extension, a director, an architect, builders and a reviewer building one request as a team
package sprint

import (
	"github.com/nickheyer/icebath/pkg/ext"
	"github.com/nickheyer/icebath/pkg/harness"
	"github.com/nickheyer/icebath/pkg/tool"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

// Keys of the panels
const (
	ArenaKey    = "arena"
	BoardKey    = "sprints"
	SettingsKey = "sprint-settings"
)

// Keymap contexts of the arena and the board
const (
	ContextArena  = "sprint.arena"
	ContextTyping = "sprint.typing"
	ContextBoard  = "sprint.board"
)

// Actions of the arena
const (
	ActNext       = "sprint.select.next"
	ActPrev       = "sprint.select.prev"
	ActInspect    = "sprint.inspect"
	ActSteer      = "sprint.steer"
	ActLeave      = "sprint.leave"
	ActFollow     = "sprint.follow"
	ActBoard      = "sprint.board"
	ActLog        = "sprint.log"
	ActCustomize  = "sprint.customize"
	ActPause      = "sprint.pause"
	ActStop       = "sprint.stop"
	ActRestart    = "sprint.restart"
	ActOpen       = "sprint.open"
	ActKill       = "sprint.kill"
	ActOrbitLeft  = "sprint.orbit.left"
	ActOrbitRight = "sprint.orbit.right"
	ActOrbitUp    = "sprint.orbit.up"
	ActOrbitDown  = "sprint.orbit.down"
	ActZoomIn     = "sprint.zoom.in"
	ActZoomOut    = "sprint.zoom.out"
	ActCamera     = "sprint.camera"
	ActReset      = "sprint.camera.reset"
	ActLabels     = "sprint.labels"
	ActHelp       = "sprint.help"
	ActSprintNext = "sprint.next"
	ActSprintPrev = "sprint.prev"
	ActPageDown   = "sprint.page.down"
	ActPageUp     = "sprint.page.up"
	ActSettings   = "sprint.settings"
	ActSidebar    = "sprint.sidebar"
	ActWorld      = "sprint.world"
	ActView       = "sprint.view"
)

// Actions of the board
const (
	BoardOpen     = "sprint.board.open"
	BoardArena    = "sprint.board.arena"
	BoardStop     = "sprint.board.stop"
	BoardPause    = "sprint.board.pause"
	BoardResume   = "sprint.board.resume"
	BoardForget   = "sprint.board.forget"
	BoardSettings = "sprint.board.settings"
)

type extension struct{}

func (extension) Manifest() ext.Manifest {
	return ext.Manifest{Name: Name, Version: "4", Description: "sprint team in a voxel arena, a director settles the request, an architect cuts it into tickets, builders build them and a reviewer checks every handoff"}
}

// Declares the kinds, tools, budget hook, workflow, settings, panels and keys of the sprint team
func (extension) Attach(h *ext.Host) error {
	for _, k := range Default().Kinds("") {
		h.Kind(k)
	}
	for _, s := range Settings() {
		h.Setting(s)
	}
	h.Tools(func(a *harness.App) ([]tool.Tool, error) {
		if _, err := migrateLegacy(a); err != nil {
			return nil, err
		}
		if err := migratePixels(a); err != nil {
			return nil, err
		}
		if _, err := recoverSprints(a); err != nil {
			return nil, err
		}
		return sprintTools(a), nil
	})
	h.Hook(budgetHook)
	h.Workflow(workflow())
	h.Context(ContextArena, "the sprint arena")
	h.Context(ContextTyping, "the sprint arena while typing to a member")
	h.Context(ContextBoard, "the sprint board")
	arena := func(id, label, doc string, chords ...string) {
		h.Action(keys.Action{ID: id, Context: ContextArena, Label: label, Doc: doc, Default: chords})
	}
	arena(ActNext, "next member", "select the next member of the team", "tab")
	arena(ActPrev, "previous member", "select the previous member of the team", "shift+tab")
	arena(ActInspect, "inspect", "open or close the transcript of the selected member", "enter", "i")
	arena(ActSteer, "talk", "type to the selected member, steering its run or starting one", "s")
	arena(ActFollow, "follow", "keep the camera on the selected member or let it go", "f")
	arena(ActBoard, "tickets", "give the tickets the whole lower sidebar, or share it with the log again", "b")
	arena(ActLog, "log", "give the log the whole lower sidebar, or share it with the tickets again", "l")
	arena(ActCustomize, "customize", "rename or dress the selected member", "c")
	arena(ActPause, "pause", "pause the sprint before its next step, or resume it", "p")
	arena(ActStop, "stop", "stop the sprint, it can be restarted from the stage it was in", "x")
	arena(ActRestart, "restart", "restart a stopped or failed sprint from the stage it was in", "r")
	arena(ActOpen, "open session", "open the session of the selected member as its own window", "o")
	arena(ActKill, "stop run", "stop the run of the selected member, the sprint pausing for you to steer it", "k")
	arena(ActOrbitLeft, "orbit left", "orbit the camera left", "left")
	arena(ActOrbitRight, "orbit right", "orbit the camera right", "right")
	arena(ActOrbitUp, "tilt up", "tilt the camera up toward the horizon", "up")
	arena(ActOrbitDown, "tilt down", "tilt the camera down", "down")
	arena(ActZoomIn, "zoom in", "move the camera closer", "+", "=")
	arena(ActZoomOut, "zoom out", "move the camera away", "-")
	arena(ActCamera, "camera", "switch between the overview, following, orbiting and cinematic cameras", "v")
	arena(ActReset, "reset camera", "put the camera back where the preset starts", "0")
	arena(ActLabels, "labels", "show or hide the names over the team and the ids over the tickets", "n")
	arena(ActSidebar, "sidebar", "show or hide the sidebar with the team, the tickets and the log", "d")
	arena(ActWorld, "world", "move the team into the next world, the blackwall, the soulkiller, night city, the den or the session windows alone", "w")
	arena(ActView, "view", "pick the world, the camera, the labels, the display and the quality of the floor", "m")
	arena(ActHelp, "help", "show or hide the key legend", "?")
	arena(ActSprintNext, "next sprint", "show the next sprint", "]")
	arena(ActSprintPrev, "previous sprint", "show the previous sprint", "[")
	arena(ActPageDown, "scroll down", "scroll the tickets or the log down a page", "pgdown")
	arena(ActPageUp, "scroll up", "scroll the tickets or the log up a page", "pgup")
	arena(ActSettings, "settings", "open the sprint settings", "S")
	h.Action(keys.Action{ID: ActLeave, Context: ContextTyping, Label: "done typing", Doc: "return focus to the arena", Default: []string{"esc"}})
	board := func(id, label, doc string, chords ...string) {
		h.Action(keys.Action{ID: id, Context: ContextBoard, Label: label, Doc: doc, Default: chords})
	}
	board(BoardOpen, "open", "open the session of the member under the cursor, or the details of the row", "enter", "o")
	board(BoardArena, "arena", "show the sprint under the cursor in the arena", "a")
	board(BoardStop, "stop", "stop the sprint under the cursor", "x")
	board(BoardPause, "pause", "pause the sprint under the cursor, or resume it", "p")
	board(BoardResume, "restart", "restart a stopped or failed sprint from the stage it was in", "r")
	board(BoardForget, "forget", "forget an ended sprint, closing the sessions of its team", "D")
	board(BoardSettings, "settings", "open the sprint settings", "s")
	h.Panel(ext.Panel{Key: ArenaKey, Title: "Arena", Detail: "the sprint team at work, with the team, the tickets and the log beside the world", New: newArena})
	h.Panel(ext.Panel{Key: BoardKey, Title: "Sprints", Detail: "every sprint with its team and tickets", New: newBoard})
	h.Panel(ext.Panel{Key: SettingsKey, Title: "Sprint settings", Detail: "models, team, time limits, arena and outfits of the sprint team", New: newSettingsCard, Settings: true})
	return nil
}

func init() { ext.Register(extension{}) }
