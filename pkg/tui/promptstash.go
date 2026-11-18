package tui

import (
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/ui/keys"
)

// Keymap context of the open stash panel, ahead of the session keys while it shows
const stashContext = "tui.stash"

// Composer drafts parked with ctrl+s, newest first, held in memory for the process as the REPL held them
type promptStash struct {
	*config.PromptStash
	start time.Time
	now   func() time.Time
}

// Seconds since the stash was made, the clock its entries are stamped with
func (s *promptStash) seconds() float64 { return s.now().Sub(s.start).Seconds() }

// Stash of the app, made on first use with its clock started then
func (a *app) promptStash() *promptStash {
	if a.stash == nil {
		s := &promptStash{start: a.store.now(), now: a.store.now}
		s.PromptStash = config.NewPromptStashWith(config.MaxStashItems, s.seconds)
		a.stash = s
	}
	return a.stash
}

// Ctrl+S on the composer and the keys of the open stash panel, registered with the keymap of the app
func registerStashKeys(km *keys.Map) {
	if keys.ContextDocs[stashContext] == "" {
		if err := keys.AddContext(stashContext, "prompt stash panel"); err != nil {
			panic(err)
		}
	}
	for _, a := range []keys.Action{
		{ID: "tui.stash", Context: tuiContext, Label: "stash", Doc: "stash the prompt, restore the stashed one, or browse several", Default: []string{"ctrl+s"}},
		{ID: "tui.stash.up", Context: stashContext, Label: "up", Doc: "highlight the newer stashed prompt", Default: []string{"up"}, Quiet: true},
		{ID: "tui.stash.down", Context: stashContext, Label: "down", Doc: "highlight the older stashed prompt", Default: []string{"down"}, Quiet: true},
		{ID: "tui.stash.restore", Context: stashContext, Label: "restore", Doc: "put the highlighted stashed prompt back into the composer", Default: []string{"enter"}},
		{ID: "tui.stash.delete", Context: stashContext, Label: "delete", Doc: "discard the highlighted stashed prompt", Default: []string{"d", "D"}},
		{ID: "tui.stash.close", Context: stashContext, Label: "close", Doc: "close the stash panel", Default: []string{"esc"}},
	} {
		km.Register(a)
	}
}

// Whether the stash panel shows its drafts
func (a *app) stashBrowsing() bool { return a.stash != nil && a.stash.PanelOpen && a.stash.Len() > 0 }

// Keys of the prompt stash, false for any other action
func (a *app) stashAction(id string) (tea.Cmd, bool) {
	switch id {
	case "tui.stash":
		return a.pressStash(), true
	case "tui.stash.up", "tui.stash.down", "tui.stash.restore", "tui.stash.delete", "tui.stash.close":
	default:
		return nil, false
	}
	s := a.promptStash()
	switch id {
	case "tui.stash.up":
		s.MoveCursor(-1)
	case "tui.stash.down":
		s.MoveCursor(1)
	case "tui.stash.delete":
		s.DeleteAtCursor()
	case "tui.stash.close":
		s.ClosePanel()
	case "tui.stash.restore":
		if text, attached, ok := s.RestoreAtCursor(); ok {
			return a.restoreDraft(text, attached), true
		}
	}
	return nil, true
}

// One ctrl+s press, parking the draft, bringing back the only parked one, or opening and closing the panel of several
func (a *app) pressStash() tea.Cmd {
	if a.dialogs.waiting() {
		return nil
	}
	value := a.card.composer.Value()
	var attached []any
	for _, t := range a.tokens {
		if strings.Contains(value, t.Label) {
			attached = append(attached, t)
		}
	}
	action, payload := config.ResolveCtrlS(a.promptStash().PromptStash, value, attached)
	switch action {
	case config.ActionStashed:
		if a.queueEdit != nil {
			a.queueEdit = nil
			if err := a.queue.setQueueEdit(nil); err != nil {
				a.sys("error: " + err.Error())
			}
		}
		cleared := a.card.composer.SetValue("")
		detached := a.syncTokens()
		a.draftRevision++
		return tea.Batch(cleared, detached)
	case config.ActionRestored:
		return a.restoreDraft(payload.Text, payload.Images)
	}
	return nil
}

