package app

import (
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/skills"
)

// voiceRecorder is the audio recorder the voice prompt drives
type voiceRecorder interface {
	CurrentRMS() int
	Cancel()
	Shutdown()
}

// The live audio recorder, nil while there is none
func (c *CLI) voiceRecorderHandle() voiceRecorder {
	rec, _ := c.voiceRecorder.(voiceRecorder)
	return rec
}

// Python repr of the first limit characters of a queued payload's text form
func pyReprPrefix(payload any, limit int) string {
	text := ""
	switch p := payload.(type) {
	case string:
		text = p
	case submittedInput:
		paths := make([]string, len(p.Images))
		for i, img := range p.Images {
			paths[i] = "PosixPath(" + pyrepr.Repr(img) + ")"
		}
		text = "(" + pyrepr.Repr(p.Text) + ", [" + strings.Join(paths, ", ") + "])"
	}
	r := []rune(text)
	if len(r) > limit {
		r = r[:limit]
	}
	return pyrepr.Repr(string(r))
}

// Resets the per run prompt state, queues, modal states, voice state and the config watcher baseline
func (c *CLI) tuiInitRunState() {
	c.uiMu.Lock()
	c.agentRunning = false
	c.pendingInput = &inputQueue{}
	c.interruptQueue = &inputQueue{}
	if seed := c.seededFirstMessage; seed != nil {
		c.seededFirstMessage = nil
		c.pendingInput.Put(seed)
	}
	c.lastTurnInterrupted = false
	c.shouldExit = false
	c.lastCtrlCTime = 0
	c.clarifyState, c.clarifyFreetext = nil, false
	zero := 0.0
	c.clarifyDeadline = &zero
	c.sudoState, c.sudoDeadline = nil, 0
	c.modalInputSnapshot = nil
	c.approvalState, c.approvalDeadline = nil, 0
	c.slashConfirmState, c.slashConfirmDeadline = nil, 0
	c.commandRunning, c.commandBlocksInput, c.commandStatus = false, false, ""
	c.secretState, c.secretDeadline = nil, 0
	c.connectionState = nil
	c.attachedImages, c.imageCounter = nil, 0
	c.uiMu.Unlock()

	sig, _ := config.ConfigVersionOrNone(c.ctx, home.ConfigPath(home.Dir(c.ctx)))
	c.tui.configSig = sig
	c.tui.configMCPServers = c.config["mcp_servers"]
	if !pyrepr.Truthy(c.tui.configMCPServers) {
		c.tui.configMCPServers = map[string]any{}
	}
	c.tui.lastConfigCheck = 0

	c.voiceLock.Lock()
	c.voiceMode, c.voiceTTS = false, false
	c.voiceRecorder = nil
	c.voiceRecording, c.voiceProcessing, c.voiceLiveText = false, false, ""
	c.voiceContinuous = false
	c.voiceTTSDone = newSyncEvent()
	c.voiceTTSDone.Set()
	c.voiceTTSStop = nil
	c.voiceBargeCapture = newSyncEvent()
	c.voiceLastTTSText, c.voiceBargePhase = "", ""
	c.voiceLock.Unlock()

	if os.Getenv("ICEBATH_DEFER_AGENT_STARTUP") != "1" {
		c.installToolCallbacks()
	}
}

// Reports whether no modal prompt, picker or palette owns the input
func (c *CLI) tuiNormalInput() bool {
	c.uiMu.Lock()
	defer c.uiMu.Unlock()
	return c.clarifyState == nil && c.approvalState == nil && c.slashConfirmState == nil && c.sudoState == nil &&
		c.secretState == nil && c.connectionState == nil && c.modelPickerState == nil && c.commandPaletteState == nil
}

