package app

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode"

	"github.com/nickheyer/icebath/pkg/agent"
	"github.com/nickheyer/icebath/pkg/commands"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/gatewaycli"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/observability"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/state"
)

// Prints the canonical version block with the update check, assigned by the binary at startup
var printVersionInfoHook func(checkUpdates bool)

// Sets the version printer /version runs
func SetPrintVersionInfo(fn func(checkUpdates bool)) { printVersionInfoHook = fn }

// Banner line printed after /clear in the active language
func (c *CLI) freshStart() string { return "  " + c.t("cli.session.fresh_start", nil) + "\n" }

// First 80 characters of a queued payload with an ellipsis past that
func queuePreview(payload string) string {
	if pystr.Len(payload) > 80 {
		return pystr.Slice(payload, 0, 80) + "..."
	}
	return payload
}

// One based queue index of a token of decimal digits, false for anything Python int rejects
func parseQueueIndex(token string) (int, bool) {
	if token == "" {
		return 0, false
	}
	n := 0
	for _, r := range token {
		if !unicode.IsDigit(r) {
			return 0, false
		}
		d, ok := decimalValue(r)
		if !ok {
			return 0, false
		}
		n = n*10 + d
		if n > 1<<40 {
			return 0, false
		}
	}
	return n, true
}

// Decimal value of a Unicode Nd digit, false for digits int refuses such as superscripts
func decimalValue(r rune) (int, bool) {
	if !unicode.Is(unicode.Nd, r) {
		return 0, false
	}
	start := r
	for unicode.Is(unicode.Nd, start-1) {
		start--
	}
	return int(r-start) % 10, true
}

// Management verbs of /queue with whether each takes a leading index
var queueVerbs = map[string]struct {
	handler    string
	takesIndex bool
}{
	"list": {"list", false}, "ls": {"list", false}, "show": {"list", false}, "clear": {"clear", false},
	"edit": {"edit", true}, "set": {"edit", true},
	"rm": {"remove", true}, "remove": {"remove", true}, "delete": {"remove", true}, "del": {"remove", true}, "pop": {"remove", true},
	"move": {"move", true},
}

// Handles /exit and /quit, false to leave the REPL, --delete marking the session for removal
func (c *CLI) cmdExit(cmdOriginal string) bool {
	args := pystr.Lower(slashArgs(cmdOriginal))
	if args == "--delete" || args == "-d" {
		c.deleteSessionOnExit = true
	} else if args != "" {
		cprint("  " + dimANSI + c.t("cli.exit.unknown_argument", map[string]any{"argument": pyrich.Escape(args)}) + resetANSI)
		return true
	}
	return false
}

// Handles /help with an optional command or category
func (c *CLI) cmdHelp(cmdOriginal string) { c.showHelp(slashArgs(cmdOriginal)) }

// Handles /redraw, the manual recovery for terminal buffer drift
func (c *CLI) cmdRedraw(string) {
	c.forceFullRedraw()
	cprint("  " + dimANSI + c.t("cli.display.ui_redrawn", nil) + resetANSI)
}

// Handles /clear, confirming then starting a fresh session on a cleared screen with the banner
func (c *CLI) cmdClear(cmdOriginal string) {
	if c.confirmDestructiveSlash("clear", "This clears the screen and starts a new session.\n"+
		"The current conversation history will be discarded.", cmdOriginal) == "" {
		return
	}
	c.newSession(true, "")
	clearOutputHistory()
	if c.app == nil {
		c.clearConsole()
		c.showBanner()
		printLines(c.freshStart())
		c.printRandomTip()
		return
	}
	_ = c.app.WriteRaw("\x1b[2J\x1b[0;0H")
	plan := bannerPlan(c)
	if plan.Banner == nil {
		chatConsolePrint(buildCompactBanner())
	} else if plan.Inventory != nil {
		var bannerErr error
		defaultChatConsole.emit(func(r *pyrich.Console) { bannerErr = c.showGatewayBanner(r, plan.Inventory, *plan.Banner) })
		if bannerErr != nil {
			panic(bannerErr)
		}
	} else {
		kw := *plan.Banner
		kw.Tools = toolDefinitionsFor(c, false)
		var bannerErr error
		defaultChatConsole.emit(func(r *pyrich.Console) { bannerErr = welcomeBannerPrinter(c.ctx, r, kw) })
		if bannerErr != nil {
			panic(bannerErr)
		}
	}
	cprint(c.freshStart())
	c.printRandomTip()
}

