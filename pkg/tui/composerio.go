package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Shell command a prompt names between braces, replaced by what it prints
var interpolationRe = regexp.MustCompile(`\{!(.+?)\}`)

// Longest the clipboard image save may take
const clipboardImageLimit = 15 * time.Second

// Saves the clipboard image of the host to a PNG file, false when the clipboard holds none
var ClipboardImage func(dest string) bool

// Clipboard image saved to a temp file, or why there is none
type clipboardImageMsg struct {
	path        string
	dir         string
	err         error
	quiet       bool
	destination submissionDestination
	revision    int
}

// Number that changes whenever the draft is replaced, so late attachments of an older draft are dropped
func (a *app) composerRevision() int { return a.draftRevision }

// Follows what the draft holds, tokens whose labels were deleted detached on a legacy gateway
func (a *app) syncTokens() tea.Cmd {
	value := a.card.composer.Value()
	dropped := droppedTokens(a.tokens, value)
	if len(dropped) == 0 {
		return nil
	}
	var cmds []tea.Cmd
	kept := a.tokens[:0]
	for _, t := range a.tokens {
		gone := false
		for _, d := range dropped {
			if d == t {
				gone = true
			}
		}
		if !gone {
			kept = append(kept, t)
			continue
		}
		if t.Kind == "image" && t.Path != "" && !a.gw.canonical() && a.store.ui.Sid != "" {
			cmds = append(cmds, a.events.quietRequest("image.detach", map[string]any{"path": t.Path, "session_id": a.store.ui.Sid}))
		}
	}
	a.tokens = kept
	return tea.Batch(cmds...)
}

// Puts an attached image into the draft as its token, with the text that followed its path
func (a *app) attachImageToken(image *stagedImage) tea.Cmd {
	index := nextImageIndex(a.tokens)
	label := imageToken(index)
	a.tokens = trimTokens(append(a.tokens, composerToken{Index: index, Kind: "image", Label: label, Path: image.Path, Mime: image.Mime}))
	text := label
	if rest := trimJS(image.Remainder); rest != "" {
		text += " " + rest
	}
	if value := a.card.composer.Value(); value != "" && !strings.HasSuffix(value, " ") && !strings.HasSuffix(value, "\n") {
		text = " " + text
	}
	return a.card.composer.Insert(text + " ")
}

// Stages an image path with the session, staged by this client on the canonical wire and by the gateway on a legacy one
func (a *app) stageImage(path string, then func(image *stagedImage, err error) tea.Cmd) tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil {
		return then(nil, err)
	}
	revision := a.composerRevision()
	current := func() bool { return a.store.isCurrentDestination(destination) && revision == a.composerRevision() }
	if a.gw.canonical() {
		return a.store.stageImagePath(path, a.gw, destination, func(image *stagedImage, err error) tea.Cmd {
			if !current() {
				return nil
			}
			return then(image, err)
		})
	}
	return gatewayCall(a.gw, "image.attach", map[string]any{"path": path, "session_id": destination.Sid}, func(v any, err error) tea.Cmd {
		r := asRPCResult(v)
		if !current() {
			if attached, _ := r["path"].(string); attached != "" {
				return a.events.quietRequest("image.detach", map[string]any{"path": attached, "session_id": destination.Sid})
			}
			return nil
		}
		if err != nil {
			return then(nil, err)
		}
		name, _ := r["name"].(string)
		if name == "" {
			return then(nil, nil)
		}
		image := &stagedImage{Name: name}
		image.Path, _ = r["path"].(string)
		image.Mime, _ = r["mime"].(string)
		image.Remainder, _ = r["remainder"].(string)
		return then(image, nil)
	})
}

// Attaches the image at a path to the draft, as /image and a dropped file do
func (a *app) attachImagePath(path string) tea.Cmd {
	if a.store.ui.Sid == "" || trimJS(path) == "" {
		return nil
	}
	return a.stageImage(path, func(image *stagedImage, err error) tea.Cmd {
		if err != nil {
			a.sys("error: " + rpcErrorMessage(err))
			return nil
		}
		if image == nil {
			a.sys("error: no image at " + path)
			return nil
		}
		return a.attachImageToken(image)
	})
}

