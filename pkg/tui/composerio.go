package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/i18n"
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/clip"
	"github.com/nickheyer/icebath/pkg/ui/tty"
	"github.com/nickheyer/icebath/pkg/ui/txt"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Shell command a prompt names between braces, replaced by what it prints
var interpolationRe = regexp.MustCompile(`\{!(.+?)\}`)

// Longest the clipboard image save may take
const clipboardImageLimit = 15 * time.Second

const (
	// Quiet time after an escape closed something, in which escape leaves the turn running
	escapeAfterClose = 300 * time.Millisecond
	// Time a first escape on an idle empty prompt keeps the rewind armed for the second
	rewindArmWindow = time.Second
	// Longest the clipboard write of a cut may take before the selection is left in place
	cutWriteLimit = 2 * time.Second
)

// Clipboard write of a cut finished, the span it copied and whether the clipboard took it
type cutDoneMsg struct {
	span    cards.Selection
	written bool
}

// Saves the clipboard image of the host to a PNG file, false when the clipboard holds none
var ClipboardImage func(dest string) bool

// Program this host lacks to read clipboard images, or that no desktop clipboard is reachable
var ClipboardImageBlocker func() (missing string, noDesktop bool)

// Clipboard image saved to a temp file, or why there is none
type clipboardImageMsg struct {
	path        string
	dir         string
	err         error
	missing     string
	noDesktop   bool
	quiet       bool
	destination submissionDestination
	revision    int
}

// Number that changes whenever the draft is replaced, so late attachments of an older draft are dropped
func (a *app) composerRevision() int { return a.card.composer.Revision() }

// Follows what the draft holds, dropping tokens whose labels were deleted; the draft and edited entries the history ring keeps out of view hold on
// to their tokens
func (a *app) syncTokens() {
	value := strings.Join(append([]string{a.card.composer.Value()}, a.card.composer.Kept()...), "\n")
	dropped := droppedTokens(a.tokens, value)
	if len(dropped) == 0 {
		return
	}
	kept := a.tokens[:0]
	for _, t := range a.tokens {
		if !slices.Contains(dropped, t) {
			kept = append(kept, t)
		}
	}
	a.tokens = kept
}

// Takes an attached image into the tokens of the draft and returns the text that stands for it, its label and what followed its path
func (a *app) imageTokenText(image *stagedImage) string {
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
	return text + " "
}

// Puts an attached image into the draft as its token, with the text that followed its path
func (a *app) attachImageToken(image *stagedImage) tea.Cmd {
	return a.card.composer.Insert(a.imageTokenText(image))
}

// Stages an image path with the session, the answer dropped once the draft or the session it was asked for is gone
func (a *app) stageImage(path string, then func(image *stagedImage, err error) tea.Cmd) tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil {
		return then(nil, err)
	}
	revision := a.composerRevision()
	current := func() bool { return a.store.isCurrentDestination(destination) && revision == a.composerRevision() }
	return a.stageImageWhile(path, destination, current, func() tea.Cmd { return nil }, then)
}

// Stages an image path with a session, stale running in place of then when current no longer holds as the answer arrives
func (a *app) stageImageWhile(path string, destination submissionDestination, current func() bool, stale func() tea.Cmd, then func(image *stagedImage, err error) tea.Cmd) tea.Cmd {
	return a.store.stageImagePath(path, a.gw, destination, func(image *stagedImage, err error) tea.Cmd {
		if !current() {
			return stale()
		}
		return then(image, err)
	})
}

// Attaches the image at a path to the draft, as /image and a dropped file do, every page of a PDF as its own image
func (a *app) attachImagePath(path string) tea.Cmd {
	if a.store.ui.Sid == "" || trimJS(path) == "" {
		return nil
	}
	if pdf, pages, isPDF := isPDFPath(path, a.store.typedPathBase()); isPDF {
		return a.attachPDFPages(pdf, pages)
	}
	return a.stageImage(path, func(image *stagedImage, err error) tea.Cmd {
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
			return nil
		}
		if image == nil {
			a.sys(errorLine(tr(txt.ComposerNoImageAt, path)))
			return nil
		}
		return a.attachImageToken(image)
	})
}

// Attaches the images of an edited prompt again, one after another at the end of the draft so text typed meanwhile stays ahead of them
func (a *app) restoreImages(paths []string) tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil || destination.Sid == "" {
		return nil
	}
	return a.restoreImage(paths, 0, destination, a.composerRevision(), a.card.composer.Value())
}