// Handles /title, showing the current title or setting one now or once the session row exists
func (c *CLI) cmdTitle(cmdOriginal string) {
	parts := pystr.SplitWhitespace(cmdOriginal, 1)
	if len(parts) == 1 {
		if c.sessionDB == nil {
			cprint(c.dbUnavailableLine())
			return
		}
		cprint("  " + c.t("cli.title.session_id", map[string]any{"session_id": c.sessionID}))
		meta := c.sessionMeta(c.sessionID)
		switch {
		case meta != nil && cliTruthy(meta["title"]):
			cprint("  " + c.t("cli.title.current", map[string]any{"title": meta["title"]}))
		case c.pendingTitle != "":
			cprint("  " + c.t("cli.title.pending", map[string]any{"title": c.pendingTitle}))
		default:
			cprint("  " + c.t("cli.title.none_set_usage", nil))
		}
		return
	}
	rawTitle := ""
	if len(parts) > 1 {
		rawTitle = pystr.Strip(parts[1])
	}
	if rawTitle == "" {
		cprint("  " + c.t("cli.title.usage", nil))
		return
	}
	if c.sessionDB == nil {
		cprint(c.dbUnavailableLine())
		return
	}
	newTitle, err := state.SanitizeTitle(rawTitle)
	if err != nil {
		cprint("  " + err.Error())
		return
	}
	if newTitle == "" {
		cprint("  " + c.t("cli.title.empty_after_cleanup", nil))
		return
	}
	if c.sessionMeta(c.sessionID) != nil {
		ok, err := c.sessionDB.SetSessionTitle(c.ctx, c.sessionID, newTitle)
		switch {
		case err != nil && isValueError(err):
			cprint("  " + err.Error())
		case err != nil:
			panic(err)
		case ok:
			c.statusBarTitleCheckedAt = 0
			cprint("  " + c.t("cli.title.set", map[string]any{"title": newTitle}))
		default:
			cprint("  " + c.t("gateway.shared.session_not_found", nil))
		}
		return
	}
	existing, err := c.sessionDB.GetSessionByTitle(c.ctx, newTitle)
	if err != nil {
		panic(err)
	}
	if existing != nil {
		id, _ := existing.Get("id")
		cprint("  " + c.t("cli.title.already_in_use", map[string]any{"title": newTitle, "session_id": id}))
		return
	}
	c.pendingTitle = newTitle
	cprint("  " + c.t("cli.title.queued", map[string]any{"title": newTitle}))
}

// Handles /new with an optional title after the inline skip tokens
func (c *CLI) cmdNew(cmdOriginal string) {
	newArgs, _ := splitDestructiveSkip(cmdOriginal)
	title := pystr.Strip(newArgs)
	if c.confirmDestructiveSlash("new", "This starts a fresh session.\n"+
		"The current conversation history will be discarded.", cmdOriginal) == "" {
		return
	}
	c.newSession(false, title)
}

// Counts one retry or undo against the model of the session's last turn
func (c *CLI) recordModelFriction(signal string, turns int) {
	if c.slashMetricsOff {
		return
	}
	opts := observability.FrictionOptions{SessionID: c.sessionID, Provider: c.provider, Model: c.model, Turns: turns}
	if c.agent != nil {
		opts.Provider, opts.Model = c.agent.Provider(), c.agent.Model()
		if sid := c.agent.SessionID(); sid != "" {
			opts.SessionID = sid
		}
	}
	observability.RecordModelFriction(c.ctx, signal, opts)
}