// Builds the key bindings of the prompt, later registrations winning for the same key
func (c *CLI) tuiBuildKeyBindings() *ptKeyBindings {
	kb := &ptKeyBindings{}
	cfg := c.config
	if len(cfg) == 0 {
		cfg = CLIConfig(c.ctx)
	}
	multiline := cliMultilineShortcutsEnabled(cfg)
	c.tui.multilineShortcuts = multiline

	kb.add(c.tuiHandleIgnoredTerminalSequence, ptBind{eager: true}, ptKeyIgnore)
	bindPromptSubmitKeys(kb, c.tuiHandleEnter, &multiline)
	kb.add(c.tuiInsertNewline, ptBind{}, "escape", "enter")
	if multiline || preserveCtrlEnterNewline() {
		kb.add(c.tuiInsertNewline, ptBind{}, "c-j")
	}
	c.tuiBindEditorAndStash(kb)
	c.tuiBindOverlayNavigation(kb)

	kb.add(c.tuiHistoryUp, ptBind{filter: c.tuiNormalInput}, "up")
	kb.add(c.tuiHistoryDown, ptBind{filter: c.tuiNormalInput}, "down")
	kb.add(c.tuiHandleCtrlL, ptBind{}, "c-l")
	kb.add(c.tuiHandleCtrlC, ptBind{}, "c-c")
	kb.add(c.tuiHandleCtrlQ, ptBind{}, "c-q")
	kb.add(c.tuiHandleCtrlD, ptBind{}, "c-d")
	modalPrompt := func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.secretState != nil || c.sudoState != nil || c.slashConfirmState != nil || c.connectionState != nil
	}
	kb.add(c.tuiHandleEscapeModal, ptBind{filter: modalPrompt, eager: true}, "escape")
	kb.add(c.tuiHandleDoubleEscape, ptBind{filter: func() bool { return !modalPrompt() }}, "escape", "escape")
	kb.add(c.tuiHandleCtrlZ, ptBind{}, "c-z")

	kb.add(c.tuiHandleVoiceRecord, ptBind{}, c.tuiVoiceRecordKeySequence()...)
	kb.add(c.tuiHandlePaste, ptBind{eager: true}, ptKeyBracketedPaste)
	kb.add(c.tuiHandleCtrlV, ptBind{}, "c-v")
	kb.add(c.tuiHandleAltV, ptBind{}, "escape", "v")
	noModal := func() bool { return !modalPromptActive(c) }
	for _, key := range []string{"c-t", "f6"} {
		kb.add(func(*ptEvent) { openMonitor(c) }, ptBind{filter: noModal}, key)
	}
	kb.add(func(*ptEvent) { toggleDock(c) }, ptBind{filter: noModal}, "c-r")
	kb.add(func(*ptEvent) { toggleDock(c) }, ptBind{filter: noModal}, "f7")
	return kb
}

// Binds the external editor keys, the stash key and the stash panel keys
func (c *CLI) tuiBindEditorAndStash(kb *ptKeyBindings) {
	editor := func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.clarifyState == nil && c.approvalState == nil && c.sudoState == nil && c.secretState == nil && c.connectionState == nil
	}
	kb.add(c.tuiHandleOpenInEditor, ptBind{filter: editor}, "escape", "g")
	kb.add(c.tuiHandleOpenInEditor, ptBind{filter: editor}, "c-g")
	stash := func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.clarifyState == nil && c.approvalState == nil && c.sudoState == nil && c.secretState == nil &&
			c.connectionState == nil && c.slashConfirmState == nil && c.modelPickerState == nil
	}
	panel := func() bool { return c.promptStash != nil && c.promptStash.PanelOpen && c.promptStash.Len() > 0 }
	kb.add(c.tuiHandlePromptStash, ptBind{filter: stash}, "c-s")
	kb.add(c.tuiHandleStashPanelUp, ptBind{filter: panel, eager: true}, "up")
	kb.add(c.tuiHandleStashPanelDown, ptBind{filter: panel, eager: true}, "down")
	kb.add(c.tuiHandleStashPanelRestore, ptBind{filter: panel, eager: true}, "enter")
	kb.add(c.tuiHandleStashPanelDelete, ptBind{filter: panel, eager: true}, "D")
	kb.add(c.tuiHandleStashPanelDelete, ptBind{filter: panel, eager: true}, "d")
	kb.add(c.tuiHandleStashPanelClose, ptBind{filter: panel, eager: true}, "escape")
	kb.add(c.tuiHandleTab, ptBind{eager: true}, "tab")
}

