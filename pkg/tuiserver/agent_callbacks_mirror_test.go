package tuiserver

import (
	"sync"
)

// One emitted session event
type emitted struct {
	event, sid string
	payload    map[string]any
}

// Recorder of every emitted session event
type emitLog struct {
	sync.Mutex
	list []emitted
}

func (l *emitLog) all() []emitted {
	l.Lock()
	defer l.Unlock()
	return append([]emitted(nil), l.list...)
}

func (l *emitLog) clear() {
	l.Lock()
	l.list = nil
	l.Unlock()
}

func (l *emitLog) eventsFor(sid string) []emitted {
	out := []emitted{}
	for _, e := range l.all() {
		if e.sid == sid {
			out = append(out, e)
		}
	}
	return out
}

func mirrorCount() int {
	childMirrorsMu.Lock()
	defer childMirrorsMu.Unlock()
	return len(childMirrors)
}

func eventNames(list []emitted) []string {
	out := []string{}
	for _, e := range list {
		out = append(out, e.event)
	}
	return out
}