// Reads the clipboard image into a temp file off the update loop
func (a *app) pasteClipboardImage(quiet bool) tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil || destination.Sid == "" {
		return nil
	}
	revision := a.composerRevision()
	if !a.gw.canonical() {
		return gatewayCall(a.gw, "clipboard.paste", map[string]any{"session_id": destination.Sid}, func(v any, err error) tea.Cmd {
			if !a.store.isCurrentDestination(destination) || revision != a.composerRevision() {
				return nil
			}
			r := asRPCResult(v)
			if err != nil {
				if !quiet {
					a.sys(tr("canonical.queue.clipboardImageFailed", rpcErrorMessage(err)))
				}
				return nil
			}
			if !truthyJS(r["attached"]) {
				if !quiet {
					message, _ := r["message"].(string)
					if message == "" {
						message = "No image found in clipboard"
					}
					a.sys(message)
				}
				return nil
			}
			image := &stagedImage{}
			image.Path, _ = r["path"].(string)
			image.Mime, _ = r["mime"].(string)
			image.Name, _ = r["name"].(string)
			return a.attachImageToken(image)
		})
	}
	return func() tea.Msg {
		out := clipboardImageMsg{quiet: quiet, destination: destination, revision: revision}
		if ClipboardImage == nil {
			out.err = fmt.Errorf("this build has no clipboard image reader")
			return out
		}
		dir, err := os.MkdirTemp("", "icebath-clipboard-")
		if err != nil {
			out.err = err
			return out
		}
		out.dir = dir
		dest := filepath.Join(dir, "clipboard.png")
		ctx, cancel := context.WithTimeout(a.ctx, clipboardImageLimit)
		defer cancel()
		saved := make(chan bool, 1)
		go func() { saved <- ClipboardImage(dest) }()
		select {
		case ok := <-saved:
			if ok {
				out.path = dest
			}
		case <-ctx.Done():
			out.err = fmt.Errorf("reading the clipboard image took longer than %s", clipboardImageLimit)
		}
		return out
	}
}

// Stages the saved clipboard image and puts its token into the draft
func (a *app) onClipboardImage(m clipboardImageMsg) tea.Cmd {
	cleanup := func() {
		if m.dir != "" {
			_ = os.RemoveAll(m.dir)
		}
	}
	if !a.store.isCurrentDestination(m.destination) || m.revision != a.composerRevision() {
		cleanup()
		return nil
	}
	if m.err != nil {
		cleanup()
		if !m.quiet {
			a.sys(tr("canonical.queue.clipboardImageFailed", m.err.Error()))
		}
		return nil
	}
	if m.path == "" {
		cleanup()
		if !m.quiet {
			a.sys("No image found in clipboard")
		}
		return nil
	}
	return a.store.stageImagePath(m.path, a.gw, m.destination, func(image *stagedImage, err error) tea.Cmd {
		cleanup()
		if err != nil || image == nil {
			if !m.quiet && err != nil {
				a.sys(tr("canonical.queue.clipboardImageFailed", rpcErrorMessage(err)))
			}
			return nil
		}
		return a.attachImageToken(image)
	})
}

// Inserts pasted text, a long paste collapsed into a token the prompt expands when sent
func (a *app) insertPaste(cleaned string) tea.Cmd {
	lineCount := strings.Count(cleaned, "\n") + 1
	if !a.store.shouldCollapsePaste(cleaned, lineCount) {
		return a.card.composer.Insert(cleaned)
	}
	label := pasteTokenLabel(cleaned, lineCount)
	a.tokens = trimTokens(append(a.tokens, composerToken{Kind: "paste", Label: label, Text: cleaned}))
	collapse := gatewayCall(a.gw, "paste.collapse", map[string]any{"text": cleaned}, func(v any, err error) tea.Cmd {
		path, _ := asRPCResult(v)["path"].(string)
		for i := range a.tokens {
			if err == nil && path != "" && a.tokens[i].Kind == "paste" && a.tokens[i].Label == label && a.tokens[i].Text == cleaned {
				a.tokens[i].Path = path
			}
		}
		return nil
	})
	return tea.Batch(a.card.composer.Insert(label+" "), collapse)
}