// Binds the navigation keys of the clarify, approval, slash confirm, model picker and palette overlays
func (c *CLI) tuiBindOverlayNavigation(kb *ptKeyBindings) {
	clarifyNav := func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.clarifyState != nil && !c.clarifyFreetext
	}
	kb.add(c.tuiClarifyUp, ptBind{filter: clarifyNav}, "up")
	kb.add(c.tuiClarifyDown, ptBind{filter: clarifyNav}, "down")
	connectionNav := func() bool { return c.connectionModal() != nil }
	kb.add(c.tuiConnectionUp, ptBind{filter: connectionNav}, "up")
	kb.add(c.tuiConnectionDown, ptBind{filter: connectionNav}, "down")
	kb.add(c.tuiConnectionSide, ptBind{filter: connectionNav}, "left")
	kb.add(c.tuiConnectionSide, ptBind{filter: connectionNav}, "right")
	kb.add(c.tuiClarifyToggle, ptBind{filter: func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.clarifyState != nil && !c.clarifyFreetext && c.clarifyState.MultiSelect
	}}, "space")
	kb.add(c.tuiClarifyBatchTab, ptBind{filter: clarifyNav, eager: true}, "tab")
	kb.add(c.tuiClarifyBatchBacktab, ptBind{filter: clarifyNav, eager: true}, "s-tab")
	numberIndex := func(num int) int {
		if num == 0 {
			return 9
		}
		return num - 1
	}
	for num := 0; num < 10; num++ {
		kb.add(c.tuiMakeClarifyNumberHandler(numberIndex(num)), ptBind{filter: clarifyNav}, string(rune('0'+num)))
	}

	approval := func() bool { return c.approvalModal() != nil }
	kb.add(c.tuiApprovalUp, ptBind{filter: approval}, "up")
	kb.add(c.tuiApprovalDown, ptBind{filter: approval}, "down")
	kb.add(c.tuiSlashConfirmUp, ptBind{filter: c.slashConfirmOpen}, "up")
	kb.add(c.tuiSlashConfirmDown, ptBind{filter: c.slashConfirmOpen}, "down")
	kb.add(c.tuiModelPickerUp, ptBind{filter: c.modelPickerOpen}, "up")
	kb.add(c.tuiModelPickerDown, ptBind{filter: c.modelPickerOpen}, "down")
	pickerTyping := func() bool {
		state := c.modelPicker()
		return state != nil && cliStr(state["stage"], "") == "model"
	}
	for _, ch := range tuiTypingChars {
		kb.add(c.tuiMakeModelFilterCharHandler(string(ch)), ptBind{filter: pickerTyping}, string(ch))
	}
	kb.add(c.tuiModelPickerFilterBackspace, ptBind{filter: pickerTyping}, "backspace")
	kb.add(c.tuiModelPickerEscape, ptBind{filter: c.modelPickerOpen, eager: true}, "escape")

	kb.add(c.tuiOpenCommandPalette, ptBind{filter: c.tuiNormalInput}, "c-p")
	kb.add(c.tuiCommandPaletteUp, ptBind{filter: c.commandPaletteOpen}, "up")
	kb.add(c.tuiCommandPaletteDown, ptBind{filter: c.commandPaletteOpen}, "down")
	kb.add(c.tuiCommandPaletteEnter, ptBind{filter: c.commandPaletteOpen}, "enter")
	kb.add(c.tuiCommandPaletteBackspace, ptBind{filter: c.commandPaletteOpen}, "backspace")
	kb.add(c.tuiCommandPaletteEscape, ptBind{filter: c.commandPaletteOpen, eager: true}, "escape")
	for _, ch := range tuiTypingChars {
		kb.add(c.tuiMakePaletteCharHandler(string(ch)), ptBind{filter: c.commandPaletteOpen}, string(ch))
	}
	for num := 0; num < 10; num++ {
		kb.add(c.tuiMakeApprovalNumberHandler(numberIndex(num)), ptBind{filter: approval}, string(rune('0'+num)))
	}
	for num := 0; num < 10; num++ {
		kb.add(c.tuiMakeSlashConfirmNumberHandler(numberIndex(num)), ptBind{filter: c.slashConfirmOpen}, string(rune('0'+num)))
	}
}