// Handles /retry, queueing the last user message again after dropping its exchange
func (c *CLI) cmdRetry(string) {
	retryMsg, ok := c.retryLast()
	if ok && retryMsg != "" {
		c.recordModelFriction("retry", 1)
		c.pendingInput.Put(retryMsg)
	}
}

// Handles /undo with an optional turn count, confirming before rewinding
func (c *CLI) cmdUndo(cmdOriginal string) {
	n := 1
	if parts := strings.Fields(cmdOriginal); len(parts) > 1 {
		parsed, ok := parsePyInt(parts[1])
		if !ok {
			printLines(c.t("cli.undo.invalid_count", map[string]any{"value": pyrepr.StrRepr(parts[1])}))
			return
		}
		n = max(1, parsed)
	}
	if len(c.conversationHistory) == 0 {
		printLines(c.t("cli.session.undo_no_messages", nil))
		return
	}
	desc := c.t("cli.undo.removes_last_turns", map[string]any{"count": n})
	if n == 1 {
		desc = c.t("cli.undo.removes_last_exchange", nil)
	}
	if c.confirmDestructiveSlash("undo", desc, cmdOriginal) == "" {
		return
	}
	if turns := c.undoLast(n, true); turns > 0 {
		c.recordModelFriction("undo", turns)
	}
}

// Handles /egress through the shared executor, printed as plain text
func (c *CLI) cmdEgress(string) {
	reply, err := commands.ExecuteCommand(c.ctx, "egress", commands.CommandContext{Surface: "cli"})
	if err != nil {
		panic(err)
	}
	c.consolePrint(pyrich.Escape(reply.Text))
}

// Handles /statusbar, toggling the status bar
func (c *CLI) cmdStatusbar(string) {
	c.statusBarVisible = !c.statusBarVisible
	key := "cli.display.status_bar_hidden"
	if c.statusBarVisible {
		key = "cli.display.status_bar_visible"
	}
	c.consolePrint("  " + c.t(key, nil))
}

// Handles /version with the update check
func (c *CLI) cmdVersion(string) { printVersionInfoHook(true) }

// Handles /reload, rereading the home .env into the process
func (c *CLI) cmdReload(string) {
	count, err := config.ReloadEnv(home.Dir(c.ctx))
	if err != nil {
		panic(err)
	}
	printLines("  " + c.t("cli.reload.env_reloaded", map[string]any{"count": count}))
}

// Handles /reload-skills under the slow command status
func (c *CLI) cmdReloadSkills(cmdOriginal string) {
	_ = c.busyCommand(c.slowCommandStatus(cmdOriginal), true, c.reloadSkills)
}

// Handles /plugins, a glance at user plugins with load details and the bundled count on one line
func (c *CLI) cmdPlugins(string) {
	defer func() {
		if r := recover(); r != nil {
			printLines(c.t("cli.plugins.system_error", map[string]any{"error": errorText(r)}))
		}
	}()
	h := home.Dir(c.ctx)
	listing, err := plugins.LoadQuickPluginListing(h)
	if err != nil {
		panic(err)
	}
	userEntries, bundledCount := listing.UserEntries()
	if len(userEntries) == 0 {
		printLines(c.t("cli.plugins.none_installed", nil), "  "+c.t("cli.plugins.install_hint", nil),
			"  "+c.t("cli.plugins.drop_dir_hint", map[string]any{"path": home.Display(h) + "/plugins/"}))
		if bundledCount > 0 {
			printLines("  " + c.t("cli.plugins.bundled_available", map[string]any{"count": bundledCount}))
		}
		return
	}
	loaded, onGateway := gatewayLoadedPlugins(c)
	if !onGateway {
		loaded = map[string]map[string]any{}
		func() {
			defer func() {
				if r := recover(); r != nil {
					loaded = map[string]map[string]any{}
				}
			}()
			for _, p := range plugins.GetPluginManager(c.ctx).ListPlugins() {
				loaded[cliStr(p["name"], "")] = p
			}
		}()
	}
	printLines(c.t("cli.plugins.user_plugins_header", map[string]any{"count": len(userEntries)}))
	for _, e := range userEntries {
		status := listing.Status(h, e)
		info := loaded[e.Name]
		var bits []string
		for _, k := range []string{"tools", "hooks", "commands"} {
			if info != nil && cliTruthy(info[k]) {
				bits = append(bits, pyrepr.Str(info[k])+" "+c.t("cli.plugins.count_noun_"+k, nil))
			}
		}
		glyph := map[string]string{"enabled": "✓", "disabled": "✗"}[status]
		if glyph == "" {
			glyph = "○"
		}
		ver, detail, label, errText := "", "", "", ""
		if e.Version != "" {
			ver = " v" + e.Version
		}
		if len(bits) > 0 {
			detail = " (" + strings.Join(bits, ", ") + ")"
		}
		if status != "enabled" {
			label = " [" + status + "]"
		}
		if info != nil && cliTruthy(info["error"]) {
			errText = " — " + pyrepr.Str(info["error"])
		}
		printLines("  " + glyph + " " + e.Name + ver + label + detail + errText)
	}
	if bundledCount > 0 {
		printLines("  " + c.t("cli.plugins.bundled_more", map[string]any{"count": bundledCount}))
	}
	printLines("  " + c.t("cli.plugins.enable_disable_hint", nil))
}