// Takes a paste, an empty one as a clipboard image, a dropped path as an attachment, anything else as text
func (a *app) pasted(content string) tea.Cmd {
	cleaned := stripTrailingPasteNewlines(content)
	if strings.Trim(cleaned, "\n") == "" {
		return a.pasteClipboardImage(true)
	}
	sid := a.store.ui.Sid
	if sid == "" || !looksLikeDroppedPath(cleaned) {
		return a.insertPaste(cleaned)
	}
	destination, err := a.store.captureDestination()
	if err != nil {
		return a.insertPaste(cleaned)
	}
	revision := a.composerRevision()
	current := func() bool { return a.store.isCurrentDestination(destination) && revision == a.composerRevision() }
	return a.stageImage(cleaned, func(image *stagedImage, err error) tea.Cmd {
		if err == nil && image != nil {
			return a.attachImageToken(image)
		}
		return gatewayCall(a.gw, "input.detect_drop", map[string]any{"session_id": sid, "text": cleaned}, func(v any, err error) tea.Cmd {
			if !current() {
				return nil
			}
			r := asRPCResult(v)
			if text, _ := r["text"].(string); err == nil && truthyJS(r["matched"]) && text != "" {
				return a.card.composer.Insert(text)
			}
			return a.insertPaste(cleaned)
		})
	})
}

// Reads the system clipboard as text and pastes it, the clipboard image when it holds no usable text
func (a *app) pasteFromClipboard() tea.Cmd {
	return func() tea.Msg {
		text, _ := readClipboardText(a.ctx)
		return clipboardTextMsg{text: text}
	}
}

// Text of the system clipboard read off the update loop
type clipboardTextMsg struct{ text string }

// Drops every attachment and collapsed paste of the draft
func (a *app) clearAttachments() tea.Cmd {
	value := a.card.composer.Value()
	for _, t := range a.tokens {
		value = strings.ReplaceAll(value, t.Label, "")
	}
	set := a.card.composer.SetValue(strings.TrimSpace(value))
	return tea.Batch(set, a.syncTokens(), wm.Notify("attachments cleared", wm.Info))
}

// Opens the draft in the external editor, the screen handed over until it exits
func (a *app) openEditor() tea.Cmd {
	a.editorRevision = a.composerRevision()
	return openInEditor(expandPasteTokens(a.tokens)(a.card.composer.Value()), ".md")
}

// Takes the edited draft back, sent at once when the editor saved text
func (a *app) onEditorDone(m editorDoneMsg) tea.Cmd {
	if m.err != nil {
		a.sys("error: " + m.err.Error())
		return nil
	}
	if !m.ok || a.editorRevision != a.composerRevision() {
		return tea.ClearScreen
	}
	text := strings.TrimRight(m.text, "\n")
	if trimJS(text) == "" {
		return tea.ClearScreen
	}
	var kept []composerToken
	for _, t := range a.tokens {
		if t.Kind == "image" && strings.Contains(text, t.Label) {
			kept = append(kept, t)
		}
	}
	a.tokens = kept
	return tea.Batch(tea.ClearScreen, a.card.composer.SetValue(text))
}

// Runs every shell command a prompt names between braces and hands the prompt with their output to then
func (a *app) interpolate(text string, destination submissionDestination, item *queueItem, then func(result string) tea.Cmd) tea.Cmd {
	a.store.ui.Status = "interpolating…"
	matches := interpolationRe.FindAllStringSubmatchIndex(text, -1)
	results := make([]string, len(matches))
	pending := len(matches)
	finish := func() tea.Cmd {
		if !a.store.isCurrentDestination(destination) {
			item.settle(false)
			return nil
		}
		out := text
		for i := len(matches) - 1; i >= 0; i-- {
			out = out[:matches[i][0]] + results[i] + out[matches[i][1]:]
		}
		return then(out)
	}
	cmds := make([]tea.Cmd, len(matches))
	for i, m := range matches {
		index := i
		cmds[i] = gatewayCall(a.gw, "shell.exec", map[string]any{"command": text[m[2]:m[3]], "session_id": destination.Sid}, func(v any, err error) tea.Cmd {
			r := asRPCResult(v)
			switch {
			case err != nil:
				results[index] = "(error: " + describeShellExecError(err) + ")"
			case r == nil:
				results[index] = "(error: " + tr("session.common.invalidResponse", "shell.exec") + ")"
			default:
				var parts []string
				for _, k := range []string{"stdout", "stderr"} {
					if s, _ := r[k].(string); s != "" {
						parts = append(parts, s)
					}
				}
				results[index] = trimJS(strings.Join(parts, "\n"))
			}
			pending--
			if pending == 0 {
				return finish()
			}
			return nil
		})
	}
	return tea.Batch(cmds...)
}