// Resolves the push to talk key to a key sequence and caches its label, unsupported modifiers becoming Ctrl B
func (c *CLI) tuiVoiceRecordKeySequence() []string {
	var raw any = "ctrl+b"
	voiceKey := "c-b"
	if cfg, err := config.Load(home.Dir(c.ctx), nil); err == nil {
		raw = voiceRecordKeyFromConfig(map[string]any(cfg))
		voiceKey = normalizeVoiceRecordKeyForPromptToolkit(raw)
		if s, ok := raw.(string); ok && voiceKey == "c-b" {
			switch strings.TrimSpace(strings.SplitN(strings.ToLower(strings.TrimSpace(s)), "+", 2)[0]) {
			case "super", "win", "windows":
				cliLogger.Warn("voice.record_key " + pyrepr.Repr(s) + " uses a TUI-only modifier (super/win); CLI fell back to Ctrl+B. Use ctrl+<key> or alt+<key> for cross-runtime parity.")
			}
		}
	}
	c.setVoiceRecordKeyCache(raw)
	return ptKeyToSequence(voiceKey)
}

// A wrapped, content sized panel shown while its state is open
func tuiOverlayWidget(name string, fragments func() []styledFragment, open func() bool) *ptWindow {
	return &ptWindow{name: name, fragments: fragments, wrapLines: true, filter: open}
}

// Row count that is always one
func oneRow() int { return 1 }

// Builds the prompt widgets, their order and the style, wrapper key bindings registered on kb
func (c *CLI) tuiBuildLayout(kb *ptKeyBindings, app *ptApplication) ([]*ptWindow, *styleDict) {
	installDock(c)
	c.tuiBuildInputArea(app)
	inputArea := &ptWindow{name: "input", kind: ptWindowBuffer}
	spinner := &ptWindow{name: "spinner", fragments: c.tuiSpinnerText, height: c.tuiSpinnerHeight, wrapLines: true}
	c.tui.petWidget = &ptWindow{name: "pet", fragments: c.petFragments, height: c.petWidgetHeight, alignRight: true}
	spacer := &ptWindow{name: "hint", fragments: c.tuiHintText, height: c.tuiHintHeight}
	clarify := tuiOverlayWidget("clarify", c.getClarifyDisplayFragments, func() bool { return c.clarifyModal() != nil })
	sudo := tuiOverlayWidget("sudo", c.getSudoDisplayFragments, func() bool { return c.sudoModal() != nil })
	secret := tuiOverlayWidget("secret", c.getSecretDisplayFragments, func() bool { return c.secretModal() != nil })
	connection := tuiOverlayWidget("connection", c.getConnectionDisplayFragments, func() bool { return c.connectionModal() != nil })
	approval := tuiOverlayWidget("approval", c.getApprovalDisplayFragments, func() bool { return c.approvalModal() != nil })
	slashConfirm := tuiOverlayWidget("slash-confirm", c.getSlashConfirmDisplayFragments, c.slashConfirmOpen)
	modelPicker := tuiOverlayWidget("model-picker", c.getModelPickerDisplayFragments, c.modelPickerOpen)
	palette := tuiOverlayWidget("command-palette", c.getCommandPaletteDisplayFragments, c.commandPaletteOpen)
	ruleTop := &ptWindow{name: "rule-top", char: "─", style: "class:input-rule", height: func() int { return c.tuiInputRuleHeight("top", nil) }}
	ruleBot := &ptWindow{name: "rule-bottom", char: "─", style: "class:input-rule", height: func() int { return c.tuiInputRuleHeight("bottom", nil) }}
	if c.modernLayout {
		ruleTop, ruleBot = modernInputRules(c, kb)
	}
	imageBar := &ptWindow{name: "image-bar", fragments: c.tuiImageBarFragments, height: func() int {
		if len(c.attachedImages) > 0 {
			return 1
		}
		return 0
	}}
	voiceBar := &ptWindow{name: "voice-status", fragments: c.tuiVoiceStatusFragments, height: oneRow, filter: func() bool {
		c.voiceLock.Lock()
		defer c.voiceLock.Unlock()
		return c.voiceMode
	}}
	statusBar := &ptWindow{name: "status-bar", fragments: c.getStatusBarFragments, height: oneRow, filter: func() bool {
		return c.statusBarVisible && !c.statusBarSuppressedAfterResize.Load()
	}}
	c.tui.stashPanelWidget = &ptWindow{name: "stash-panel", fragments: c.getStashPanelDisplayFragments, filter: func() bool {
		return c.promptStash != nil && c.promptStash.PanelOpen && c.promptStash.Len() > 0
	}}
	c.registerExtraTUIKeybindings(kb, app)
	children := c.buildTUILayoutChildren(tuiLayoutChildren{sudo: sudo, secret: secret, connection: connection, approval: approval,
		slashConfirm: slashConfirm, clarify: clarify, modelPicker: modelPicker, commandPalette: palette, spinner: spinner, spacer: spacer,
		statusBar: statusBar, inputRuleTop: ruleTop, imageBar: imageBar, inputArea: inputArea, inputRuleBot: ruleBot, voiceStatusBar: voiceBar,
		completionsMenu: &ptWindow{name: "completions", kind: ptWindowMenu, maxHeight: 12}})
	c.tuiSetBaseStyle()
	return children, c.buildTUIStyleDict()
}

