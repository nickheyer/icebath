package files

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// fakeHost stands in for the shell, recording dialogs and broadcasts
type fakeHost struct {
	mu         sync.Mutex
	main       WatchOwner
	open       func(WatchOwner, OpenDialogOptions) DialogResult
	save       func(SaveDialogOptions) SaveDialogResult
	openCalls  []OpenDialogOptions
	openParent []WatchOwner
	broadcasts []sent
}

func (h *fakeHost) MainWindow() WatchOwner { return h.main }

func (h *fakeHost) ShowOpenDialog(parent WatchOwner, o OpenDialogOptions) (DialogResult, error) {
	h.openCalls = append(h.openCalls, o)
	h.openParent = append(h.openParent, parent)
	return h.open(parent, o), nil
}

func (h *fakeHost) ShowSaveDialog(_ WatchOwner, o SaveDialogOptions) (SaveDialogResult, error) {
	return h.save(o), nil
}

func (h *fakeHost) Broadcast(event string, payload any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.broadcasts = append(h.broadcasts, sent{event, payload})
}

// Files group over a scratch home with the launch profile active and reveal recorded
func fsIpcService(t *testing.T) (*Service, string, *[]string) {
	scratch := realTemp(t)
	t.Setenv("HOME", scratch)
	userData := filepath.Join(scratch, "userData")
	writeFile(t, filepath.Join(userData, "active-profile.json"), `{"profile":"launch-profile"}`)
	var revealed []string
	orig := showItemInFolder
	showItemInFolder = func(p string) { revealed = append(revealed, p) }
	t.Cleanup(func() { showItemInFolder = orig })
	return NewService(&fakeHost{}, Config{HermesHome: scratch, UserData: userData}), scratch, &revealed
}

func TestHermesFsRevealRevealsAPathThatExistsOnThisComputer(t *testing.T) {
	s, scratch, revealed := fsIpcService(t)
	file := filepath.Join(scratch, "workspace")
	os.Mkdir(file, 0o755)
	if !s.RevealPath(file) || !reflect.DeepEqual(*revealed, []string{file}) {
		t.Fatal(*revealed)
	}
}

func TestHermesFsRevealReportsFalseWithoutTouchingTheFileManagerWhenThePathIsMissing(t *testing.T) {
	s, scratch, revealed := fsIpcService(t)
	if s.RevealPath(filepath.Join(scratch, "not-here")) || len(*revealed) != 0 {
		t.Fatal(*revealed)
	}
}

func TestHermesFsRevealExpandsATildePathBeforeCheckingAndRevealingIt(t *testing.T) {
	s, scratch, revealed := fsIpcService(t)
	here := filepath.Join(scratch, "tilde.md")
	writeFile(t, here, "x")
	if !s.RevealPath("~/tilde.md") || !reflect.DeepEqual(*revealed, []string{here}) {
		t.Fatal(*revealed)
	}
}

func TestHermesFsLogsRootResolvesTheLogsDirOfTheProfileThatOwnsTheSession(t *testing.T) {
	s, scratch, _ := fsIpcService(t)
	if s.LogsRoot("finex") != filepath.Join(scratch, "profiles", "finex", "logs") || s.LogsRoot("default") != filepath.Join(scratch, "logs") {
		t.Fatal(s.LogsRoot("finex"), s.LogsRoot("default"))
	}
}

func TestHermesFsLogsRootFallsBackToTheActiveDesktopProfileWhenNoOwnerIsNamed(t *testing.T) {
	s, scratch, _ := fsIpcService(t)
	if got := s.LogsRoot(nil); got != filepath.Join(scratch, "profiles", "launch-profile", "logs") {
		t.Fatal(got)
	}
}

func TestHermesFsLogsRootNeverLeavesHermesHomeForAnOwnerThatIsNotAProfileName(t *testing.T) {
	s, scratch, _ := fsIpcService(t)
	for _, bad := range []string{"../../x", "/etc", "a/b", "Upper", ".hidden"} {
		root := s.LogsRoot(bad)
		rel, _ := filepath.Rel(scratch, root)
		if strings.HasPrefix(rel, "..") || root != filepath.Join(scratch, "profiles", "launch-profile", "logs") {
			t.Fatal(bad, root)
		}
	}
}
