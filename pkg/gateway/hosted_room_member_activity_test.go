package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Real driver path whose member turn stays running until the test releases it
type hostedHeldRPC struct {
	mu       sync.Mutex
	started  chan struct{}
	release  chan struct{}
	once     sync.Once
	sessions map[[2]string]map[string]any
}

func newHostedHeldRPC() *hostedHeldRPC {
	return &hostedHeldRPC{started: make(chan struct{}), release: make(chan struct{}), sessions: map[[2]string]map[string]any{}}
}

func (h *hostedHeldRPC) AcceptsAttachments() bool { return false }

func (h *hostedHeldRPC) ResolveExact(_ context.Context, profile, title, _ string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sessions[[2]string{profile, title}], nil
}

func (h *hostedHeldRPC) Create(_ context.Context, profile, title, _ string) (map[string]any, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	session := map[string]any{"session_id": profile + "-session", "title": title}
	h.sessions[[2]string{profile, title}] = session
	return session, nil
}

func (h *hostedHeldRPC) Resume(_ context.Context, _, sessionID, _ string) (map[string]any, error) {
	return map[string]any{"session_id": sessionID}, nil
}

func (h *hostedHeldRPC) Submit(_ context.Context, req HostedSubmit) (map[string]any, error) {
	h.once.Do(func() { close(h.started) })
	select {
	case <-h.release:
	case <-time.After(5 * time.Second):
		return nil, errors.New("release was never set")
	}
	if err := req.OnTerminal(map[string]any{"status": "settled", "text": "reply from " + req.Profile}); err != nil {
		return nil, err
	}
	return map[string]any{"accepted": true}, nil
}

func (h *hostedHeldRPC) History(context.Context, string, string, string) ([]map[string]any, error) {
	return []map[string]any{}, nil
}

func (h *hostedHeldRPC) Info(context.Context, string, string, string) (map[string]any, error) {
	return map[string]any{"active": false, "task_id": nil}, nil
}

func (h *hostedHeldRPC) Interrupt(context.Context, string, string, string, string) (map[string]any, error) {
	return nil, errors.New("interrupt is not part of the held turn")
}

func (h *hostedHeldRPC) Approve(context.Context, string, string, string) (map[string]any, error) {
	return nil, errors.New("approve is not part of the held turn")
}

// Service whose local profiles the case pins to default and ops
type hostedPinnedProfiles struct{ *HostedRoomService }

func (p hostedPinnedProfiles) LocalProfiles() ([]string, error) {
	return []string{"default", "ops"}, nil
}

func TestStatusNamesTheMemberAnsweringWhileItsTurnRuns(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	held := newHostedHeldRPC()
	service := &HostedRoomService{}
	if err := service.init(context.Background(), hostedPinnedProfiles{service}, HostedRoomServiceOptions{
		DBPath: filepath.Join(t.TempDir(), "state.db"), MakeRPC: func(string) (HostedRoomRPC, error) { return held, nil }}); err != nil {
		t.Fatal(err)
	}
	service.runtime.RPC = held
	if _, err := service.CreateRoom("room-1", "Release room", []any{
		map[string]any{"member_id": "default", "profile": "default", "handle": "hermes"},
		map[string]any{"member_id": "ops", "profile": "ops", "handle": "ops"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(); err != nil {
		t.Fatal(err)
	}
	defer service.Stop(5)
	if _, err := service.Send("room-1", "user-1", map[string]any{"text": "@ops inspect the release", "thread_id": "thread-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-held.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the ops turn never started")
	}
	room := "room-1"
	status, err := service.Status(&room)
	if err != nil {
		t.Fatal(err)
	}
	if status["working"] != true {
		t.Fatalf("working = %v", status["working"])
	}
	if !reflect.DeepEqual(status["member_activity"], []any{map[string]any{"member_id": "ops", "status": "running"}}) {
		t.Fatalf("member_activity = %#v", status["member_activity"])
	}

	close(held.release)
	if !waitUntil(5*time.Second, func() bool {
		status, err := service.Status(&room)
		activity, _ := status["member_activity"].([]any)
		return err == nil && len(activity) == 0
	}) {
		t.Fatal("member activity never cleared")
	}
	status, err = service.Status(&room)
	if err != nil || status["working"] != false {
		t.Fatalf("working after release = %v, %v", status["working"], err)
	}
}