// One step of an image restore, done counting the images put back and base the draft as the restore last left it: a draft cleared, sent, replaced
// or edited inside since ends the restore with the number of images left behind
func (a *app) restoreImage(paths []string, done int, destination submissionDestination, revision int, base string) tea.Cmd {
	if done >= len(paths) || trimJS(paths[done]) == "" {
		return nil
	}
	current := func() bool {
		return a.store.isCurrentDestination(destination) && revision == a.composerRevision() && strings.HasPrefix(a.card.composer.Value(), base)
	}
	dropped := func() tea.Cmd {
		if a.store.isCurrentDestination(destination) {
			a.sys(tr(txt.CanonicalQueueImagesNotRestored, len(paths)-done))
		}
		return nil
	}
	if !current() {
		return dropped()
	}
	return a.stageImageWhile(paths[done], destination, current, dropped, func(image *stagedImage, err error) tea.Cmd {
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
		}
		if err != nil || image == nil {
			return nil
		}
		landed := a.card.composer.Append(a.imageTokenText(image))
		return tea.Batch(landed, a.restoreImage(paths, done+1, destination, revision, a.card.composer.Value()))
	})
}

// Reads the clipboard image into a temp file off the update loop
func (a *app) pasteClipboardImage(quiet bool) tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil || destination.Sid == "" {
		return nil
	}
	revision := a.composerRevision()
	return func() tea.Msg {
		out := clipboardImageMsg{quiet: quiet, destination: destination, revision: revision}
		if ClipboardImage == nil {
			out.err = errors.New(tr(txt.ComposerNoClipboardReader))
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
			} else if ClipboardImageBlocker != nil {
				out.missing, out.noDesktop = ClipboardImageBlocker()
			}
		case <-ctx.Done():
			out.err = errors.New(tr(txt.ComposerClipboardImageSlow, clipboardImageLimit))
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
			a.sys(tr(txt.CanonicalQueueClipboardImageFailed, m.err.Error()))
		}
		return nil
	}
	if m.path == "" {
		cleanup()
		switch {
		case m.quiet:
		case (m.missing != "" || m.noDesktop) && a.clipboardWarned:
			return wm.Notify(tr(txt.ComposerNoClipboardImage), wm.Info)
		case m.missing != "":
			a.clipboardWarned = true
			a.sys(tr(txt.ComposerClipboardImageTool, m.missing))
		case m.noDesktop:
			a.clipboardWarned = true
			a.sys(tr(txt.ComposerClipboardNoDesktop))
		default:
			a.sys(tr(txt.ComposerNoClipboardImage))
		}
		return nil
	}
	return a.store.stageImagePath(m.path, a.gw, m.destination, func(image *stagedImage, err error) tea.Cmd {
		cleanup()
		if err != nil || image == nil {
			if !m.quiet && err != nil {
				a.sys(tr(txt.CanonicalQueueClipboardImageFailed, rpcErrorMessage(err)))
			}
			return nil
		}
		return a.attachImageToken(image)
	})
}