// Applies mutate to the queued prompts under the queue lock, returning the items before and after
func (c *CLI) mutatePendingInput(mutate func([]any) []any) ([]any, []any) {
	q := c.pendingInput
	q.mu.Lock()
	before := append([]any(nil), q.items...)
	after := mutate(append([]any(nil), before...))
	q.items = append([]any(nil), after...)
	notify := q.waker()
	q.mu.Unlock()
	if len(after) > 0 {
		select {
		case notify <- struct{}{}:
		default:
		}
	}
	return before, after
}

// Display text of a queued item, the prompt of a voice or seeded message
func queueItemText(item any) string {
	switch v := item.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	}
	return pyrepr.Str(item)
}

// Gateway session store while this CLI is a viewer of the gateway's session, nil otherwise
func (c *CLI) gatewayQueueStore() *gatewaycli.GatewaySessionStore {
	if c.sessionID == "" {
		return nil
	}
	return c.gatewayStore()
}

// One queue entry, the owner's admission id or empty for a turn waiting in the pending input
type queueItem struct {
	admission string
	item      any
}

// The owner's queued admissions in its FIFO order, then the turns still waiting in the pending input
func (c *CLI) queueItems() []queueItem {
	var items []queueItem
	if store := c.gatewayQueueStore(); store != nil {
		for _, row := range queuedRows(c, store) {
			items = append(items, queueItem{row.admission, row.text})
		}
	}
	for _, item := range c.pendingInput.snapshot() {
		items = append(items, queueItem{"", item})
	}
	return items
}

// Admissions of the owner's FIFO among the queue entries
func ownedQueueCount(items []queueItem) int {
	owned := 0
	for _, item := range items {
		if item.admission != "" {
			owned++
		}
	}
	return owned
}

// Enqueues a prompt, in the gateway's durable FIFO while a turn runs, else for the next local turn
func (c *CLI) queueEnqueue(text string) {
	payload, err := c.expandPasteReferences(text)
	if err != nil {
		panic(err)
	}
	var store *gatewaycli.GatewaySessionStore
	if c.agentRunning {
		store = c.gatewayQueueStore()
	}
	if store != nil {
		if _, err := submitQueued(c, store, payload); err != nil {
			var unknown *QueueOutcomeUnknown
			switch {
			case errors.As(err, &unknown):
				cprint("  " + c.t("cli.queue.submit_unknown", map[string]any{"error": err.Error()}))
			case isGatewayClientError(err):
				cprint("  " + c.t("cli.queue.gateway_refused", map[string]any{"error": err.Error()}))
			default:
				panic(err)
			}
			return
		}
	} else {
		c.pendingInput.Put(payload)
	}
	key := "cli.queue.queued"
	if c.agentRunning {
		key = "cli.queue.queued_next_turn"
	}
	cprint("  " + c.t(key, map[string]any{"preview": queuePreview(payload)}))
}

