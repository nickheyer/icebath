package tools

import (
	"container/list"
	"context"
	"sync"
	"time"
)

// Deferred verdict queue limits and the bounded turn end wait
const (
	lspDeferredMaxOwners   = 64
	lspDeferredMaxPerOwner = 32
	LSPTurnEndWait         = 5 * time.Second
)

// LSPDeferredNote is one undelivered late verdict of a file
type LSPDeferredNote struct {
	Path string
	Text string
}

// Undelivered notes of one owner by path, oldest first
type lspOwnerNotes struct {
	order *list.List
	index map[string]*list.Element
}

type lspPathNote struct {
	path string
	note lspNote
}

func newLSPOwnerNotes() *lspOwnerNotes {
	return &lspOwnerNotes{order: list.New(), index: map[string]*list.Element{}}
}

func (n *lspOwnerNotes) pop(path string) (lspNote, bool) {
	e, ok := n.index[path]
	if !ok {
		return lspNote{}, false
	}
	delete(n.index, path)
	n.order.Remove(e)
	return e.Value.(*lspPathNote).note, true
}

func (n *lspOwnerNotes) set(path string, note lspNote) {
	if e, ok := n.index[path]; ok {
		e.Value.(*lspPathNote).note = note
		return
	}
	n.index[path] = n.order.PushBack(&lspPathNote{path, note})
}

var lspDeferred = struct {
	mu      sync.Mutex
	owners  *list.List
	byOwner map[string]*list.Element
	pending map[string]map[any]bool
	settled chan struct{}
}{owners: list.New(), byOwner: map[string]*list.Element{}, pending: map[string]map[any]bool{}, settled: make(chan struct{})}

// One owner row of the queue
type lspOwnerRow struct {
	owner string
	notes *lspOwnerNotes
}

// Session late verdicts of the current write are attributed to, empty when unbound
func lspDeferredCurrentOwner(ctx context.Context, _ *Env) string {
	if ctx == nil {
		return ""
	}
	return LSPOwner(ctx)
}

// Removes and returns an owner's notes with the queue lock held
func lspDeferredPopOwner(owner string) *lspOwnerNotes {
	e, ok := lspDeferred.byOwner[owner]
	if !ok {
		return nil
	}
	delete(lspDeferred.byOwner, owner)
	lspDeferred.owners.Remove(e)
	return e.Value.(*lspOwnerRow).notes
}

// Queues a formatted block for the owner, a note still queued for the path kept in front with its baseline
func lspDeferredPost(owner, path, text string, baseline *string) {
	lspDeferred.mu.Lock()
	defer lspDeferred.mu.Unlock()
	notes := lspDeferredPopOwner(owner)
	if notes == nil {
		notes = newLSPOwnerNotes()
	}
	if older, ok := notes.pop(path); ok && older.Text != text {
		text, baseline = older.Text+"\n\n"+text, older.BaselineText
	}
	notes.set(path, lspNote{Text: text, BaselineText: baseline})
	for notes.order.Len() > lspDeferredMaxPerOwner {
		front := notes.order.Front()
		delete(notes.index, front.Value.(*lspPathNote).path)
		notes.order.Remove(front)
	}
	lspDeferred.byOwner[owner] = lspDeferred.owners.PushBack(&lspOwnerRow{owner, notes})
	for lspDeferred.owners.Len() > lspDeferredMaxOwners {
		front := lspDeferred.owners.Front()
		delete(lspDeferred.byOwner, front.Value.(*lspOwnerRow).owner)
		lspDeferred.owners.Remove(front)
	}
}

// Withdraws the owner's undelivered note for a path so a newer edit of the file takes it over
func lspDeferredTake(owner, path string) *lspNote {
	if owner == "" {
		return nil
	}
	lspDeferred.mu.Lock()
	defer lspDeferred.mu.Unlock()
	e, ok := lspDeferred.byOwner[owner]
	if !ok {
		return nil
	}
	note, ok := e.Value.(*lspOwnerRow).notes.pop(path)
	if !ok {
		return nil
	}
	return &note
}