// Puts a parked draft back into the composer, images of the current draft kept beside it as the REPL did
func (a *app) restoreDraft(text string, attached []any) tea.Cmd {
	restored := make([]composerToken, 0, len(attached))
	for _, item := range attached {
		if t, isToken := item.(composerToken); isToken {
			restored = append(restored, t)
		}
	}
	current := a.card.composer.Value()
	var kept []composerToken
	for _, t := range a.tokens {
		same := func(r composerToken) bool { return r.Kind == t.Kind && r.Path == t.Path }
		if t.Kind == "image" && strings.Contains(current, t.Label) && !slices.ContainsFunc(restored, same) {
			kept = append(kept, t)
		}
	}
	next := nextImageIndex(append(slices.Clone(restored), kept...))
	value := text
	for i := range kept {
		label := kept[i].Label
		if slices.ContainsFunc(restored, func(r composerToken) bool { return r.Label == label }) {
			kept[i].Index, kept[i].Label = next, imageToken(next)
			next++
		}
		if trimJS(value) == "" {
			value = kept[i].Label
		} else {
			value = strings.TrimRightFunc(value, isJSSpace) + " " + kept[i].Label
		}
	}
	a.tokens = trimTokens(append(restored, kept...))
	a.draftRevision++
	cmds := []tea.Cmd{a.card.composer.SetValue(value)}
	if sid := a.store.ui.Sid; !a.gw.canonical() && sid != "" {
		for _, t := range restored {
			if t.Kind == "image" && t.Path != "" {
				cmds = append(cmds, a.events.rpc("image.attach", map[string]any{"path": t.Path, "session_id": sid}, nil))
			}
		}
	}
	return tea.Batch(cmds...)
}

// Age of a parked draft as the REPL panel words it
func stashAge(seconds float64) string {
	secs := int(seconds)
	switch {
	case secs < 10:
		return tr("promptStash.justNow")
	case secs < 90:
		return tr("promptStash.agoSeconds", secs)
	}
	mins := secs / 60
	if mins < 60 {
		return tr("promptStash.agoMinutes", mins)
	}
	return tr("promptStash.agoHours", mins/60)
}

// Box of the parked drafts above the status row while browsing, the highlighted one on the selection chip
func (a *app) stashPanelRows(width int, rc renderCtx) []string {
	if !a.stashBrowsing() {
		return nil
	}
	t := rc.t
	s := a.stash
	cw := ansi.StringWidth
	box := max(12, min(width-4, 80))
	items := s.Items()
	n := len(items)
	header := tr("promptStash.headerOther", n)
	if n == 1 {
		header = tr("promptStash.headerOne", n)
	}
	headPrefix, headSuffix := "╭─ "+header+" ", " Ctrl+S ─╮"
	footPrefix, footSuffix := "╰", " "+tr("promptStash.footer")+" ─╯"
	if cw(headPrefix)+cw(headSuffix) > box {
		headPrefix, headSuffix = "╭─ 📌 "+strconv.Itoa(n)+" ", "─╮"
	}
	if cw(footPrefix)+cw(footSuffix) > box {
		footSuffix = " " + tr("promptStash.footerCompact") + " ─╯"
	}
	if cw(footPrefix)+cw(footSuffix) > box {
		footSuffix = "─╯"
	}
	border, sub := toneStyle(t.Color.Border), toneStyle(t.Color.Muted)
	chip := toneStyle(t.Color.Accent).Bold(true).Background(toneColor(t.Color.SelectionBg))
	inner := box - 2
	rows := []string{"  " + border.Render(truncateEnd(headPrefix+strings.Repeat("─", max(0, box-cw(headPrefix)-cw(headSuffix)))+headSuffix, box))}
	for i, item := range items {
		marker := " "
		if i == s.PanelCursor {
			marker = "►"
		}
		age := stashAge(s.seconds() - item.StashedAt)
		prefix := " " + marker + " [" + strconv.Itoa(i+1) + "] " + age + strings.Repeat(" ", max(0, 10-cw(age))) + " "
		if cw(prefix) > inner-2 {
			prefix = " " + marker + " [" + strconv.Itoa(i+1) + "] "
		}
		room := max(0, inner-cw(prefix)-1)
		preview := truncateEnd(item.Preview, room)
		preview += strings.Repeat(" ", max(0, room-cw(preview)))
		if i == s.PanelCursor {
			rows = append(rows, "  "+chip.Render(truncateEnd("│"+prefix+preview+" │", box)))
			continue
		}
		rows = append(rows, "  "+border.Render("│")+sub.Render(prefix+preview+" ")+border.Render("│"))
	}
	return append(rows, "  "+border.Render(truncateEnd(footPrefix+strings.Repeat("─", max(0, box-cw(footPrefix)-cw(footSuffix)))+footSuffix, box)))
}

// Pin of the parked drafts for the status bar, empty when nothing is parked or the stash field is hidden
func (a *app) stashIndicator() string {
	ui := a.store.ui
	if a.stash == nil || ui.StatusBar == "off" || (len(ui.StatusBarFields) > 0 && !ui.StatusBarFields["stash"]) {
		return ""
	}
	return a.stash.Indicator()
}

// Placeholder of the empty composer, the English REPL stash hint while idle with parked drafts, else the launch hint
func (a *app) composerPlaceholder() string {
	if a.stash != nil && !a.store.ui.Busy && !a.voiceEnabled {
		if hint := a.stash.ComposerHint(); hint != "" {
			return hint
		}
	}
	return composerHint()
}
