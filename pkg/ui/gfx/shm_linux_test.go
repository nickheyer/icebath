package gfx

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Kitty terminal on this machine, verified and sending frames through shared memory
func sharedTerm(t *testing.T) (*Terminal, *clock) {
	t.Helper()
	term, c := kittyTerm("TERM", "xterm-kitty")
	term.shm = shmWorks()
	if !term.shared() {
		t.Fatal("this linux machine refused a shared memory object")
	}
	t.Cleanup(Sweep)
	return term, c
}

// Reports whether a shared memory object of the name exists
func objectExists(name string) bool {
	_, err := os.Stat(shmDir + name)
	return err == nil
}

// Frames go through a shared memory object named for the process, image and frame, holding the raw pixels
func TestSharedTransmission(t *testing.T) {
	term, c := sharedTerm(t)
	s := term.NewSurface(KittyCells)
	px := ramp(16, 8)
	p := s.Pack(px, 16, 8, 4, 2)
	if p.shm == "" || !objectExists(p.shm) || p.first != nil {
		t.Fatalf("packet %q holds no object", p.shm)
	}
	out := s.Emit(p, 0, 0, c.now)
	cmds := gcmds(out)
	k := cmds[0].keys
	if len(cmds) != 1 || k["t"] != "s" || k["S"] != "384" || k["o"] != "" || k["q"] != "0" || k["U"] != "1" {
		t.Fatalf("shared transmission %q", out)
	}
	name, err := base64.StdEncoding.DecodeString(cmds[0].payload)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/icebath-%d-%s-1", os.Getpid(), k["i"])
	if string(name) != want {
		t.Fatalf("object %q, want %q", name, want)
	}
	data, err := os.ReadFile(shmDir + strings.TrimPrefix(string(name), "/"))
	if err != nil || !bytes.Equal(data, px) {
		t.Fatalf("object holds %d bytes, %v", len(data), err)
	}
	os.Remove(shmDir + strings.TrimPrefix(string(name), "/"))
	if !term.Update(answer(idOf(t, cmds), "OK")) || s.Stats().Acked != 1 {
		t.Fatal("the acknowledgement went unreported")
	}
	again := s.Emit(p, 2, 2, c.add(time.Second))
	if !strings.Contains(again, "t=s") {
		t.Fatalf("showing the packet again lost its object %q", again)
	}
	next := s.Pack(px, 16, 8, 4, 2)
	s.Emit(next, 0, 0, c.add(time.Second))
	if objectExists(p.shm) || !objectExists(next.shm) {
		t.Fatal("the older packet kept its object after a newer one showed")
	}
	stale := s.Pack(px, 16, 8, 4, 2)
	s.Release()
	if objectExists(next.shm) || objectExists(stale.shm) {
		t.Fatal("release left packed objects")
	}
	matches, _ := filepath.Glob(fmt.Sprintf("%sicebath-%d-%d-*", shmDir, os.Getpid(), s.ids[0]))
	more, _ := filepath.Glob(fmt.Sprintf("%sicebath-%d-%d-*", shmDir, os.Getpid(), s.ids[1]))
	if len(matches)+len(more) != 0 {
		t.Fatalf("release left %v %v", matches, more)
	}
	s.Pack(px, 16, 8, 4, 2)
	Sweep()
	if all, _ := filepath.Glob(fmt.Sprintf("%sicebath-%d-*", shmDir, os.Getpid())); len(all) != 0 {
		t.Fatalf("sweep left %v", all)
	}
}

// A refused shared memory frame, as konsole answers ENOTSUPPORTED, switches to direct for good without a failure
func TestSharedRefusalSwitchesToDirect(t *testing.T) {
	for _, refusal := range []string{"ENOTSUPPORTED:", "EBADF:Failed to open", "ENOENT:no such object"} {
		term, c := sharedTerm(t)
		s := term.NewSurface(KittyCells)
		px := grainy(64, 40)
		first := s.Pack(px, 64, 40, 8, 2)
		waiting := s.Pack(px, 64, 40, 8, 2)
		out := s.Emit(first, 0, 0, c.now)
		cmds := gcmds(out)
		name, _ := base64.StdEncoding.DecodeString(cmds[0].payload)
		if !term.Update(answer(idOf(t, cmds), refusal)) {
			t.Fatalf("%s went unreported", refusal)
		}
		if term.shared() || term.Protocol() != KittyCells || strings.Contains(term.Describe(), "shared") {
			t.Fatalf("%s left shared memory on or broke the protocol", refusal)
		}
		st := s.Stats()
		if st.Failed != 0 || st.Divisor != directDivisor || !s.Ready(c.now) {
			t.Fatalf("%s stats %+v", refusal, st)
		}
		if objectExists(strings.TrimPrefix(string(name), "/")) {
			t.Fatalf("%s left the refused object", refusal)
		}
		held := waiting.shm
		out = s.Emit(waiting, 0, 0, c.add(time.Millisecond))
		tx := transmissions(gcmds(out))
		if strings.Contains(out, "t=s") || !bytes.Equal(directPixels(t, tx), px) {
			t.Fatalf("a frame packed before %s went out through shared memory", refusal)
		}
		if held == "" || objectExists(held) || waiting.shm != "" {
			t.Fatalf("%s left the object of the converted packet", refusal)
		}
		later := s.Pack(px, 64, 40, 8, 2)
		if later.shm != "" || strings.Contains(s.Emit(later, 0, 0, c.add(time.Second)), "t=s") {
			t.Fatalf("a frame packed after %s used shared memory", refusal)
		}
		if w, h := s.Budget(200, 60); float64(w*h) > 0.8e6 {
			t.Fatalf("budget after %s is %dx%d past the direct cap", refusal, w, h)
		}
	}
}

func BenchmarkPackKittyShared(b *testing.B) {
	term, _ := kittyTerm("TERM", "xterm-kitty")
	term.shm = shmWorks()
	s := term.NewSurface(KittyCells)
	w, h := 1360, 960
	rgb := grainy(w, h)
	b.ReportAllocs()
	for b.Loop() {
		p := s.Pack(rgb, w, h, 136, 48)
		s.mu.Lock()
		s.dropLocked(p.seq)
		s.mu.Unlock()
	}
	Sweep()
}

// Objects of icebath processes that no longer run are removed once they are a minute old, others stay
func TestReapDeadObjects(t *testing.T) {
	old := time.Now().Add(-2 * time.Minute)
	dead := "icebath-999999999-2883584-1"
	fresh := "icebath-999999998-2883584-1"
	mine := fmt.Sprintf("icebath-%d-2883584-77", os.Getpid())
	other := "icebathx-999999999-1-1"
	for _, n := range []string{dead, fresh, mine, other} {
		if err := shmPut(n, []byte{1}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { shmRemove(n) })
	}
	for _, n := range []string{dead, mine, other} {
		os.Chtimes(shmDir+n, old, old)
	}
	NewWith(envOf("TERM", "xterm-kitty"))
	if objectExists(dead) || !objectExists(fresh) || !objectExists(mine) || !objectExists(other) {
		t.Fatalf("reaped dead %v fresh %v mine %v other %v", !objectExists(dead), !objectExists(fresh), !objectExists(mine), !objectExists(other))
	}
}
