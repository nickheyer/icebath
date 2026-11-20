package tui

import "strings"

// Watch over the sessions this view left running: a session left mid turn keeps working out of sight, and the poll of the live sessions notices its
// turn ending or a question waiting and says so in the transcript in view. Only sessions this view has shown count, since the shared gateway also
// lists the chats of other clients
type backgroundWatch struct {
	shown map[string]bool
	last  map[string]string
}

func newBackgroundWatch() *backgroundWatch {
	return &backgroundWatch{shown: map[string]bool{}, last: map[string]string{}}
}

// Whether a listed session is working, waiting on an answer or idle
func backgroundPhase(row listedSession) string {
	status := row.Status
	if status == "" {
		status = "idle"
		if row.Running {
			status = "working"
		}
	}
	switch status {
	case "waiting":
		return "waiting"
	case "working", "starting":
		return "busy"
	}
	return "idle"
}

// Marks a session as one this view displayed, its later turns worth announcing
func (w *backgroundWatch) focus(sid string) {
	if sid != "" {
		w.shown[sid] = true
	}
}

// A session was just left mid turn: it is this view's and busy whatever the last poll sampled, so that a turn that ends before the next poll still
// gets its notice
func (w *backgroundWatch) handoff(sid string) {
	if sid != "" {
		w.shown[sid], w.last[sid] = true, "busy"
	}
}

// Whether a session this view showed is live off screen, the only case in which the waiting state is worth a second read
func (w *backgroundWatch) watching(rows []listedSession, focused string) bool {
	for _, row := range rows {
		if row.ID != "" && row.ID != focused && w.shown[row.ID] {
			return true
		}
	}
	return false
}

// Notices of one poll: a shown session out of view whose turn finished or that now waits on an answer
func (w *backgroundWatch) update(rows []listedSession, focused string) []string {
	var notices []string
	seen := map[string]bool{}
	for _, row := range rows {
		id := row.ID
		if id == "" {
			continue
		}
		seen[id] = true
		phase := backgroundPhase(row)
		before, known := w.last[id]
		w.last[id] = phase
		if id == focused || !w.shown[id] || !known || before == phase {
			continue
		}
		label := strings.TrimSpace(row.Title)
		if label == "" {
			label = jsSlice(id, 0, 12)
		}
		switch {
		case phase == "waiting":
			notices = append(notices, tr("session.lifecycle.backgroundWaiting", label))
		case phase == "idle" && before == "busy":
			notices = append(notices, tr("session.lifecycle.backgroundFinished", label))
		}
	}
	for id := range w.last {
		if !seen[id] {
			delete(w.last, id)
		}
	}
	return notices
}