// Lists the owner's queued admissions then the local queue, voice items tagged
func (c *CLI) queueList(rest string) {
	items := c.queueItems()
	if len(items) == 0 {
		suffix := ""
		if rest == "" {
			suffix = "  " + c.t("cli.queue.usage", nil)
		}
		cprint("  " + c.t("cli.queue.empty", nil) + suffix)
		return
	}
	cprint("  " + c.t("cli.queue.pending_header", map[string]any{"count": len(items)}))
	for idx, entry := range items {
		tag := ""
		if _, voice := entry.item.(*voiceInputMessage); voice {
			tag = " [voice]"
		}
		cprint(fmt.Sprintf("    %d. %s%s", idx+1, queuePreview(strings.ReplaceAll(queueItemText(entry.item), "\n", " ")), tag))
	}
}

// Runs one owner verb on queued item index, printing its refusal in its own words
func (c *CLI) queueGatewayCall(verb string, index int, call func(store *gatewaycli.GatewaySessionStore) (string, error)) string {
	result, err := call(c.gatewayQueueStore())
	if err == nil {
		return result
	}
	var refused *gateway.GatewayRPCError
	switch {
	case errors.As(err, &refused):
		if queueStartedRefusals[refused.Error()] {
			cprint("  " + c.t("cli.queue.already_started", map[string]any{"index": index}))
			break
		}
		key := "cli.queue.remove_refused"
		if verb == "replace" {
			key = "cli.queue.edit_refused"
		}
		cprint("  " + c.t(key, map[string]any{"index": index, "error": err.Error()}))
	case isGatewayClientError(err):
		cprint("  " + c.t("cli.queue.change_unknown", map[string]any{"index": index, "error": err.Error()}))
	default:
		panic(err)
	}
	return ""
}

// Cancels the owner's queued admissions and drops every local queued prompt
func (c *CLI) queueClear(string) {
	cleared := 0
	idx := 0
	for _, entry := range c.queueItems() {
		if entry.admission == "" {
			continue
		}
		idx++
		admission := entry.admission
		if c.queueGatewayCall("cancel", idx, func(store *gatewaycli.GatewaySessionStore) (string, error) {
			return cancelQueued(c, store, admission)
		}) == "cancelled" {
			cleared++
		}
	}
	before, _ := c.mutatePendingInput(func([]any) []any { return nil })
	cleared += len(before)
	key := "cli.queue.cleared_other"
	if cleared == 1 {
		key = "cli.queue.cleared_one"
	}
	cprint("  " + c.t(key, map[string]any{"count": cleared}))
}

// Removes the queued prompt at a one based index, the owner's admissions first
func (c *CLI) queueRemove(rest string) {
	bits := strings.Fields(rest)
	idx, ok := 0, false
	if len(bits) == 1 {
		idx, ok = parseQueueIndex(bits[0])
	}
	if !ok {
		cprint("  " + c.t("cli.queue.usage_remove", nil))
		return
	}
	items := c.queueItems()
	owned := ownedQueueCount(items)
	if idx >= 1 && idx <= owned {
		entry := items[idx-1]
		switch c.queueGatewayCall("cancel", idx, func(store *gatewaycli.GatewaySessionStore) (string, error) {
			return cancelQueued(c, store, entry.admission)
		}) {
		case "cancelled":
			cprint("  " + c.t("cli.queue.removed", map[string]any{"index": idx, "preview": queuePreview(queueItemText(entry.item))}))
		case "stopped":
			cprint("  " + c.t("cli.queue.started_stopped", map[string]any{"index": idx}))
		case "started":
			cprint("  " + c.t("cli.queue.already_started", map[string]any{"index": idx}))
		}
		return
	}
	local := idx - owned
	var removed []any
	before, _ := c.mutatePendingInput(func(items []any) []any {
		if local >= 1 && local <= len(items) {
			removed = append(removed, items[local-1])
			return append(items[:local-1], items[local:]...)
		}
		return items
	})
	if len(removed) > 0 {
		cprint("  " + c.t("cli.queue.removed", map[string]any{"index": idx, "preview": queuePreview(queueItemText(removed[0]))}))
		return
	}
	cprint("  " + c.t("cli.queue.item_not_found", map[string]any{"index": idx, "size": owned + len(before)}))
}