// The slash completer of the classic prompt over this session's skills, bundles and plugin commands
func cliSlashCompleter(c *CLI) *commands.SlashCompleter {
	return commands.NewCompleter(commands.CompleterSources{
		Ctx:                c.ctx,
		Cfg:                func() config.Tree { return config.Tree(CLIConfig(c.ctx)) },
		Skills:             func() map[string]commands.SkillInfo { return completerSkills(sessionSkillCommands(c)) },
		Bundles:            func() map[string]commands.BundleInfo { return completerBundles(getSkillBundles(c.ctx)) },
		PluginCommands:     func() map[string]string { return pluginCommandNames(c) },
		Skins:              func() []commands.NameMeta { return commands.LiveSkinRows(c.ctx) },
		ToolsetRows:        func() []commands.ToolsetRow { return commands.LiveToolsetRows(c.ctx) },
		ConnectedPlatforms: func() []commands.PlatformHome { return commands.LiveConnectedPlatforms(c.ctx) },
		CommandAllowed:     c.commandAvailable,
	})
}

// Skill command rows in the shape the completer reads
func completerSkills(found skills.SkillCommands) map[string]commands.SkillInfo {
	out := make(map[string]commands.SkillInfo, len(found))
	for key, info := range found {
		out[key] = commands.SkillInfo{Name: cliStr(info["name"], ""), Description: cliStr(info["description"], ""), SkillDir: cliStr(info["skill_dir"], ""),
			SkillMDPath: cliStr(info["skill_md_path"], ""), SkillIdentifier: cliStr(info["skill_identifier"], "")}
	}
	return out
}

// Skill bundle rows in the shape the completer reads
func completerBundles(found skills.SkillCommands) map[string]commands.BundleInfo {
	out := make(map[string]commands.BundleInfo, len(found))
	for key, info := range found {
		out[key] = commands.BundleInfo{Name: cliStr(info["name"], ""), Slug: cliStr(info["slug"], ""), Description: cliStr(info["description"], ""),
			Skills: cliStrList(info["skills"])}
	}
	return out
}

// Configures the multi line input buffer with history, threaded slash completion, ghost text, masking and the empty hint
func (c *CLI) tuiBuildInputArea(app *ptApplication) {
	completer := cliSlashCompleter(c)
	buf := newPTBuffer(newPTFileHistory(c.historyFile))
	buf.readOnly = func() bool {
		c.uiMu.Lock()
		defer c.uiMu.Unlock()
		return c.commandBlocksInput
	}
	buf.completer = func(textBeforeCursor string) []ptCompletion {
		found := completer.Complete(textBeforeCursor)
		out := make([]ptCompletion, len(found))
		for i, f := range found {
			out[i] = ptCompletion{Text: f.Text, Start: f.Start, Display: f.Display, Meta: f.Meta}
		}
		return out
	}
	buf.completeWhileTyping = true
	buf.suggester = slashCommandAutoSuggest(completer)
	buf.tempfileSuffix = ".md"
	app.setBuffer(buf)
	app.inputStyle = "class:input-area"
	app.prompt = c.getTUIPromptFragments
	app.inputHeight = func() int {
		columns, _, ok := app.Size()
		if !ok {
			columns, _ = pyshutil.GetTerminalSize(80, 24)
		}
		return estimateTUIInputHeight(buf.document().lines(), c.getTUIPromptText(), columns, 8)
	}
	c.tui.pasteCounter, c.tui.prevTextLen, c.tui.prevNewlineCount = 0, 0, 0
	c.tui.pasteJustCollapsed, c.tui.skipPasteCollapse = false, false
	buf.onTextChanged = append(buf.onTextChanged, c.tuiOnTextChanged)
	app.password = func() bool {
		c.uiMu.Lock()
		sudo, secret := c.sudoState, c.secretState != nil
		c.uiMu.Unlock()
		if sudo != nil && !(sudo.VaultSave != nil && sudo.VaultSave.Step == "identifier") && sudo.VaultCode == nil {
			return true
		}
		return secret || (c.connectionModal() != nil && c.connectionActiveFieldIsSecret())
	}
	app.emptyHint = c.tuiPlaceholderText
}

