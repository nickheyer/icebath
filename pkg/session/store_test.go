package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/nickheyer/icebath/pkg/protocol"
)

// Child process mode, leasing the session named by the environment until stdin closes
func TestMain(m *testing.M) {
	if dir := os.Getenv("SESSION_LEASE_DIR"); dir != "" {
		st := &Store{Dir: dir}
		if err := st.Lease(os.Getenv("SESSION_LEASE_ID")); err != nil {
			os.Exit(3)
		}
		os.Stdout.Write([]byte("held\n"))
		buf := make([]byte, 1)
		os.Stdin.Read(buf)
		st.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func holdLease(t *testing.T, dir, id string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "SESSION_LEASE_DIR="+dir, "SESSION_LEASE_ID="+id)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := out.Read(buf); err != nil || string(buf) != "held\n" {
		cmd.Process.Kill()
		t.Fatalf("child did not take the lease: %v %q", err, buf)
	}
	return func() {
		in.Close()
		cmd.Wait()
	}
}

func stored(t *testing.T, st *Store, project string) *Session {
	t.Helper()
	s := New()
	s.Label(ProjectLabel, project)
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	if err := st.Save(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLeaseBlocksOtherProcess(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := stored(t, st, "/a")
	release := holdLease(t, st.Dir, s.ID())
	if err := st.Lease(s.ID()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Lease while another process holds it: %v, want ErrBusy", err)
	}
	if !st.Busy(s.ID()) {
		t.Fatal("Busy false while another process holds the lease")
	}
	infos, err := st.List(context.Background())
	if err != nil || len(infos) != 1 || !infos[0].Busy {
		t.Fatalf("List did not mark the session busy: %+v %v", infos, err)
	}
	if err := st.Delete(context.Background(), s.ID()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Delete of a busy session: %v, want ErrBusy", err)
	}
	release()
	if st.Busy(s.ID()) {
		t.Fatal("Busy after the other process let go")
	}
	if err := st.Lease(s.ID()); err != nil {
		t.Fatalf("Lease after release: %v", err)
	}
	if st.Busy(s.ID()) {
		t.Fatal("own lease reads as busy")
	}
	if err := st.Lease(s.ID()); err != nil {
		t.Fatalf("second Lease by the holder: %v", err)
	}
	if err := st.Delete(context.Background(), s.ID()); err != nil {
		t.Fatalf("Delete of an own session: %v", err)
	}
	if st.Leased(s.ID()) {
		t.Fatal("lease kept after delete")
	}
	if _, err := os.Stat(filepath.Join(st.Dir, s.ID()+".lease")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lease file left after delete")
	}
}

func TestUnleaseDropsFileOfUnwrittenSession(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := New()
	if err := st.Lease(s.ID()); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(st.Dir, s.ID()+".lease")
	if _, err := os.Stat(lease); err != nil {
		t.Fatal("lease file not written")
	}
	st.Unlease(s.ID())
	if _, err := os.Stat(lease); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("lease file of a session never saved left behind")
	}
	w := stored(t, st, "/a")
	st.Lease(w.ID())
	st.Unlease(w.ID())
	if _, err := os.Stat(filepath.Join(st.Dir, w.ID()+".lease")); err != nil {
		t.Fatal("lease file of a stored session removed, which would race another opener")
	}
}

func TestListProject(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	a := stored(t, st, "/a")
	stored(t, st, "/b")
	infos, err := st.ListProject(context.Background(), "/a")
	if err != nil || len(infos) != 1 || infos[0].ID != a.ID() {
		t.Fatalf("ListProject: %+v %v", infos, err)
	}
	all, err := st.List(context.Background())
	if err != nil || len(all) != 2 {
		t.Fatalf("List: %+v %v", all, err)
	}
	if all[0].Labels[ProjectLabel] == "" {
		t.Fatal("project label lost in the listing")
	}
}

func TestProjectSurvivesStore(t *testing.T) {
	st := &Store{Dir: t.TempDir()}
	s := stored(t, st, "/p")
	loaded, err := st.Load(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project() != "/p" {
		t.Fatalf("Project after load: %q", loaded.Project())
	}
	if f := loaded.Fork(); f.Project() != "/p" {
		t.Fatalf("Project after fork: %q", f.Project())
	}
}

func TestTruncateKeepsEarlierRecords(t *testing.T) {
	s := New()
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "hi"}}})
	s.AddResponse("a", protocol.Response{Content: []protocol.Part{protocol.Text{Text: "hello"}}})
	s.Log("a", "run_end", map[string]any{"error": "context deadline exceeded"})
	s.Log("a", "run_start", map[string]any{"input": "again"})
	s.Append(protocol.Message{Role: protocol.RoleUser, Content: []protocol.Part{protocol.Text{Text: "again"}}})
	s.Truncate(s.Len() - 1)
	if s.Len() != 2 {
		t.Fatalf("%d entries kept", s.Len())
	}
	records := s.Records()
	if len(records) != 2 || records[0].Kind != "run_end" || records[1].Kind != "run_start" {
		t.Fatalf("records kept: %+v", records)
	}
	s.Truncate(1)
	if len(s.Records()) != 0 {
		t.Fatalf("records newer than the dropped entry kept: %+v", s.Records())
	}
}
