package tui

import (
	"math"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/google/uuid"
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
	kept.Failed, kept.InFlight, kept.LegacyAttempted, kept.PreparedText, kept.Settle, kept.SubmissionID = false, false, nil, nil, nil, ""
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
	return &i
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
		var edited *queueItem
		if source.edit != nil && *source.edit >= 0 && *source.edit < len(source.items) {
			edited = source.items[*source.edit]
		}
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
		if edited != nil {
			target.edit = editIndex(items, edited)
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
		var edited *queueItem
		if queue.edit != nil && *queue.edit >= 0 && *queue.edit < len(queue.items) {
			edited = queue.items[*queue.edit]
		}
		items, err := reloadItems(d, queue.items)
		if err != nil {
			return nil, err
		}
		queue.items, queue.edit = items, editIndex(items, edited)
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
func (q *composerQueue) serverRows() []map[string]any {
	rows := []map[string]any{}
	for _, row := range pendingRows(q.store.ui.Info["pending_submissions"]) {
		if isServerQueued(row) {
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
			prefix = tr("canonical.queue.unconfirmedPrefix")
		}
		out = append(out, prefix+item.Display)
	}
	for _, row := range q.serverRows() {
		switch row["status"] {
		case "queued":
			out = append(out, jsTemplate(row, "user"))
		case "unknown":
			out = append(out, tr("canonical.queue.lostRow", jsTemplate(row, "user")))
		default:
			out = append(out, tr("canonical.queue.serverRow", jsTemplate(row, "status"), jsTemplate(row, "user")))
		}
	}
	return out, nil
}

// Owner row at a panel index past the local entries, nil for a local index
func (q *composerQueue) serverRowAt(index int) (map[string]any, error) {
	queue, err := q.getQueue(nil)
	if err != nil {
		return nil, err
	}
	local := len(queue.items)
	rows := q.serverRows()
	if index < local || index-local >= len(rows) {
		return nil, nil
	}
	return rows[index-local], nil
}

// Text an entry puts back in the composer for editing
func (q *composerQueue) queueDraft(index int) (string, error) {
	row, err := q.serverRowAt(index)
	if err != nil {
		return "", err
	}
	if row != nil && row["user"] != nil {
		return jsValueString(row["user"]), nil
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
func (q *composerQueue) cancelServerRow(row map[string]any, then func(err error) tea.Cmd) tea.Cmd {
	var sid any
	if q.store.ui.Sid != "" {
		sid = q.store.ui.Sid
	}
	unknown := row["status"] == "unknown"
	method, params := "prompt.cancel", map[string]any{"session_id": sid, "admission_id": row["admission_id"]}
	if unknown {
		method, params["execution_generation"] = "prompt.resolve_unknown", row["execution_generation"]
	}
	return gatewayCall(q.gw, method, params, func(_ any, err error) tea.Cmd {
		if err != nil && jsStrictEqual(sid, nilIfEmpty(q.store.ui.Sid)) {
			q.store.ui.Status = tr("canonical.queue.discardFailed", err.Error())
		}
		return then(err)
	})
}

// Session id as JavaScript holds it, null when empty
func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
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
			q.store.ui.Status = tr("canonical.queue.journalCleanupFailed", err.Error())
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
	for _, receipt := range pendingRows(q.store.ui.Info["pending_submissions"]) {
		id := receipt["input_id"]
		if id == nil {
			id = receipt["admission_id"]
		}
		at := slices.IndexFunc(queue.items, func(item *queueItem) bool {
			d := item.Destination
			if d == nil || d.StoredSid == "" || !jsStrictEqual(id, item.SubmissionID) || receipt["target_session_id"] != d.StoredSid {
				return false
			}
			ownerHome := d.OwnerHome
			if ownerHome == "" {
				ownerHome = d.ProfileHome
			}
			return receipt["target_profile_home"] == ownerHome
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
		q.store.ui.Status = tr("canonical.queue.notSavedQueueKept", err.Error())
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
		if server["status"] == "unknown" {
			q.store.ui.Status = tr("canonical.queue.unknownExecution")
			return then(nil), nil
		}
		text := jsTemplate(server, "user")
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