// Ghost text for slash commands and subcommands, the newest history line starting with the text otherwise
func slashCommandAutoSuggest(completer *commands.SlashCompleter) func(b *ptBuffer, doc ptDocument) (string, bool) {
	suggest := commands.NewSlashCommandAutoSuggest(autoSuggestFromHistory, completer)
	return func(b *ptBuffer, doc ptDocument) (string, bool) {
		return suggest.GetSuggestion(b, doc, doc.textBeforeCursor())
	}
}

// Rest of the newest history line that starts with the current line, none for a blank line
func autoSuggestFromHistory(b *ptBuffer, doc ptDocument) (string, bool) {
	lines := strings.Split(doc.text(), "\n")
	last := lines[len(lines)-1]
	if strings.TrimSpace(last) == "" {
		return "", false
	}
	entries := b.history.strings()
	for i := len(entries) - 1; i >= 0; i-- {
		parts := strings.Split(entries[i], "\n")
		for j := len(parts) - 1; j >= 0; j-- {
			if strings.HasPrefix(parts[j], last) {
				return parts[j][len(last):], true
			}
		}
	}
	return "", false
}

// Fills the skin aware default styles the style dict is built from
func (c *CLI) tuiSetBaseStyle() {
	d := &styleDict{}
	for _, pair := range [][2]string{
		{"input-area", ""}, {"placeholder", "#888888 italic"}, {"prompt", ""}, {"prompt-working", "#888888 italic"}, {"hint", "#888888 italic"},
		{"status-bar", "bg:#1a1a2e #C0C0C0"}, {"status-bar-strong", "bg:#1a1a2e #FFD700 bold"}, {"status-bar-dim", "bg:#1a1a2e #8B8682"},
		{"status-bar-good", "bg:#1a1a2e #8FBC8F bold"}, {"status-bar-warn", "bg:#1a1a2e #FFD700 bold"}, {"status-bar-bad", "bg:#1a1a2e #FF8C00 bold"},
		{"status-bar-critical", "bg:#1a1a2e #FF6B6B bold"}, {"status-bar-yolo", "bg:#1a1a2e #FF4444 bold"},
		{"status-bar-session-title", "bg:#FFD700 #1a1a2e bold"}, {"input-rule", "#CD7F32"}, {"image-badge", "#87CEEB bold"},
		{"completion-menu", "bg:#1a1a2e #FFF8DC"}, {"completion-menu.completion", "bg:#1a1a2e #FFF8DC"},
		{"completion-menu.completion.current", "bg:#333355 #FFD700"}, {"completion-menu.meta.completion", "bg:#1a1a2e #888888"},
		{"completion-menu.meta.completion.current", "bg:#333355 #FFBF00"}, {"clarify-border", "#CD7F32"}, {"clarify-title", "#FFD700 bold"},
		{"clarify-question", "#FFF8DC bold"}, {"clarify-choice", "#AAAAAA"}, {"clarify-selected", "#FFD700 bold"},
		{"clarify-active-other", "#FFD700 italic"}, {"clarify-answer", "#98FB98"}, {"clarify-countdown", "#CD7F32"},
		{"sudo-prompt", "#FF6B6B bold"}, {"sudo-border", "#CD7F32"}, {"sudo-title", "#FF6B6B bold"}, {"sudo-text", "#FFF8DC"},
		{"approval-border", "#CD7F32"}, {"approval-title", "#FF8C00 bold"}, {"approval-desc", "#FFF8DC bold"},
		{"approval-cmd", "#AAAAAA italic"}, {"approval-choice", "#AAAAAA"}, {"approval-selected", "#FFD700 bold"},
		{"voice-prompt", "#87CEEB"}, {"voice-recording", "#FF4444 bold"}, {"voice-processing", "#FFA500 italic"},
		{"voice-status", "bg:#1a1a2e #87CEEB"}, {"voice-status-recording", "bg:#1a1a2e #FF4444 bold"}} {
		d.set(pair[0], pair[1])
	}
	c.tui.styleBase = d
}