// Replaces the queued prompt at a one based index, the owner admitting an edit at the back of its FIFO
func (c *CLI) queueEdit(rest string) {
	bits := pystr.SplitWhitespace(rest, 1)
	idx, ok := 0, false
	if len(bits) == 2 {
		idx, ok = parseQueueIndex(bits[0])
	}
	if !ok {
		cprint("  " + c.t("cli.queue.usage_edit", nil))
		return
	}
	newText, err := c.expandPasteReferences(pystr.Strip(bits[1]))
	if err != nil {
		panic(err)
	}
	items := c.queueItems()
	owned := ownedQueueCount(items)
	if idx >= 1 && idx <= owned {
		admission := items[idx-1].admission
		if c.queueGatewayCall("replace", idx, func(store *gatewaycli.GatewaySessionStore) (string, error) {
			return replaceQueued(c, store, admission, newText)
		}) != "" {
			cprint("  " + c.t("cli.queue.updated", map[string]any{"index": idx, "preview": queuePreview(newText)}))
		}
		return
	}
	local := idx - owned
	changed := false
	before, _ := c.mutatePendingInput(func(items []any) []any {
		if local >= 1 && local <= len(items) {
			old := items[local-1]
			if _, voice := old.(*voiceInputMessage); voice {
				items[local-1] = &voiceInputMessage{Text: newText}
				changed = true
			} else {
				changed = !pyrepr.Equal(old, newText)
				items[local-1] = newText
			}
		}
		return items
	})
	if !changed {
		cprint("  " + c.t("cli.queue.item_not_found", map[string]any{"index": idx, "size": owned + len(before)}))
		return
	}
	cprint("  " + c.t("cli.queue.updated", map[string]any{"index": idx, "preview": queuePreview(newText)}))
}

// Moves a local queued prompt between one based positions, the owner's FIFO having no reorder verb
func (c *CLI) queueMove(rest string) {
	bits := strings.Fields(rest)
	src, dst, ok := 0, 0, false
	if len(bits) == 2 {
		var okA, okB bool
		src, okA = parseQueueIndex(bits[0])
		dst, okB = parseQueueIndex(bits[1])
		ok = okA && okB
	}
	if !ok {
		cprint("  " + c.t("cli.queue.usage_move", nil))
		return
	}
	owned := ownedQueueCount(c.queueItems())
	if owned > 0 && min(src, dst) <= owned && src != dst {
		cprint("  " + c.t("cli.queue.move_gateway_fifo", nil))
		return
	}
	src, dst = src-owned, dst-owned
	moved := false
	before, _ := c.mutatePendingInput(func(items []any) []any {
		if src >= 1 && src <= len(items) && dst >= 1 && dst <= len(items) {
			item := items[src-1]
			items = append(items[:src-1], items[src:]...)
			items = append(items[:dst-1], append([]any{item}, items[dst-1:]...)...)
			moved = src != dst
		}
		return items
	})
	if !moved && src != dst {
		cprint("  " + c.t("cli.queue.move_out_of_range", map[string]any{"size": owned + len(before)}))
		return
	}
	cprint("  " + c.t("cli.queue.moved", map[string]any{"source": src + owned, "destination": dst + owned}))
}

