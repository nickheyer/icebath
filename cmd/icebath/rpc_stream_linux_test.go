//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// JSON RPC frames of one transport, replies kept by id and pushed events kept in arrival order
type rpcStream struct {
	mu      sync.Mutex
	wake    chan struct{}
	replies map[string]map[string]any
	events  []map[string]any
	ended   error
	send    func(frame map[string]any) error
	ids     int
	logs    func() string
}

// Starts the reader that routes every frame the transport yields until it ends
func newRPCStream(read func() (map[string]any, error), send func(map[string]any) error, logs func() string) *rpcStream {
	s := &rpcStream{wake: make(chan struct{}), replies: map[string]map[string]any{}, send: send, logs: logs}
	go func() {
		for {
			frame, err := read()
			s.mu.Lock()
			if err != nil {
				s.ended = err
			} else if frame["method"] == "event" {
				params, _ := frame["params"].(map[string]any)
				s.events = append(s.events, params)
			} else if id, has := frame["id"]; has {
				if _, isReply := frame["result"]; isReply {
					s.replies[fmt.Sprint(id)] = frame
				} else if _, isRefusal := frame["error"]; isRefusal {
					s.replies[fmt.Sprint(id)] = frame
				}
			}
			close(s.wake)
			s.wake = make(chan struct{})
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return s
}

// Reply frame of one request, failing the case when the transport ends or the deadline passes
func (s *rpcStream) call(t *testing.T, method string, params map[string]any, timeout time.Duration) map[string]any {
	t.Helper()
	if params == nil {
		params = map[string]any{}
	}
	s.mu.Lock()
	s.ids++
	id := fmt.Sprintf("r%d", s.ids)
	s.mu.Unlock()
	if err := s.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		t.Fatalf("%s: %v\n%s", method, err, s.logs())
	}
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		reply, done := s.replies[id]
		ended, wake := s.ended, s.wake
		delete(s.replies, id)
		s.mu.Unlock()
		if done {
			return reply
		}
		if ended != nil {
			t.Fatalf("%s: the transport ended: %v\n%s", method, ended, s.logs())
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("%s: no reply in %v\n%s", method, timeout, s.logs())
		}
	}
}

// Result of a request that must succeed
func (s *rpcStream) result(t *testing.T, method string, params map[string]any) map[string]any {
	t.Helper()
	reply := s.call(t, method, params, 60*time.Second)
	result, ok := reply["result"].(map[string]any)
	if !ok {
		t.Fatalf("%s: %v\n%s", method, reply, s.logs())
	}
	return result
}

// First pushed event from index start that matches, failing the case at the deadline
func (s *rpcStream) waitEvent(t *testing.T, start int, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for seen := start; ; {
		s.mu.Lock()
		events, ended, wake := s.events, s.ended, s.wake
		s.mu.Unlock()
		for ; seen < len(events); seen++ {
			if match(events[seen]) {
				return events[seen]
			}
		}
		if ended != nil {
			t.Fatalf("the transport ended before the event: %v\nevents %s\n%s", ended, s.eventTypes(start), s.logs())
		}
		select {
		case <-wake:
		case <-deadline:
			t.Fatalf("no matching event in %v, saw %s\n%s", timeout, s.eventTypes(start), s.logs())
		}
	}
}

// Count of pushed events so far, the start index of a later wait
func (s *rpcStream) eventCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.events)
}

// Types of the pushed events from index start, what a failed wait prints
func (s *rpcStream) eventTypes(start int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var types []string
	for _, event := range s.events[min(start, len(s.events)):] {
		types = append(types, fmt.Sprint(event["type"]))
	}
	return strings.Join(types, " ")
}

// Matches a pushed event of the type for the session
func eventOf(kind, sid string) func(map[string]any) bool {
	return func(event map[string]any) bool { return event["type"] == kind && event["session_id"] == sid }
}

// JSON text of a value, what a transcript check searches
func jsonText(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
