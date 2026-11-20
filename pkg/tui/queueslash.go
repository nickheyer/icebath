package tui

import (
	"regexp"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// What a /queue line asks for, a management verb or a prompt to queue
type queuePlan struct {
	kind    string
	index   int
	to      int
	text    string
	display string
}

var (
	queueListVerbs   = map[string]bool{"list": true, "ls": true, "show": true}
	queueRemoveVerbs = map[string]bool{"rm": true, "remove": true, "delete": true, "del": true, "pop": true, "drop": true}
	queueEditVerbs   = map[string]bool{"edit": true, "set": true}
	queueUsageKeys   = map[string]string{"add": "slashCmd.core.queue.usageAdd", "edit": "slashCmd.core.queue.usageEdit",
		"move": "slashCmd.core.queue.usageMove", "remove": "slashCmd.core.queue.usageRemove"}
	queueIndexRe   = regexp.MustCompile(`^\d+$`)
	queueEditHead  = regexp.MustCompile(`^\S+\s+\S+\s*`)
	queueSpaceRuns = regexp.MustCompile(`\s+`)
)

// First word of a payload, the text after it and the word in lower case
func queueHead(payload string) (head, rest, verb string) {
	head = queueSpaceRuns.Split(payload, 2)[0]
	return head, strings.TrimSpace(payload[len(head):]), strings.ToLower(head)
}

// Row number a word names, false for anything that is not a plain count
func queueIndex(token string) (int, bool) {
	if !queueIndexRe.MatchString(token) {
		return 0, false
	}
	n, err := strconv.Atoi(token)
	return n, err == nil
}

// Plan of a /queue line, anything that is not a management verb staying a prompt to queue
func planQueueSlash(displayCommand, expandedCommand string) queuePlan {
	payload := strings.TrimSpace(slashArgument(expandedCommand))
	displayPayload := strings.TrimSpace(slashArgument(displayCommand))
	enqueue := func() queuePlan {
		display := displayPayload
		if display == "" {
			display = payload
		}
		return queuePlan{kind: "enqueue", text: payload, display: display}
	}
	usage := func(key string) queuePlan { return queuePlan{kind: "usage", text: key} }
	if payload == "" {
		return queuePlan{kind: "list"}
	}
	head, rest, verb := queueHead(payload)
	words := strings.Fields(rest)
	switch {
	case verb == "add":
		if rest == "" {
			return usage("add")
		}
		display := strings.TrimSpace(strings.TrimPrefix(displayPayload, head))
		if display == "" {
			display = rest
		}
		return queuePlan{kind: "enqueue", text: rest, display: display}
	case queueListVerbs[verb]:
		if rest != "" {
			return enqueue()
		}
		return queuePlan{kind: "list"}
	case verb == "clear":
		if rest != "" {
			return enqueue()
		}
		return queuePlan{kind: "clear"}
	case queueEditVerbs[verb]:
		if rest == "" {
			return usage("edit")
		}
		index, isIndex := queueIndex(words[0])
		if !isIndex {
			return enqueue()
		}
		text := strings.TrimSpace(rest[len(words[0]):])
		if text == "" {
			return queuePlan{kind: "compose", index: index}
		}
		display := text
		if displayPayload != payload {
			if folded := queueEditHead.ReplaceAllString(displayPayload, ""); folded != "" {
				display = folded
			}
		}
		return queuePlan{kind: "edit", index: index, text: text, display: display}
	case queueRemoveVerbs[verb]:
		if rest == "" {
			return usage("remove")
		}
		index, isIndex := queueIndex(words[0])
		if !isIndex {
			return enqueue()
		}
		if len(words) != 1 {
			return usage("remove")
		}
		return queuePlan{kind: "remove", index: index}
	case verb == "move":
		if rest == "" {
			return usage("move")
		}
		from, isIndex := queueIndex(words[0])
		if !isIndex {
			return enqueue()
		}
		if len(words) != 2 {
			return usage("move")
		}
		to, isIndex := queueIndex(words[1])
		if !isIndex {
			return usage("move")
		}
		return queuePlan{kind: "move", index: from, to: to}
	}
	return enqueue()
}

// One line of a queued prompt for a notice, eighty characters of it
func queueClip(text string) string {
	return previewOfN(strings.ReplaceAll(text, "\n", " "), 80)
}

// Carries out a management plan on the queue of the session in view and says what happened
func (r *slashRun) applyQueuePlan(plan queuePlan) tea.Cmd {
	a := r.a
	rows, err := a.queue.queuedDisplay()
	if err != nil {
		r.sys("error: " + err.Error())
		return nil
	}
	queue, err := a.queue.getQueue(nil)
	if err != nil {
		r.sys("error: " + err.Error())
		return nil
	}
	size, local := len(rows), len(queue.items)
	outOfRange := func(n int) bool { return n < 1 || n > size }
	switch plan.kind {
	case "usage":
		r.sys(tr(queueUsageKeys[plan.text]))
	case "list":
		if size == 0 {
			r.sys(tr("slashCmd.core.queue.empty") + "  " + tr("slashCmd.core.queue.usage"))
			return nil
		}
		lines := []string{tr("slashCmd.core.queue.pendingHeader", strconv.Itoa(size))}
		for i, row := range rows {
			lines = append(lines, "  "+strconv.Itoa(i+1)+". "+queueClip(row))
		}
		r.sys(strings.Join(lines, "\n"))
	case "clear":
		var cmds []tea.Cmd
		for i := size - 1; i >= 0; i-- {
			cmd, err := a.queue.removeQ(i)
			if err != nil {
				r.sys("error: " + err.Error())
				return tea.Batch(cmds...)
			}
			cmds = append(cmds, cmd)
		}
		key := "slashCmd.core.queue.clearedOther"
		if size == 1 {
			key = "slashCmd.core.queue.clearedOne"
		}
		r.sys(tr(key, strconv.Itoa(size)))
		return tea.Batch(cmds...)
	case "remove":
		if outOfRange(plan.index) {
			r.sys(tr("slashCmd.core.queue.notFound", strconv.Itoa(plan.index), strconv.Itoa(size)))
			return nil
		}
		if plan.index <= local && queue.items[plan.index-1].InFlight {
			r.sys(tr("slashCmd.core.queue.alreadyStarted", strconv.Itoa(plan.index)))
			return nil
		}
		preview := queueClip(rows[plan.index-1])
		cmd, err := a.queue.removeQ(plan.index - 1)
		if err != nil {
			r.sys("error: " + err.Error())
			return nil
		}
		r.sys(tr("slashCmd.core.queue.removed", strconv.Itoa(plan.index), preview))
		return cmd
	case "compose":
		if outOfRange(plan.index) {
			r.sys(tr("slashCmd.core.queue.notFound", strconv.Itoa(plan.index), strconv.Itoa(size)))
			return nil
		}
		draft, err := a.queue.queueDraft(plan.index - 1)
		if err != nil {
			r.sys("error: " + err.Error())
			return nil
		}
		index := plan.index - 1
		if err := a.queue.setQueueEdit(&index); err != nil {
			r.sys("error: " + err.Error())
			return nil
		}
		a.queueEdit = &index
		a.cancelPendingRewind(false)
		return a.card.composer.SetValue(draft)
	case "edit":
		if outOfRange(plan.index) {
			r.sys(tr("slashCmd.core.queue.notFound", strconv.Itoa(plan.index), strconv.Itoa(size)))
			return nil
		}
		if plan.index > local {
			r.sys(tr("slashCmd.core.queue.gatewayFifo"))
			return nil
		}
		item := queue.items[plan.index-1]
		if item.InFlight {
			r.sys(tr("slashCmd.core.queue.alreadyStarted", strconv.Itoa(plan.index)))
			return nil
		}
		text, display := item.Text, item.Display
		item.Text, item.Display = plan.text, plan.display
		if err := pendingJournal.save(*item); err != nil {
			item.Text, item.Display = text, display
			r.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
			return nil
		}
		r.sys(tr("slashCmd.core.queue.updated", strconv.Itoa(plan.index), queueClip(plan.text)))
	case "move":
		if outOfRange(plan.index) || outOfRange(plan.to) {
			r.sys(tr("slashCmd.core.queue.moveOutOfRange", strconv.Itoa(size)))
			return nil
		}
		if plan.index > local || plan.to > local {
			r.sys(tr("slashCmd.core.queue.gatewayFifo"))
			return nil
		}
		if queue.items[plan.index-1].InFlight || queue.items[plan.to-1].InFlight {
			r.sys(tr("slashCmd.core.queue.alreadyStarted", strconv.Itoa(plan.index)))
			return nil
		}
		if plan.index != plan.to {
			moved := queue.items[plan.index-1]
			removeAtInPlace(&queue.items, plan.index-1)
			queue.items = append(queue.items[:plan.to-1], append([]*queueItem{moved}, queue.items[plan.to-1:]...)...)
			floor := 0.0
			for _, item := range queue.items {
				stamp := a.queue.nextCreatedAt(floor)
				item.CreatedAt, floor = &stamp, stamp
				if err := pendingJournal.save(*item); err != nil {
					r.sys(tr("canonical.queue.notSavedDraftKept", err.Error()))
					return nil
				}
			}
		}
		r.sys(tr("slashCmd.core.queue.moved", strconv.Itoa(plan.index), strconv.Itoa(plan.to)))
	}
	return nil
}
