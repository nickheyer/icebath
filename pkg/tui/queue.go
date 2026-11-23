package tui

import (
	"math"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Queue entry of a text and the display it shows, the text when display is empty
func newQueueItem(text, display string) queueItem {
	if display == "" {
		display = text
	}
	return queueItem{Display: display, Text: text}
}

// Puts an item at the head of the queue
func prependQueueItem(queue *[]*queueItem, item *queueItem) {
	*queue = append([]*queueItem{item}, *queue...)
}

// Removes the item at index, an edited display replacing its text and dropping its delivery identity when the text changed
func takeQueueItem(queue *[]*queueItem, index int, editedDisplay *string) *queueItem {
	if index < 0 || index >= len(*queue) {
		return nil
	}
	item := (*queue)[index]
	*queue = slices.Delete(*queue, index, index+1)
	if item == nil || editedDisplay == nil {
		return item
	}
	text := *editedDisplay
	if strings.Contains(*editedDisplay, item.Display) {
		text = jsReplaceFirst(*editedDisplay, item.Display, item.Text)
	}
	if text == item.Text {
		next := *item
		next.Display = *editedDisplay
		return &next
	}
	kept := *item
	kept.Failed, kept.InFlight, kept.SentOnce, kept.PreparedText, kept.Settle, kept.SubmissionID = false, false, nil, nil, nil, ""
	kept.Display, kept.Text = *editedDisplay, text
	return &kept
}

// First occurrence of pattern replaced the way String.replace expands the dollar patterns of its replacement
func jsReplaceFirst(s, pattern, replacement string) string {
	at := strings.Index(s, pattern)
	if at < 0 {
		return s
	}
	before, after := s[:at], s[at+len(pattern):]
	var b strings.Builder
	for i := 0; i < len(replacement); i++ {
		c := replacement[i]
		if c != '$' || i+1 >= len(replacement) {
			b.WriteByte(c)
			continue
		}
		switch replacement[i+1] {
		case '$':
			b.WriteByte('$')
		case '&':
			b.WriteString(pattern)
		case '`':
			b.WriteString(before)
		case '\'':
			b.WriteString(after)
		default:
			b.WriteByte(c)
			continue
		}
		i++
	}
	return before + b.String() + after
}

// Removes index i in place, out of range leaving the list as it was
func removeAtInPlace(items *[]*queueItem, i int) {
	if i < 0 || i >= len(*items) {
		return
	}
	*items = slices.Delete(*items, i, i+1)
}

// Queue of one destination with the entry being edited and the journal revision it was loaded at
type pendingQueue struct {
	revision    int
	destination submissionDestination
	edit        *int
	items       []*queueItem
}

// Composer queue per destination, journaled so queued inputs survive a crash
type composerQueue struct {
	store         *tuiStore
	gw            turnGateway
	fail          func(error)
	queues        map[string]*pendingQueue
	unbound       *pendingQueue
	lastCreatedAt float64
}

// Queue of the app, failures of journal writes inside settlement reported through fail
func newComposerQueue(store *tuiStore, gw turnGateway, fail func(error)) *composerQueue {
	return &composerQueue{store: store, gw: gw, fail: fail, queues: map[string]*pendingQueue{}, unbound: &pendingQueue{revision: -1}}
}

// Creation time after the previous entry, strictly increasing
func (q *composerQueue) nextCreatedAt(floor float64) float64 {
	q.lastCreatedAt = math.Max(q.store.nowMillis(), math.Max(q.lastCreatedAt+1, floor+1))
	return q.lastCreatedAt
}

// Index of an item in a list, minus one when absent
func indexOfItem(items []*queueItem, item *queueItem) int {
	return slices.Index(items, item)
}

// Journal entries of a destination with the live items kept in their place
func reloadItems(destination submissionDestination, live []*queueItem) ([]*queueItem, error) {
	loaded, err := pendingJournal.load(destination, false)
	if err != nil {
		return nil, err
	}
	retained := map[string]*queueItem{}
	for _, item := range live {
		retained[item.SubmissionID] = item
	}
	out := make([]*queueItem, len(loaded))
	for i := range loaded {
		if kept, has := retained[loaded[i].SubmissionID]; has {
			out[i] = kept
		} else {
			item := loaded[i]
			out[i] = &item
		}
	}
	return out, nil
}

// Edit index of an item after a reload, nil when it is gone
func editIndex(items []*queueItem, edited *queueItem) *int {
	if edited == nil {
		return nil
	}
	i := indexOfItem(items, edited)
	if i < 0 {
		return nil
	}
	return &i
}

// What a queue holds for editing: one of its own items, or a row of the background service counted from the end of the items
type queueEdit struct {
	item *queueItem
	row  int
	held bool
}

// What the queue holds for editing as it stands, before its items are reloaded
func (queue *pendingQueue) editing() queueEdit {
	switch {
	case queue.edit == nil || *queue.edit < 0:
		return queueEdit{}
	case *queue.edit < len(queue.items):
		return queueEdit{item: queue.items[*queue.edit], held: true}
	}
	return queueEdit{row: *queue.edit - len(queue.items), held: true}
}

// Edit index of what was held for editing over reloaded items: the item where it now stands, a row of the background service still behind
// the items, nil when nothing was held or the item is gone
func (e queueEdit) index(items []*queueItem) *int {
	switch {
	case !e.held:
		return nil
	case e.item != nil:
		return editIndex(items, e.item)
	}
	at := len(items) + e.row
	return &at
}

// Queue of a destination, moving queues of migrated sessions and binding inputs queued before a session existed
func (q *composerQueue) getQueue(destination *submissionDestination) (*pendingQueue, error) {
	for oldKey, source := range q.queues {
		owner := pendingJournal.owner(source.destination)
		newKey := pendingDestinationKey(owner)
		if newKey == oldKey {
			continue
		}
		target := q.queues[newKey]
		if target == nil {
			target = &pendingQueue{destination: owner, revision: -1}
		}
		edited := source.editing()
		for _, item := range source.items {
			o := owner
			item.OwnerDestination = &o
			if !item.InFlight && !item.Failed {
				d := owner
				item.Destination = &d
			}
		}
		items, err := reloadItems(owner, append(slices.Clone(target.items), source.items...))
		if err != nil {
			return nil, err
		}
		target.items = items
		if edited.held {
			target.edit = edited.index(items)
		}
		delete(q.queues, oldKey)
		q.queues[newKey] = target
	}
	var d submissionDestination
	if destination != nil {
		d = *destination
	} else {
		captured, err := q.store.captureDestination()
		if err != nil {
			return nil, err
		}
		d = captured
	}
	d = pendingJournal.owner(d)
	if d.Sid == "" {
		return q.unbound, nil
	}
	key := pendingDestinationKey(d)
	queue := q.queues[key]
	if queue == nil {
		items, err := reloadItems(d, nil)
		if err != nil {
			return nil, err
		}
		queue = &pendingQueue{destination: d, items: items, revision: -1}
		q.queues[key] = queue
	}
	if queue.revision != pendingJournal.revision {
		edited := queue.editing()
		items, err := reloadItems(d, queue.items)
		if err != nil {
			return nil, err
		}
		queue.items, queue.edit = items, edited.index(items)
		queue.revision = pendingJournal.revision
	}
	if len(q.unbound.items) > 0 {
		offset := len(queue.items)
		for _, item := range q.unbound.items {
			bound := d
			item.Destination = &bound
			if err := pendingJournal.save(*item); err != nil {
				return nil, err
			}
			queue.items = append(queue.items, item)
		}
		if q.unbound.edit != nil {
			at := offset + *q.unbound.edit
			queue.edit = &at
		}
		q.unbound = &pendingQueue{revision: -1}
	}
	return queue, nil
}

// Owner's pending rows still waiting on its queue
func (q *composerQueue) serverRows() []pendingRow {
	rows := []pendingRow{}
	for _, row := range pendingRowsOf(q.store.ui.Info) {
		if row.serverQueued() {
			rows = append(rows, row)
		}
	}
	return rows
}

// Lines of the queue panel, local entries then the owner's rows
func (q *composerQueue) queuedDisplay() ([]string, error) {
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, item := range queue.items {
		prefix := ""
		if item.Failed {
			prefix = tr(txt.CanonicalQueueUnconfirmedPrefix)
		}
		out = append(out, prefix+item.Display)
	}
	for _, row := range q.serverRows() {
		switch row.Status {
		case "queued":
			out = append(out, row.User)
		case "unknown":
			out = append(out, tr(txt.CanonicalQueueLostRow, row.User))
		default:
			out = append(out, tr(txt.CanonicalQueueServerRow, row.Status, row.User))
		}
	}
	return out, nil
}

// Owner row at a panel index past the local entries, nil for a local index
func (q *composerQueue) serverRowAt(index int) (*pendingRow, error) {
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	local := len(queue.items)
	rows := q.serverRows()
	if index < local || index-local >= len(rows) {
		return nil, nil
	}
	return &rows[index-local], nil
}

// Text an entry puts back in the composer for editing
func (q *composerQueue) queueDraft(index int) (string, error) {
	row, err := q.serverRowAt(index)
	if err != nil {
		return "", err
	}
	if row != nil {
		return row.User, nil
	}
	queue, err := q.getQueue(nil)
	if err != nil {
		return "", err
	}
	if index >= 0 && index < len(queue.items) {
		return queue.items[index].Display, nil
	}
	return "", nil
}

// Cancels an owner row, an unknown one resolved instead, reporting a failure while its session is current
func (q *composerQueue) cancelServerRow(row *pendingRow, then func(err error) tea.Cmd) tea.Cmd {
	sid := q.store.ui.Sid
	settled := func(_ *tuigateway.AdmissionReceipt, err error) tea.Cmd {
		if err != nil && sid == q.store.ui.Sid {
			q.store.ui.Status = tr(txt.CanonicalQueueDiscardFailed, err.Error())
		}
		return then(err)
	}
	if row.Status != "unknown" {
		return call(q.gw, tuigateway.CanonicalPromptCancel, tuigateway.PromptCancelParams{SessionID: sid, AdmissionID: row.AdmissionID}, settled)
	}
	if row.ExecutionGeneration == nil {
		return resolved(func() tea.Cmd {
			return settled(nil, tuigateway.MissingParam(tuigateway.CanonicalPromptResolveUnknown.Name(), "execution_generation"))
		})
	}
	return call(q.gw, tuigateway.CanonicalPromptResolveUnknown, tuigateway.PromptResolveUnknownParams{SessionID: sid, AdmissionID: row.AdmissionID, ExecutionGeneration: *row.ExecutionGeneration}, settled)
}

// Index of the queued input, local or the owner's, that the composer holds for editing, nil when it holds none
func (q *composerQueue) queueEditIndex() *int {
	queue, err := q.getQueue(nil)
	if err != nil || queue.edit == nil || *queue.edit < 0 || *queue.edit >= len(queue.items)+len(q.serverRows()) {
		return nil
	}
	i := *queue.edit
	return &i
}

// Forgets the input held for editing in every queue, the composer having moved to another session
func (q *composerQueue) dropEdits() {
	for _, queue := range q.queues {
		queue.edit = nil
	}
	q.unbound.edit = nil
}

// Sets the entry being edited
func (q *composerQueue) setQueueEdit(idx *int) error {
	queue, err := q.getQueue(nil)
	if err != nil {
		return err
	}
	queue.edit = idx
	return nil
}

// New journaled entry for a destination's queue
func (q *composerQueue) draft(text, display string, destination *submissionDestination, attachments []imageAttachment) (*pendingQueue, *queueItem, error) {
	var d submissionDestination
	if destination != nil {
		d = *destination
	} else {
		captured, err := q.store.captureDestination()
		if err != nil {
			return nil, nil, err
		}
		d = captured
	}
	owner := pendingJournal.owner(d)
	queue, err := q.getQueue(&owner)
	if err != nil {
		return nil, nil, err
	}
	item := newQueueItem(text, display)
	if len(attachments) > 0 {
		item.Attachments = attachments
	}
	floor := 0.0
	if n := len(queue.items); n > 0 && queue.items[n-1].CreatedAt != nil {
		floor = *queue.items[n-1].CreatedAt
	}
	created := q.nextCreatedAt(floor)
	item.SubmissionID, item.Destination, item.CreatedAt = uuid.NewString(), &owner, &created
	return queue, &item, nil
}

// Queues an input for its destination's next turn
func (q *composerQueue) enqueue(text, display string, destination *submissionDestination, attachments []imageAttachment) (*queueItem, error) {
	queue, item, err := q.draft(text, display, destination, attachments)
	if err != nil {
		return nil, err
	}
	if err := pendingJournal.save(*item); err != nil {
		return nil, err
	}
	queue.items = append(queue.items, item)
	return item, nil
}

// Puts an item back at the head of its queue
func (q *composerQueue) prependQ(item *queueItem, destination *submissionDestination) error {
	queue, err := q.getQueue(destination)
	if err != nil {
		return err
	}
	item.InFlight = false
	if item.SubmissionID == "" {
		item.SubmissionID = uuid.NewString()
	}
	if item.Destination == nil {
		if destination != nil {
			d := *destination
			item.Destination = &d
		} else {
			captured, err := q.store.captureDestination()
			if err != nil {
				return err
			}
			item.Destination = &captured
		}
	}
	if err := pendingJournal.save(*item); err != nil {
		return err
	}
	if indexOfItem(queue.items, item) < 0 {
		prependQueueItem(&queue.items, item)
	}
	return nil
}

// Marks an item in flight and arms the settlement that removes it once accepted or keeps it failed
func (q *composerQueue) claim(queue *pendingQueue, item *queueItem) (*queueItem, error) {
	if item.SubmissionID == "" {
		item.SubmissionID = uuid.NewString()
	}
	if item.Destination == nil {
		captured, err := q.store.captureDestination()
		if err != nil {
			return nil, err
		}
		item.Destination = &captured
	}
	flying := *item
	flying.InFlight, flying.Failed = true, false
	if err := pendingJournal.save(flying); err != nil {
		return nil, err
	}
	item.InFlight, item.Failed = true, false
	confirmed := false
	item.Settle = func(accepted bool) {
		if confirmed {
			return
		}
		if _, err := q.getQueue(nil); err != nil {
			q.fail(err)
			return
		}
		confirmed = accepted
		item.InFlight, item.Failed = false, !accepted
		if !accepted {
			if err := pendingJournal.save(*item); err != nil {
				q.fail(err)
			}
			return
		}
		if err := pendingJournal.remove(*item); err != nil {
			q.store.ui.Status = tr(txt.CanonicalQueueJournalCleanupFailed, err.Error())
		}
		for _, pending := range q.queues {
			removeAtInPlace(&pending.items, indexOfItem(pending.items, item))
		}
		removeAtInPlace(&queue.items, indexOfItem(queue.items, item))
	}
	return item, nil
}

// Settles or drops entries the owner's receipts confirm, run whenever the session info changes
func (q *composerQueue) reconcileReceipts() error {
	queue, err := q.getQueue(nil)
	if err != nil {
		return err
	}
	for _, receipt := range pendingRowsOf(q.store.ui.Info) {
		at := slices.IndexFunc(queue.items, func(item *queueItem) bool {
			d := item.Destination
			if d == nil || d.StoredSid == "" || receipt.submissionID() != item.SubmissionID || receipt.TargetSessionID != d.StoredSid {
				return false
			}
			return receipt.TargetProfileHome == firstText(d.OwnerHome, d.ProfileHome)
		})
		if at < 0 {
			continue
		}
		item := queue.items[at]
		if item.Settle != nil {
			item.Settle(true)
			continue
		}
		if err := pendingJournal.remove(*item); err != nil {
			return err
		}
		removeAtInPlace(&queue.items, indexOfItem(queue.items, item))
	}
	return nil
}

// Stages an input sent at once outside the queue order, claimed for its answer
func (q *composerQueue) stage(text, display string, destination *submissionDestination, attachments []imageAttachment) (*queueItem, error) {
	queue, item, err := q.draft(text, display, destination, attachments)
	if err != nil {
		return nil, err
	}
	queued := false
	item.Queued = &queued
	if _, err := q.claim(queue, item); err != nil {
		return nil, err
	}
	queue.items = append(queue.items, item)
	return item, nil
}

// Claims the head of the queue for delivery, a failed head only on retry, nothing while disconnected
func (q *composerQueue) dequeue(retry bool) (*queueItem, error) {
	if c := q.store.ui.GatewayConnected; c != nil && !*c {
		return nil, nil
	}
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	if len(queue.items) == 0 {
		return nil, nil
	}
	item := queue.items[0]
	if item.InFlight || (item.Failed && !retry) {
		return nil, nil
	}
	claimed, err := q.claim(queue, item)
	if err != nil {
		q.store.ui.Status = tr(txt.CanonicalQueueNotSavedQueueKept, err.Error())
		return nil, nil
	}
	return claimed, nil
}

// Takes an entry for sending now, an owner row cancelled on the owner first and resent from here
func (q *composerQueue) takeQ(i int, editedDisplay *string, then func(*queueItem) tea.Cmd) (tea.Cmd, error) {
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	server, err := q.serverRowAt(i)
	if err != nil {
		return nil, err
	}
	if server != nil {
		if server.Status == "unknown" {
			q.store.ui.Status = tr(txt.CanonicalQueueUnknownExecution)
			return then(nil), nil
		}
		text := server.User
		if editedDisplay != nil {
			text = *editedDisplay
		}
		dest := queue.destination
		_, item, err := q.draft(text, text, &dest, nil)
		if err != nil {
			return nil, err
		}
		flying := *item
		flying.InFlight = true
		if err := pendingJournal.save(flying); err != nil {
			return nil, err
		}
		item.InFlight = true
		queue.items = append(queue.items, item)
		return q.cancelServerRow(server, func(cancelErr error) tea.Cmd {
			if cancelErr != nil {
				item.InFlight, item.Failed = false, true
				if err := pendingJournal.save(*item); err != nil {
					q.fail(err)
				}
				return then(nil)
			}
			claimed, err := q.claim(queue, item)
			if err != nil {
				q.fail(err)
				return nil
			}
			return then(claimed)
		}), nil
	}
	if i >= 0 && i < len(queue.items) && queue.items[i].InFlight {
		return then(nil), nil
	}
	var previous *queueItem
	if i >= 0 && i < len(queue.items) {
		previous = queue.items[i]
	}
	item := takeQueueItem(&queue.items, i, editedDisplay)
	if item == nil {
		return then(nil), nil
	}
	if previous != nil && previous.SubmissionID != item.SubmissionID {
		if err := pendingJournal.remove(*previous); err != nil {
			return nil, err
		}
	}
	queue.items = slices.Insert(queue.items, min(i, len(queue.items)), item)
	claimed, err := q.claim(queue, item)
	if err != nil {
		return nil, err
	}
	return then(claimed), nil
}

// Removes an entry, an owner row cancelled on the owner, an entry in flight left alone
func (q *composerQueue) removeQ(i int) (tea.Cmd, error) {
	server, err := q.serverRowAt(i)
	if err != nil {
		return nil, err
	}
	if server != nil {
		return q.cancelServerRow(server, func(error) tea.Cmd { return nil }), nil
	}
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	if i >= 0 && i < len(queue.items) {
		if queue.items[i].InFlight {
			return nil, nil
		}
		if err := pendingJournal.remove(*queue.items[i]); err != nil {
			return nil, err
		}
	}
	removeAtInPlace(&queue.items, i)
	return nil, nil
}

// Number of inputs in the local queue of the session in view
func (q *composerQueue) size() int {
	queue, err := q.getQueue(nil)
	if err != nil {
		return 0
	}
	return len(queue.items)
}

// Whether the head of the local queue can go out now, it being there, not in flight and the gateway reachable
func (q *composerQueue) headWaits() bool {
	if c := q.store.ui.GatewayConnected; c != nil && !*c {
		return false
	}
	queue, err := q.getQueue(nil)
	return err == nil && len(queue.items) > 0 && !queue.items[0].InFlight
}

// Shell command a person typed between braces in a queued input, with where it stands in the display and in the text sent
type typedInterpolation struct {
	command       string
	display, text [2]int
}

// Shell interpolations of a queued input its person typed, found in the display and placed in the text sent by lining up the typed stretches between
// the paste and image labels, so one that only appears inside an expanded paste is never run
func typedInterpolations(display, text string) []typedInterpolation {
	matches := interpolationRe.FindAllStringSubmatchIndex(display, -1)
	if len(matches) == 0 {
		return nil
	}
	var out []typedInterpolation
	if display == text {
		for _, m := range matches {
			out = append(out, typedInterpolation{command: display[m[2]:m[3]], display: [2]int{m[0], m[1]}, text: [2]int{m[0], m[1]}})
		}
		return out
	}
	labels := pasteTokenRunRe.FindAllStringIndex(display, -1)
	start, from := 0, 0
	for i := 0; i <= len(labels); i++ {
		end := len(display)
		if i < len(labels) {
			end = labels[i][0]
		}
		stretch := display[start:end]
		core := strings.TrimFunc(stretch, isJSSpace)
		lead := strings.Index(stretch, core)
		at := -1
		switch {
		case core == "":
		case i == 0 && strings.HasPrefix(text, core):
			at = 0
		case i == len(labels) && strings.HasSuffix(text, core) && len(text)-len(core) >= from:
			at = len(text) - len(core)
		default:
			if found := strings.Index(text[from:], core); found >= 0 {
				at = from + found
			}
		}
		if at >= 0 {
			for _, m := range matches {
				if m[0] < start+lead || m[1] > start+lead+len(core) {
					continue
				}
				shift := at - start - lead
				out = append(out, typedInterpolation{command: display[m[2]:m[3]], display: [2]int{m[0], m[1]}, text: [2]int{m[0] + shift, m[1] + shift}})
			}
			from = at + len(core)
		}
		if i < len(labels) {
			start = labels[i][1]
		}
	}
	return out
}

// Display and text of an input with the output of each typed shell interpolation put in its place
func spliceInterpolations(display, text string, typed []typedInterpolation, outputs []string) (string, string) {
	for i := len(typed) - 1; i >= 0; i-- {
		t := typed[i]
		display = display[:t.display[0]] + outputs[i] + display[t.display[1]:]
		text = text[:t.text[0]] + outputs[i] + text[t.text[1]:]
	}
	return display, text
}

// Marks the queued input the composer holds for editing; nil marks none and forgets what was typed into the queued inputs
func (a *app) setQueueEdit(index *int) {
	if index == nil {
		a.queueSlots = nil
	}
	if err := a.queue.setQueueEdit(index); err != nil {
		a.sys(errorLine(err.Error()))
	}
}

// Whether the first queued input can be sent now
func (a *app) queueHeadWaits() bool { return a.store.ui.Sid != "" && a.queue.headWaits() }

// Sends the first queued input now, one the owner never confirmed going out again
func (a *app) sendQueueHead() tea.Cmd {
	if a.store.ui.Sid == "" {
		return nil
	}
	item, err := a.queue.dequeue(true)
	if err != nil {
		a.sys(errorLine(err.Error()))
		return nil
	}
	if item == nil {
		return nil
	}
	a.setQueueEdit(nil)
	return a.sendQueued(item)
}

// Steps through the queued inputs, newest first, keeping what was typed into each; false leaves the key to the composer's history
func (a *app) cycleQueue(dir int) (tea.Cmd, bool) {
	rows, err := a.queue.queuedDisplay()
	if err != nil {
		return nil, false
	}
	editing := a.queue.queueEditIndex()
	if editing != nil {
		if a.queueSlots == nil {
			a.queueSlots = map[int]string{}
		}
		a.queueSlots[*editing] = a.card.composer.Value()
	}
	leave := func() tea.Cmd {
		a.setQueueEdit(nil)
		return a.card.composer.SetValue("")
	}
	n := len(rows)
	if n == 0 {
		if editing != nil {
			return leave(), false
		}
		return nil, false
	}
	var index int
	switch {
	case editing == nil && dir < 0:
		index = n - 1
	case editing == nil:
		return nil, false
	case *editing+dir < 0:
		return leave(), false
	case *editing+dir >= n:
		return leave(), true
	default:
		index = *editing + dir
	}
	draft, ok := a.queueSlots[index]
	if !ok {
		if draft, err = a.queue.queueDraft(index); err != nil {
			a.sys(errorLine(err.Error()))
			return nil, true
		}
	}
	a.cancelPendingRewind(true)
	a.setQueueEdit(&index)
	set := a.card.composer.SetValue(draft)
	a.syncTokens()
	return set, true
}

// Deletes the queued input the composer holds for editing and empties the composer
func (a *app) deleteQueueEdit() tea.Cmd {
	editing := a.queue.queueEditIndex()
	if editing == nil {
		return nil
	}
	removed, err := a.queue.removeQ(*editing)
	if err != nil {
		a.sys(errorLine(err.Error()))
	}
	return tea.Batch(removed, a.clearIn())
}