// Sets the global details mode of the transcript and saves it
func (a *app) setDetails(mode string) tea.Cmd {
	ui := &a.store.ui
	ui.DetailsMode, ui.DetailsModeCommandOverride = mode, true
	a.card.view.invalidate()
	return tea.Batch(a.events.quietRequest("config.set", map[string]any{"key": "details_mode", "value": mode}), wm.Notify("details "+mode, wm.Info))
}

// Opens reasoning as it arrives or leaves it folded
func (a *app) toggleThinking() tea.Cmd {
	ui := &a.store.ui
	next := "expanded"
	if sectionMode("thinking", ui.DetailsMode, ui.Sections, ui.DetailsModeCommandOverride) == "expanded" {
		next = "collapsed"
	}
	sections := map[string]string{}
	for k, v := range ui.Sections {
		sections[k] = v
	}
	sections["thinking"] = next
	ui.Sections = sections
	a.card.view.invalidate()
	return tea.Batch(a.events.quietRequest("config.set", map[string]any{"key": "details_mode.thinking", "value": next}), wm.Notify("thinking "+next, wm.Info))
}

// Next way an input typed during a turn is admitted, queue, steer or interrupt
func (a *app) cycleBusyMode() tea.Cmd {
	modes := []string{"queue", "steer", "interrupt"}
	next := modes[0]
	for i, m := range modes {
		if m == a.store.ui.BusyInputMode {
			next = modes[(i+1)%len(modes)]
		}
	}
	a.store.ui.BusyInputMode = next
	return tea.Batch(a.events.quietRequest("config.set", map[string]any{"key": "busy", "value": next}), wm.Notify("busy input: "+next, wm.Info))
}

// Keys of the terminal UI context the session window does not answer itself
func (a *app) tuiAction(id string) (tea.Cmd, bool) {
	switch id {
	case "tui.model":
		return a.openModelPicker(), true
	case "tui.sessions":
		return a.openSessionSwitcher(), true
	case "tui.agents":
		return a.openAgents(), true
	case "tui.editor":
		return a.openEditor(), true
	case "tui.redraw":
		a.card.view.invalidate()
		return tea.ClearScreen, true
	case "tui.paste.image":
		return a.pasteClipboardImage(false), true
	case "tui.voice":
		return a.toggleVoiceRecording(), true
	case "tui.newline":
		return a.card.composer.Insert("\n"), true
	case "composer.paste":
		return a.pasteFromClipboard(), true
	}
	return nil, false
}

// Messages of the composer and widget machinery the root update hands on
func (a *app) updateExtras(m tea.Msg) (bool, tea.Cmd) {
	switch m := m.(type) {
	case clipboardImageMsg:
		return true, a.onClipboardImage(m)
	case clipboardTextMsg:
		if isUsableClipboardText(m.text) {
			return true, a.pasted(m.text)
		}
		return true, a.pasteClipboardImage(false)
	case widgetTickMsg:
		return true, a.widgets.onTick(m)
	case widgetsLoadedMsg:
		a.widgets.loaded = true
		a.card.composer.SetCommands(a.composerCommands())
		return true, nil
	case dieMsg:
		return true, a.die(m.code)
	}
	return false, nil
}

// Starts or stops voice recording of the session
func (a *app) toggleVoiceRecording() tea.Cmd {
	if !a.voiceEnabled {
		return wm.Notify("voice is off, /voice on turns it on", wm.Warn)
	}
	action := "start"
	if a.voiceRecording {
		action = "stop"
	}
	sid := a.store.ui.Sid
	return gatewayCall(a.gw, "voice.record", map[string]any{"action": action, "session_id": sid}, func(v any, err error) tea.Cmd {
		if err != nil {
			a.sys("error: " + rpcErrorMessage(err))
			return nil
		}
		a.voiceRecording = action == "start"
		a.voiceProcessing = action == "stop"
		return nil
	})
}