// Handles /queue, managing the queue for a fitting verb and enqueueing anything else
func (c *CLI) cmdQueue(cmdOriginal string) {
	payload := slashArgs(cmdOriginal)
	if payload == "" {
		c.queueList("")
		return
	}
	verb, rest, _ := strings.Cut(payload, " ")
	verb, rest = pystr.Lower(verb), pystr.Strip(rest)
	if verb == "add" {
		if rest != "" {
			c.queueEnqueue(rest)
		} else {
			cprint("  " + c.t("cli.queue.usage_add", nil))
		}
		return
	}
	entry, known := queueVerbs[verb]
	management := false
	if known {
		if entry.takesIndex && rest != "" {
			first := pystr.SplitWhitespace(rest, 1)[0]
			management = first != "" && strings.IndexFunc(first, func(r rune) bool { return !pystr.IsDigit(r) }) < 0
		} else {
			management = rest == ""
		}
	}
	if !management {
		c.queueEnqueue(payload)
		return
	}
	switch entry.handler {
	case "list":
		c.queueList(rest)
	case "clear":
		c.queueClear(rest)
	case "edit":
		c.queueEdit(rest)
	case "remove":
		c.queueRemove(rest)
	case "move":
		c.queueMove(rest)
	}
}

// Handles /steer, injecting text after the next tool call while a turn runs, else queueing it
func (c *CLI) cmdSteer(cmdOriginal string) {
	payload := slashArgs(cmdOriginal)
	switch {
	case payload == "":
		cprint("  " + c.t("cli.steer.usage", nil))
	case c.agentRunning && c.agent != nil:
		accepted, err := func() (ok any, err error) {
			defer func() {
				if r := recover(); r != nil {
					err = asError(r)
				}
			}()
			return c.agent.Steer(payload), nil
		}()
		switch {
		case err != nil:
			cprint("  " + c.t("cli.steer.failed", map[string]any{"error": err.Error()}))
		case gatewayCorrectionVoiceStopped(accepted):
		case accepted == true:
			cprint("  " + c.t("cli.steer.queued", map[string]any{"preview": queuePreview(payload)}))
		default:
			cprint("  " + c.t("cli.steer.rejected_empty", nil))
		}
	default:
		c.pendingInput.Put(payload)
		cprint("  " + c.t("cli.steer.no_agent_queued", map[string]any{"preview": queuePreview(payload)}))
	}
}

// Heartbeat manager bound to the current session id, the gateway's on a gateway store and none before its session opens
func (c *CLI) getHeartbeatManager() agent.HeartbeatAutomation {
	return automationManager(c, &c.heartbeatManager,
		func(sid string) agent.HeartbeatAutomation { return agent.NewHeartbeatManager(c.ctx, sid) },
		func(sid string, connection *gatewaycli.GatewayConnection) (agent.HeartbeatAutomation, error) {
			return NewGatewayHeartbeatManager(c.ctx, sid, connection)
		})
}

// Wait between two heartbeat watchdog polls
var heartbeatWatchdogSleep = time.Sleep

// Starts the idle poll that queues a due heartbeat prompt as a normal turn, once per process run
func (c *CLI) startHeartbeatWatchdog() {
	c.uiMu.Lock()
	if c.heartbeatWatchdogStarted {
		c.uiMu.Unlock()
		return
	}
	c.heartbeatWatchdogStarted = true
	c.uiMu.Unlock()
	go func() {
		defer func() {
			c.uiMu.Lock()
			c.heartbeatWatchdogStarted = false
			c.uiMu.Unlock()
		}()
		for !c.shouldExit {
			heartbeatWatchdogSleep(time.Duration(agent.HeartbeatPollSeconds * float64(time.Second)))
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.Debug(fmt.Sprintf("heartbeat watchdog tick failed: %v", r))
					}
				}()
				mgr := c.getHeartbeatManager()
				if remote, isRemote := mgr.(*GatewayHeartbeatManager); isRemote {
					remote.Refresh()
				}
				if mgr == nil || !mgr.IsActive() {
					return
				}
				if c.agentRunning || c.voiceRecording || c.voiceProcessing || !c.pendingInput.Empty() {
					return
				}
				if prompt, ok := mgr.DuePrompt(nil); ok && prompt != "" {
					c.pendingInput.Put(prompt)
				}
			}()
		}
	}()
}