// Queues withdrawn notes again, oldest first
func lspDeferredRestore(owner, path string, notes []lspNote) {
	if owner == "" {
		return
	}
	for _, n := range notes {
		lspDeferredPost(owner, path, n.Text, n.BaselineText)
	}
}

// LSPPopNotes returns and clears every queued note of the owners, oldest first
func LSPPopNotes(owners ...string) []LSPDeferredNote {
	lspDeferred.mu.Lock()
	defer lspDeferred.mu.Unlock()
	return lspDeferredPopLocked(owners)
}

func lspDeferredPopLocked(owners []string) []LSPDeferredNote {
	out := []LSPDeferredNote{}
	seen := map[string]bool{}
	for _, owner := range owners {
		if owner == "" || seen[owner] {
			continue
		}
		seen[owner] = true
		notes := lspDeferredPopOwner(owner)
		if notes == nil {
			continue
		}
		for e := notes.order.Front(); e != nil; e = e.Next() {
			pn := e.Value.(*lspPathNote)
			out = append(out, LSPDeferredNote{Path: pn.path, Text: pn.note.Text})
		}
	}
	return out
}

// Records a running check of the owner, the token handed to lspDeferredSettled once its verdict landed
func lspDeferredTrackPending(owner string) any {
	token := new(byte)
	lspDeferred.mu.Lock()
	defer lspDeferred.mu.Unlock()
	if lspDeferred.pending[owner] == nil {
		lspDeferred.pending[owner] = map[any]bool{}
	}
	lspDeferred.pending[owner][token] = true
	return token
}

// Drops a settled check and wakes every turn end waiting on the owner
func lspDeferredSettled(owner string, token any) {
	lspDeferred.mu.Lock()
	defer lspDeferred.mu.Unlock()
	if tokens := lspDeferred.pending[owner]; tokens != nil {
		delete(tokens, token)
		if len(tokens) == 0 {
			delete(lspDeferred.pending, owner)
		}
	}
	close(lspDeferred.settled)
	lspDeferred.settled = make(chan struct{})
}

// Distinct non empty owners of an agent, session first then task
func lspAgentOwners(sessionID, taskID string) []string {
	var out []string
	for _, o := range []string{sessionID, taskID} {
		if o != "" && (len(out) == 0 || out[0] != o) {
			out = append(out, o)
		}
	}
	return out
}

// LSPSettleForAgent waits up to timeout for the agent's running checks, then pops every undelivered verdict
func LSPSettleForAgent(sessionID, taskID string, timeout time.Duration) []LSPDeferredNote {
	owners := lspAgentOwners(sessionID, taskID)
	if len(owners) == 0 {
		return []LSPDeferredNote{}
	}
	deadline := time.Now().Add(timeout)
	lspDeferred.mu.Lock()
	for {
		running := false
		for _, o := range owners {
			if len(lspDeferred.pending[o]) > 0 {
				running = true
				break
			}
		}
		remaining := time.Until(deadline)
		if !running || remaining <= 0 {
			break
		}
		wake := lspDeferred.settled
		lspDeferred.mu.Unlock()
		t := time.NewTimer(remaining)
		select {
		case <-wake:
		case <-t.C:
		}
		t.Stop()
		lspDeferred.mu.Lock()
	}
	out := lspDeferredPopLocked(owners)
	lspDeferred.mu.Unlock()
	return out
}

// LSPForwardToAgent moves the agent's finished verdicts onto its live notes queue under lsp path keys
func LSPForwardToAgent(sessionID, taskID string, queuePluginNotice func(key, text string)) {
	for _, n := range LSPPopNotes(sessionID, taskID) {
		queuePluginNotice("lsp:"+n.Path, n.Text)
	}
}
