package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/linktier"
	"github.com/nickheyer/icebath/pkg/ui/keys"
	uitheme "github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Pet card of the session window, its sprite laid over the transcript corner
type petCard interface {
	start() tea.Cmd
	update(m tea.Msg) (bool, tea.Cmd)
	observe(ev gatewayEventMsg) tea.Cmd
	follow() tea.Cmd
	place(w, h, top int) petPlacement
	origin(x, y int)
	holes() []wm.Box
}

// Voxel arena of the session's agents, fed by every update and gateway event
type arenaView interface {
	focus(m tea.Msg)
	observe(ev gatewayEventMsg)
	sync()
	retheme(t uitheme.Theme)
}

// Surfaces of the extras tier, bound by the files that carry pets and the arena and unset in a build without them
var (
	newPetCard   func(a *app) petCard
	newArenaView func(a *app) arenaView
	// Keymap contexts and local slash commands those files add
	tierKeys     []func(km *keys.Map)
	tierCommands []func(a *app) []slashCommand
)

// Local commands of the extras tier that drive gateway voice methods, left out of a build without voice
var extrasLocalCommands = map[string]bool{"wake": true, "voice": true}

// Whether a key action belongs to a tier this binary links, the voice recording key needing the extras tier
func keyLinked(id string) bool {
	return id != "tui.voice" || linktier.Linked(linktier.Extras)
}

// Whether a local command belongs to a tier this binary links, the voice commands and every slash command of a tier counted
func localCommandLinked(name string) bool {
	if tier, ok := linktier.SlashTiers[name]; ok && !linktier.Linked(tier) {
		return false
	}
	return !extrasLocalCommands[name] || linktier.Linked(linktier.Extras)
}

// Setup sections whose configuration lives in a tier above the core, text to speech and messaging in plugins, agent import in extras
var setupSectionTiers = map[string]string{"tts": linktier.Plugins, "gateway": linktier.Plugins, "import": linktier.Extras}

// Line naming the build that carries a setup section this binary leaves out, false for a linked or unknown section
func setupSectionRefusal(key string) (string, bool) {
	return linktier.Refusal("/setup "+key, setupSectionTiers[key])
}

// Quick Setup detail of this build, the messaging step named only when platforms are linked
func quickSetupDetail() string {
	if linktier.Linked(linktier.Plugins) {
		return tr("setupFlow.quick.detail")
	}
	return tr("setupFlow.quick.core_detail")
}

// Pet card of the app, the empty one in a build without pets
func petCardFor(a *app) petCard {
	if newPetCard == nil {
		return noPet{}
	}
	return newPetCard(a)
}

// Arena of the app, the empty one in a build without voxel rendering
func arenaViewFor(a *app) arenaView {
	if newArenaView == nil {
		return noArena{}
	}
	return newArenaView(a)
}

// Pet card of a build without pets, drawing nothing and taking no room
type noPet struct{}

func (noPet) start() tea.Cmd                   { return nil }
func (noPet) update(tea.Msg) (bool, tea.Cmd)   { return false, nil }
func (noPet) observe(gatewayEventMsg) tea.Cmd  { return nil }
func (noPet) follow() tea.Cmd                  { return nil }
func (noPet) place(int, int, int) petPlacement { return petPlacement{} }
func (noPet) origin(int, int)                  {}
func (noPet) holes() []wm.Box                  { return nil }

// Arena of a build without voxel rendering, keeping no journal
type noArena struct{}

func (noArena) focus(tea.Msg)           {}
func (noArena) observe(gatewayEventMsg) {}
func (noArena) sync()                   {}
func (noArena) retheme(uitheme.Theme)   {}

// Next frame of the pet animation
type petTickMsg struct{ gen int }

// Corner stack of the session window and the room it takes from the transcript
type petPlacement struct {
	rows   []string
	gutter int
	band   int
}

// Transcript rows with the band rows below them and the corner stack laid over their bottom right
func (pl petPlacement) over(body []string, w int) []string {
	if len(pl.rows) == 0 {
		return body
	}
	out := append(append([]string{}, body...), make([]string, pl.band)...)
	first := max(len(out)-len(pl.rows), 0)
	for i, row := range pl.rows {
		if first+i >= len(out) {
			break
		}
		keep := max(w-1-ansi.StringWidth(row), 0)
		left := ansi.Truncate(out[first+i], keep, "")
		out[first+i] = left + strings.Repeat(" ", max(keep-ansi.StringWidth(left), 0)) + row + " "
	}
	return out
}

// Takes a terminal answer about graphics, repainting when what the terminal draws changed
func (a *app) terminalGraphics(m tea.Msg) {
	if a.graphics.Update(m) {
		a.wm.Repaint()
	}
}

// Help line of /setup naming the sections and start over flows this binary carries
func setupHelpLine() string {
	keep := func(keys ...string) []string {
		var out []string
		for _, k := range keys {
			if _, refused := setupSectionRefusal(k); !refused {
				out = append(out, k)
			}
		}
		return out
	}
	flows := keep("quick", "blank", "import")
	last := flows[len(flows)-1]
	if len(flows) > 1 {
		last = strings.Join(flows[:len(flows)-1], ", ") + " or " + last
	}
	return "configure a section: " + strings.Join(keep("model", "aux", "agent", "tts", "terminal", "tools", "gateway"), ", ") + ", or start over with " + last
}