// Inserts pasted text, a long paste collapsed into a token the prompt expands when sent unless it lands inside an expanded paste, whose text it joins
func (a *app) insertPaste(cleaned string) tea.Cmd {
	lineCount := strings.Count(cleaned, "\n") + 1
	if a.inExpandedPaste() || !a.store.shouldCollapsePaste(cleaned, lineCount) {
		return a.card.composer.Insert(cleaned)
	}
	label := pasteTokenLabel(cleaned, lineCount)
	a.tokens = trimTokens(append(a.tokens, composerToken{Kind: "paste", Label: label, Text: cleaned}))
	collapse := call(a.gw, tuigateway.PasteCollapse, tuigateway.PasteCollapseParams{Text: cleaned}, func(r *tuigateway.PasteCollapseResult, err error) tea.Cmd {
		for i := range a.tokens {
			if err == nil && r.Path != "" && a.tokens[i].Kind == "paste" && a.tokens[i].Label == label && a.tokens[i].Text == cleaned {
				a.tokens[i].Path = r.Path
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
	if pdf, pages, isPDF := isPDFPath(cleaned, a.store.typedPathBase()); isPDF {
		return tea.Batch(a.dropImageCommand(), a.attachPDFPages(pdf, pages))
	}
	return a.stageImage(cleaned, func(image *stagedImage, err error) tea.Cmd {
		if err == nil && image != nil {
			return tea.Batch(a.dropImageCommand(), a.attachImageToken(image))
		}
		return call(a.gw, tuigateway.InputDetectDrop, tuigateway.InputDetectDropParams{Text: cleaned, Cwd: a.store.typedPathBase()}, func(r *tuigateway.InputDetectDropResult, err error) tea.Cmd {
			if !current() {
				return nil
			}
			if err == nil && r.Matched && r.Text != "" {
				return a.card.composer.Insert(r.Text)
			}
			return a.insertPaste(cleaned)
		})
	})
}

// Drops a "/image " typed right before the cursor once a pasted path attached its file, so the command word is neither sent to the model
// as text nor run as a command over the attachment token
func (a *app) dropImageCommand() tea.Cmd {
	composer := a.card.composer
	runes := []rune(composer.Value())
	at := min(max(composer.Offset(), 0), len(runes))
	head := strings.TrimRight(string(runes[:at]), " \t")
	if !strings.HasSuffix(head, "/image") {
		return nil
	}
	cut := len([]rune(head)) - len([]rune("/image"))
	if cut > 0 && !unicode.IsSpace(runes[cut-1]) {
		return nil
	}
	return composer.SetValueAt(string(runes[:cut])+string(runes[at:]), cut)
}

// Longest the terminal may take to answer a clipboard query before the paste goes on without it
const terminalClipboardWait = 500 * time.Millisecond

// Paste waiting on the clipboard, the draft and session it was asked for and which of the two sources still has to be read
type clipboardRead struct {
	id          int
	destination submissionDestination
	captured    bool
	revision    int
	toolRead    bool
	terminalAsk bool
}

// Text of the system clipboard read off the update loop
type clipboardTextMsg struct {
	id   int
	text string
}

// The terminal did not answer a clipboard query in time
type clipboardWaitedMsg struct{ id int }

// Reads the clipboard as text and pastes it, the clipboard image when it holds no usable text: the terminal's clipboard first over SSH, where the
// tool of this machine reads the far clipboard, the tool first anywhere else
func (a *app) pasteFromClipboard() tea.Cmd {
	a.clipboardSeq++
	read := &clipboardRead{id: a.clipboardSeq, revision: a.composerRevision()}
	if destination, err := a.store.captureDestination(); err == nil {
		read.destination, read.captured = destination, true
	}
	a.clipboardRead = read
	if isRemoteShellSession(os.LookupEnv) {
		return a.askTerminalClipboard(read)
	}
	return a.readClipboardTool(read)
}

// Reads the clipboard through the tool of this machine
func (a *app) readClipboardTool(read *clipboardRead) tea.Cmd {
	read.toolRead = true
	id := read.id
	return func() tea.Msg {
		text, _ := readClipboardText(a.ctx)
		return clipboardTextMsg{id: id, text: text}
	}
}

// Asks the terminal for its clipboard, the wait ending the query when the terminal stays silent
func (a *app) askTerminalClipboard(read *clipboardRead) tea.Cmd {
	read.terminalAsk = true
	id := read.id
	return tea.Batch(clip.Query(), tea.Tick(terminalClipboardWait, func(time.Time) tea.Msg { return clipboardWaitedMsg{id: id} }))
}

// Takes what one clipboard source held: usable text is pasted, anything else moves on to the other source and then to the clipboard image
func (a *app) clipboardAnswered(id int, text string) tea.Cmd {
	read := a.clipboardRead
	if read == nil || read.id != id {
		return nil
	}
	if read.revision != a.composerRevision() || (read.captured && !a.store.isCurrentDestination(read.destination)) {
		a.clipboardRead = nil
		return nil
	}
	switch {
	case isUsableClipboardText(text):
		a.clipboardRead = nil
		return a.pasted(text)
	case !read.toolRead:
		return a.readClipboardTool(read)
	case !read.terminalAsk:
		return a.askTerminalClipboard(read)
	}
	a.clipboardRead = nil
	return a.pasteClipboardImage(false)
}

// Whether a clipboard message of the terminal answers the paste in flight, taking it when it does
func (a *app) terminalClipboardAnswer(m tea.ClipboardMsg) (tea.Cmd, bool) {
	read := a.clipboardRead
	if read == nil || !read.terminalAsk {
		return nil, false
	}
	return a.clipboardAnswered(read.id, m.Content), true
}

// Drops every attachment and collapsed paste of the draft
func (a *app) clearAttachments() tea.Cmd {
	value := a.card.composer.Value()
	for _, t := range a.tokens {
		value = strings.ReplaceAll(value, t.Label, "")
	}
	set := a.card.composer.SetValue(strings.TrimSpace(value))
	a.syncTokens()
	return tea.Batch(set, wm.Notify(tr(txt.ComposerAttachmentsCleared), wm.Info))
}

// Opens the draft in the external editor, the screen handed over until it exits
func (a *app) openEditor() tea.Cmd {
	destination, err := a.store.captureDestination()
	if err != nil {
		a.sys(tr(txt.SessionInputFailedToOpenEditorWith, err.Error()))
		return nil
	}
	a.editorRevision, a.editorTarget = a.composerRevision(), destination
	return openInEditor(expandPasteTokens(a.tokens)(a.card.composer.Value()), ".md")
}

// Sends the saved draft, queued for its own session instead when the draft or session changed meanwhile
func (a *app) onEditorDone(m editorDoneMsg) tea.Cmd {
	if m.err != nil {
		a.sys(tr(txt.SessionInputFailedToOpenEditorWith, m.err.Error()))
		return nil
	}
	text := strings.TrimRightFunc(m.text, isJSSpace)
	if !m.ok || text == "" {
		return a.store.clearScreen()
	}
	if !a.store.isCurrentDestination(a.editorTarget) || a.editorRevision != a.composerRevision() {
		a.enqueue(text, text, a.editorTarget)
		return a.store.clearScreen()
	}
	var kept []composerToken
	for _, t := range a.tokens {
		if t.Kind == "image" && strings.Contains(text, t.Label) {
			kept = append(kept, t)
		}
	}
	a.tokens = kept
	emptied := a.card.composer.SetValue("")
	return tea.Batch(a.store.clearScreen(), emptied, a.submit(text))
}

// Runs the shell commands a person typed between braces and hands their output on in order, one that cannot run refusing the whole send, its input
// kept in the queue as unconfirmed so the model never reads a stand-in for output the person expected
func (a *app) interpolate(typed []typedInterpolation, destination submissionDestination, item *queueItem, then func(outputs []string) tea.Cmd) tea.Cmd {
	a.store.ui.Status = tr(txt.SessionStatusInterpolating)
	outputs := make([]string, len(typed))
	pending, refused := len(typed), false
	cmds := make([]tea.Cmd, len(typed))
	for i, t := range typed {
		params := tuigateway.CanonicalShellExecParams{Command: t.command, SessionID: destination.Sid}
		cmds[i] = call(a.gw, tuigateway.CanonicalShellExec, params, func(r *tuigateway.ShellExecResult, err error) tea.Cmd {
			pending--
			switch {
			case refused:
				return nil
			case err != nil:
				refused = true
				item.settle(false)
				if a.store.isCurrentDestination(destination) {
					a.sys(tr(txt.SessionCommonInterpolationFailed, describeShellExecError(err)))
					a.store.setBusy(false)
					a.store.ui.Status = tr(txt.StatusReady)
				}
				return nil
			}
			outputs[i] = trimJS(strings.Join(slices.DeleteFunc([]string{r.Stdout, r.Stderr}, func(s string) bool { return s == "" }), "\n"))
			if pending > 0 {
				return nil
			}
			return then(outputs)
		})
	}
	return tea.Batch(cmds...)
}

// Sets the details mode of the transcript and of every section of it and saves it
func (a *app) setDetails(mode string) tea.Cmd {
	ui := &a.store.ui
	sections := map[string]string{}
	for _, section := range sectionNames {
		sections[section] = mode
	}
	ui.DetailsMode, ui.DetailsModeCommandOverride, ui.Sections = mode, true, sections
	a.card.view.invalidate()
	return tea.Batch(a.saveSetting("details_mode", mode), wm.Notify(tr(txt.ComposerDetailsMode, mode), wm.Info))
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
	return tea.Batch(a.saveSetting("details_mode.thinking", next), wm.Notify(tr(txt.ComposerThinkingMode, next), wm.Info))
}

// Turns yolo of the session in view on or off without spending a turn
func (a *app) toggleYolo() tea.Cmd {
	sid := a.store.ui.Sid
	if sid == "" {
		a.sys(tr(txt.SessionInputYoloNeedsSession))
		return nil
	}
	return call(a.gw, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: "yolo", SessionID: sid}, func(res *tuigateway.ConfigSetResult, err error) tea.Cmd {
		switch {
		case err != nil:
			a.sys(errorLine(rpcErrorMessage(err)))
		case setWord(res) == "1":
			a.store.ui.Yolo = true
			a.sys(tr(txt.SessionInputYoloOn))
		case setWord(res) == "0":
			a.store.ui.Yolo = false
			a.sys(tr(txt.SessionInputYoloOff))
		default:
			a.sys(tr(txt.SessionInputYoloToggleFailed))
		}
		return nil
	})
}

// Empties the composer with everything that rode on its draft, the tokens, an expanded paste, a queued input held for editing and a recalled history line
func (a *app) clearIn() tea.Cmd {
	a.clearComposerTokens()
	a.pasteSpan = nil
	a.setQueueEdit(nil)
	return a.card.composer.SetValue("")
}

// Keeps the expanded paste on its text across whatever edited the draft since the span was last looked at. Text put in at the cursor inside the
// span or at its very end joins it even where the draft alone cannot tell, a space typed before a space
func (a *app) followPaste() {
	if a.pasteSpan == nil {
		return
	}
	value := a.card.composer.Value()
	if value == a.pasteDraft {
		return
	}
	prev, next, span := []rune(a.pasteDraft), []rune(value), *a.pasteSpan
	a.pasteDraft = value
	grown := len(next) - len(prev)
	if at := a.card.composer.Offset() - grown; grown > 0 && at > span.start && at <= min(span.end, len(prev)) && slices.Equal(next[:at], prev[:at]) && slices.Equal(next[at+grown:], prev[at:]) {
		a.pasteSpan = &pasteSpan{start: span.start, end: span.end + grown}
		return
	}
	a.pasteSpan = shiftSpan(prev, next, span)
}

// Whether the cursor sits inside the expanded paste, where a paste joins its text instead of becoming a chip of its own
func (a *app) inExpandedPaste() bool {
	a.followPaste()
	return insideSpan(a.pasteSpan, a.card.composer.Offset())
}

// Paste key on the draft: an expanded paste folds back into a chip, else the paste chip under the cursor, or the last one, expands into text to
// read and edit, false when the draft holds neither
func (a *app) togglePaste() (tea.Cmd, bool) {
	if a.followPaste(); a.pasteSpan != nil {
		return a.collapsePaste(), true
	}
	composer := a.card.composer
	expanded, found := expandPasteChip(composer.Value(), composer.Offset(), a.tokens)
	if !found {
		return nil, false
	}
	a.tokens = expanded.tokens
	placed := composer.SetValueAt(expanded.value, expanded.cursor)
	a.pasteSpan, a.pasteDraft = &expanded.span, expanded.value
	return placed, true
}

// Folds the expanded paste back into a chip holding the text as it reads now. A span another chip landed in is not folded, its text staying in
// the draft as typed text
func (a *app) collapsePaste() tea.Cmd {
	a.followPaste()
	span := a.pasteSpan
	if a.pasteSpan = nil; span == nil {
		return nil
	}
	composer := a.card.composer
	collapsed, folded := collapsePasteSpan(composer.Value(), *span, a.tokens)
	if !folded {
		return nil
	}
	a.tokens = collapsed.tokens
	return composer.SetValueAt(collapsed.value, collapsed.cursor)
}

// Enter on an empty prompt, which only drops an armed rewind; false when the prompt holds something to send
func (a *app) emptyEnter() (tea.Cmd, bool) {
	composer := a.card.composer
	if trimJS(composer.Value()) != "" || !composer.Empty() {
		return nil, false
	}
	a.cancelPendingRewind(true)
	return nil, true
}

// Whether prompt text is selected
func (a *app) selecting() bool {
	_, selected := a.card.composer.Selection()
	return selected
}

// Cuts the selected prompt text, the text removed only once the clipboard took it
func (a *app) cutSelection() tea.Cmd {
	span, selected := a.card.composer.Selection()
	if !selected {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(a.ctx, cutWriteLimit)
		defer cancel()
		return cutDoneMsg{span: span, written: clip.WriteText(ctx, span.Text)}
	}
}

// Saves one display setting word, its answer and failure unread
func (a *app) saveSetting(key, word string) tea.Cmd {
	return fire(a.gw, tuigateway.ConfigSet, tuigateway.ConfigSetParams{Key: key, Value: tuigateway.TextValue(word)})
}

// Keys of the terminal UI context the session window does not answer itself, the reading keys first while the transcript is read
func (a *app) tuiAction(id string) (tea.Cmd, bool) {
	if cmd, taken := a.readAction(id); taken {
		return cmd, true
	}
	switch id {
	case "tui.model":
		return a.openModelPicker(), true
	case "tui.sessions":
		return a.openSessions(""), true
	case "tui.cut":
		return a.cutSelection(), true
	case "tui.queue.delete":
		return a.deleteQueueEdit(), true
	case "tui.queue.send":
		return a.sendQueueHead(), true
	case "tui.queue.drop":
		if a.hasQueued() {
			return a.dropQueued(), true
		}
		return nil, true
	case "tui.yolo":
		return a.toggleYolo(), true
	case "tui.agents":
		return a.openSpawnTree(0, nil), true
	case "tui.agents.dock":
		a.agentDockCollapsed = !a.agentDockCollapsed
		return nil, true
	case "tui.editor":
		return a.openEditor(), true
	case "tui.redraw":
		a.card.view.invalidate()
		return a.store.clearScreen(), true
	case "tui.suspend":
		return a.suspend(), true
	case "tui.paste.image":
		return a.pasteClipboardImage(false), true
	case "tui.voice":
		return a.toggleVoiceRecording(), true
	case "text.paste":
		return a.pasteFromClipboard(), true
	case "complete.start":
		return a.tabComplete(a.card.composer), true
	}
	return a.stashAction(id)
}

// Stops to the shell with the line saying fg brings the UI back
func (a *app) suspend() tea.Cmd {
	return tty.Suspend("\n" + i18n.T(a.ctx, "cli.tui.suspended", "", map[string]any{"agent_name": a.store.ui.Theme.Brand.Name}))
}

// Line of a suspend refused for want of job control, the Windows one or the one naming the shell
func (a *app) suspendRefused() {
	if runtime.GOOS == "windows" {
		a.sys(i18n.T(a.ctx, "cli.tui.suspend_unsupported", "", nil))
		return
	}
	a.sys(i18n.T(a.ctx, "cli.tui.suspend_no_job_control", "", map[string]any{"agent_name": a.store.ui.Theme.Brand.Name}))
}

// Messages of the composer and widget machinery the root update hands on
func (a *app) updateExtras(m tea.Msg) (bool, tea.Cmd) {
	switch m := m.(type) {
	case clipboardImageMsg:
		return true, a.onClipboardImage(m)
	case clipboardTextMsg:
		return true, a.clipboardAnswered(m.id, m.text)
	case clipboardWaitedMsg:
		return true, a.clipboardAnswered(m.id, "")
	case cutDoneMsg:
		if !m.written {
			return true, nil
		}
		cut := a.card.composer.Cut(m.span)
		a.syncTokens()
		return true, cut
	case tea.ClipboardMsg:
		if cmd, taken := a.terminalClipboardAnswer(m); taken {
			return true, cmd
		}
	case terminalHintsMsg:
		a.terminalHints = m.hints
		return true, nil
	case widgetTickedMsg:
		a.widgets.onTicked(m)
		return true, nil
	case widgetsLoadedMsg:
		return true, a.widgets.onScanned(m)
	case dieMsg:
		return true, a.die(m.code)
	case tty.SuspendRefusedMsg:
		a.suspendRefused()
		return true, nil
	case tty.TerminateMsg:
		return true, a.terminated(m.Signal)
	case tty.GoneMsg:
		return true, a.outputGone(m.Err)
	case completionDueMsg:
		return true, a.askCompletion(m)
	}
	return false, nil
}

// Starts or stops voice recording of the session, the recording mark flipped at once and taken back when the gateway did not start, a gateway still
// transcribing the last capture saying so
func (a *app) toggleVoiceRecording() tea.Cmd {
	if !a.voiceEnabled {
		a.sys(tr(txt.SessionInputVoiceModeOff))
		return nil
	}
	starting := !a.voiceRecording
	action := tuigateway.VoiceRecordAction("stop")
	if starting {
		action = "start"
	}
	a.setVoiceRecording(starting)
	if !starting {
		a.setVoiceProcessing(false)
	}
	params := tuigateway.VoiceRecordParams{Action: action, SessionID: a.store.ui.Sid}
	return call(a.gw, tuigateway.VoiceRecord, params, func(r *tuigateway.VoiceRecordResult, err error) tea.Cmd {
		if err != nil {
			a.sys(errorLine(rpcErrorMessage(err)))
		}
		if !starting || (err == nil && r.Status == "recording") {
			return nil
		}
		busy := err == nil && r.Status == "busy"
		a.setVoiceRecording(false)
		a.setVoiceProcessing(busy)
		if busy {
			a.sys(tr(txt.SessionInputVoiceStillTranscribing))
		}
		return nil
	})
}
