package state

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
)

// Maps a /proc prefixed path into the fake procfs rooted at root
func projectProc(root, path string) string {
	if rest, ok := strings.CutPrefix(path, "/proc"); ok {
		return root + rest
	}
	return path
}

// Projects the holder scan onto a fake procfs under base with our pid pinned to 111
func installFakeProc(t *testing.T, base string, unreadable, fdPIDs []int) string {
	t.Helper()
	procRoot := filepath.Join(base, "proc")
	if err := os.MkdirAll(procRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pid := range append(slices.Clone(unreadable), fdPIDs...) {
		if err := os.MkdirAll(filepath.Join(procRoot, itoa(pid)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, pid := range fdPIDs {
		fd := filepath.Join(procRoot, itoa(pid), "fd")
		if err := os.MkdirAll(fd, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fd, "3"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prevSys, prevPid := holdersSys, holderGetpid
	t.Cleanup(func() { holdersSys, holderGetpid = prevSys, prevPid })
	holderGetpid = func() int { return 111 }
	holdersSys = holderSys{
		listdir: func(path string) ([]string, error) {
			for _, pid := range unreadable {
				if path == "/proc/"+itoa(pid)+"/fd" {
					return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EACCES}
				}
			}
			return listdirUnordered(projectProc(procRoot, path))
		},
		readlink: func(path string) (string, error) {
			if strings.Contains(path, "222/fd/3") {
				return "", &os.PathError{Op: "readlink", Path: path, Err: syscall.EACCES}
			}
			return os.Readlink(projectProc(procRoot, path))
		},
		stat: func(path string) (FileIdentity, error) { return statIdentity(projectProc(procRoot, path)) },
	}
	return procRoot
}

func itoa(n int) string { return pyFormat(n) }

func mustHolders(t *testing.T, dbPath string) []Holder {
	t.Helper()
	holders, err := ForeignStateDBHolders(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	return holders
}

func holderPIDs(holders []Holder) []int {
	pids := []int{}
	for _, h := range holders {
		pids = append(pids, h.PID)
	}
	return pids
}

func TestForeignHolderUsesDeviceAndInodeNotPath(t *testing.T) {
	for _, identity := range []string{"alias", "different-inode", "different-device"} {
		t.Run(identity, func(t *testing.T) {
			tmp := t.TempDir()
			dbPath := filepath.Join(tmp, "state.db")
			if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			aliasPath := dbPath
			if identity == "alias" {
				aliasPath = filepath.Join(tmp, "namespace-alias", "state.db")
			}
			procRoot := filepath.Join(tmp, "proc")
			for _, pid := range []string{"111", "222"} {
				if err := os.MkdirAll(filepath.Join(procRoot, pid, "fd"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(dbPath, filepath.Join(procRoot, "222", "fd", "3")); err != nil {
				t.Fatal(err)
			}
			prevSys, prevPid := holdersSys, holderGetpid
			t.Cleanup(func() { holdersSys, holderGetpid = prevSys, prevPid })
			holderGetpid = func() int { return 111 }
			holdersSys = holderSys{
				listdir: func(path string) ([]string, error) { return listdirUnordered(projectProc(procRoot, path)) },
				readlink: func(path string) (string, error) {
					if path == "/proc/222/fd/3" {
						return aliasPath, nil
					}
					return os.Readlink(projectProc(procRoot, path))
				},
				stat: func(path string) (FileIdentity, error) {
					id, err := statIdentity(projectProc(procRoot, path))
					if err == nil && path == "/proc/222/fd/3" && identity != "alias" {
						if identity == "different-inode" {
							id.Ino++
						} else {
							id.Dev++
						}
					}
					return id, err
				},
			}
			want := []Holder{}
			if identity == "alias" {
				want = []Holder{{222, aliasPath}}
			}
			if got := mustHolders(t, dbPath); !slices.Equal(got, want) {
				t.Fatalf("holders = %v, want %v", got, want)
			}
		})
	}
}

var (
	demoHomeArgv  = []string{"/home/demo/.icebath/icebath-agent/icebath", "gateway", "run"}
	demoVenvArgv  = []string{"/home/demo/.icebath/icebath-agent/venv/bin/python", "/home/demo/.icebath/icebath-agent/icebath_cli/main.py", "gateway"}
	ambiguousArgv = []string{"icebath", "gateway", "run"}
)

func TestOtherInstanceArgvIsNotAHolderOfOurDB(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".icebath", "state.db")
	installFakeProc(t, tmp, []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: demoHomeArgv})
	if got := mustHolders(t, dbPath); len(got) != 0 {
		t.Fatalf("holders = %v", got)
	}
}

func TestOtherInstanceVenvLauncherIsNotAHolderOfOurDB(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".icebath", "state.db")
	installFakeProc(t, tmp, []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: demoVenvArgv})
	if got := mustHolders(t, dbPath); len(got) != 0 {
		t.Fatalf("holders = %v", got)
	}
}

func TestAmbiguousArgvStaysFailClosed(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, ".icebath", "state.db")
	installFakeProc(t, tmp, []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: ambiguousArgv})
	got := mustHolders(t, dbPath)
	if !slices.Equal(got, []Holder{{222, "uninspectable holder: icebath gateway run"}}) {
		t.Fatalf("holders = %v", got)
	}
}

func TestArgvReferencingOurDBStaysFlagged(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	for _, argv := range [][]string{
		{"icebath", "--db=" + dbPath, "gateway"},
		{"icebath", "checkpoint", dbPath + "-wal"},
		{"icebath", "--home", tmp, "gateway"},
	} {
		if !looksLikeIcebath(argv) {
			t.Fatalf("argv %v not recognised", argv)
		}
		installFakeProc(t, tmp, []int{222}, nil)
		installFakeArgv(t, map[int][]string{222: argv})
		got := mustHolders(t, dbPath)
		if !slices.Equal(holderPIDs(got), []int{222}) || !strings.HasPrefix(got[0].Target, "uninspectable holder:") {
			t.Fatalf("argv %v holders = %v", argv, got)
		}
	}
}

func TestInstallRootArgvStillHoldsAProfileStoreUnderIt(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	dbPath := filepath.Join(root, "profiles", "b", "state.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: {"icebath", "--home", root, "gateway", "run"}})
	if got := holderPIDs(mustHolders(t, dbPath)); !slices.Equal(got, []int{222}) {
		t.Fatalf("multiplexer holders = %v", got)
	}
	installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: {"icebath", "--home", "/home/demo/.icebath", "gateway", "run"}})
	if got := mustHolders(t, dbPath); len(got) != 0 {
		t.Fatalf("other install holders = %v", got)
	}
}

func TestSharedBinaryPlusAnotherProfileSelectionIsDismissed(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	dbPath := filepath.Join(root, "profiles", "b", "state.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(root, "venv", "bin", "icebath")
	for _, tc := range []struct {
		argv []string
		want []int
	}{
		{[]string{shared, "-p", "other", "chat", "-q"}, []int{}},
		{[]string{shared, "--profile=other", "gateway", "run"}, []int{}},
		{[]string{shared, "-p", "b", "gateway", "run"}, []int{222}},
		{[]string{shared, "gateway", "run"}, []int{222}},
	} {
		installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
		installFakeArgv(t, map[int][]string{222: tc.argv})
		if got := holderPIDs(mustHolders(t, dbPath)); !slices.Equal(got, tc.want) {
			t.Fatalf("argv %v holders = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestTokenNamingAnotherProfilesStoreIsDismissalEvidence(t *testing.T) {
	root := filepath.Join(t.TempDir(), ".icebath")
	dbPath := filepath.Join(root, "profiles", "b", "state.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	otherStore := filepath.Join(root, "profiles", "other", "state.db")
	for _, argv := range [][]string{
		{"icebath", "--db=" + otherStore, "sessions", "optimize"},
		{filepath.Join(root, "venv", "bin", "icebath"), "sessions", otherStore},
	} {
		installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
		installFakeArgv(t, map[int][]string{222: argv})
		if got := mustHolders(t, dbPath); len(got) != 0 {
			t.Fatalf("argv %v holders = %v", argv, got)
		}
	}
}

func TestUnrelatedInstallUnderANonIcebathProfilesTreeStaysDismissed(t *testing.T) {
	work := filepath.Join(t.TempDir(), "work")
	dbPath := filepath.Join(work, "profiles", "b", "state.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(work, "demo", ".icebath")
	if err := os.MkdirAll(unrelated, 0o755); err != nil {
		t.Fatal(err)
	}
	installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: {"icebath", "--icebath-home", unrelated, "gateway", "run"}})
	if got := mustHolders(t, dbPath); len(got) != 0 {
		t.Fatalf("holders = %v", got)
	}
}

func TestCustomHomeIsNotDismissedByTheInstallLocation(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "custom-store", "state.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	installFakeProc(t, filepath.Dir(dbPath), []int{222}, nil)
	installFakeArgv(t, map[int][]string{222: {"/home/u/.icebath/venv/bin/icebath", "gateway", "run"}})
	if got := holderPIDs(mustHolders(t, dbPath)); !slices.Equal(got, []int{222}) {
		t.Fatalf("holders = %v", got)
	}
}

func TestEnvHomeAssignmentSelectsTheHome(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	for _, tc := range []struct {
		argv []string
		want []int
	}{
		{[]string{"env", home.EnvHome + "=/elsewhere", "icebath", "gateway"}, []int{}},
		{[]string{"icebath", home.EnvHome + "=" + tmp, "gateway"}, []int{222}},
	} {
		installFakeProc(t, tmp, []int{222}, nil)
		installFakeArgv(t, map[int][]string{222: tc.argv})
		got := holderPIDs(mustHolders(t, dbPath))
		if tc.argv[0] == "env" {
			if len(got) != 0 {
				t.Fatalf("non icebath argv %v counted: %v", tc.argv, got)
			}
			continue
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("argv %v holders = %v, want %v", tc.argv, got, tc.want)
		}
	}
}

func TestUninspectableDescriptorOfAnIcebathProcessIsAHolder(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	installFakeProc(t, tmp, nil, []int{222, 333})
	installFakeArgv(t, map[int][]string{222: ambiguousArgv})
	got := mustHolders(t, dbPath)
	want := []Holder{{222, "uninspectable descriptor: /proc/222/fd/3: [Errno 13] Permission denied: '/proc/222/fd/3'"}}
	if !slices.Equal(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
}

func TestWatchedFileStatFailureFailsClosed(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	installFakeProc(t, tmp, nil, nil)
	stat := holdersSys.stat
	holdersSys.stat = func(path string) (FileIdentity, error) {
		if path == dbPath+"-wal" {
			return FileIdentity{}, &os.PathError{Op: "stat", Path: path, Err: syscall.EACCES}
		}
		return stat(path)
	}
	got := mustHolders(t, dbPath)
	want := []Holder{{-1, "watched-file stat failed: " + dbPath + "-wal: [Errno 13] Permission denied: '" + dbPath + "-wal'"}}
	if !slices.Equal(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
}

func TestProcListingFailureIsAnUnknownHolder(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	installFakeProc(t, tmp, nil, nil)
	holdersSys.listdir = func(path string) ([]string, error) {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.EIO}
	}
	got := mustHolders(t, dbPath)
	want := []Holder{{-1, "open-file scan failed: [Errno 5] Input/output error: '/proc'"}}
	if !slices.Equal(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
}

func TestDeletedGenerationOnTheSameDeviceIsAHolder(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(tmp, "old.db")
	if err := os.WriteFile(old, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	procRoot := installFakeProc(t, tmp, nil, []int{444})
	fd := filepath.Join(procRoot, "444", "fd", "3")
	if err := os.Remove(fd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(old, fd); err != nil {
		t.Fatal(err)
	}
	readlink := holdersSys.readlink
	holdersSys.readlink = func(path string) (string, error) {
		if path == "/proc/444/fd/3" {
			return dbPath + "-wal (deleted)", nil
		}
		return readlink(path)
	}
	want := []Holder{{444, dbPath + "-wal (deleted)"}}
	if got := mustHolders(t, dbPath); !slices.Equal(got, want) {
		t.Fatalf("holders = %v, want %v", got, want)
	}
}

func TestHeldStoreRefusalNamesHoldersAndIncompleteScans(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "state.db")
	if err := os.WriteFile(dbPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	procRoot := installFakeProc(t, tmp, []int{333}, []int{555})
	fd := filepath.Join(procRoot, "555", "fd", "3")
	if err := os.Remove(fd); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dbPath, fd); err != nil {
		t.Fatal(err)
	}
	installFakeArgv(t, map[int][]string{333: ambiguousArgv, 555: {"/opt/bin/icebath", "gateway", "run"}})
	ctx := home.WithOverride(context.Background(), tmp)
	got, held, err := HeldStoreRefusal(ctx, dbPath, "optimize-storage", "--force")
	if err != nil || !held {
		t.Fatalf("held = %v err = %v", held, err)
	}
	want := strings.Join([]string{
		"Refusing `icebath sessions optimize-storage`: another process is using " + dbPath + ".",
		"  PID 555 (icebath gateway run): state.db",
		"  cannot prove the database is quiet (holder scan incomplete: uninspectable holder: icebath gateway run)",
		"Rewriting the database under a live writer is how every agent ends up refusing turns with the retired state.db-wal error. Nothing is lost.",
		"Stop them first (`icebath gateway stop`, quit the Desktop app, pause cron), then re-run.",
		"Override with --force if you accept the risk.",
		"Recovery guide: " + StorageRecoveryDocsURL,
	}, "\n")
	if got != want {
		t.Fatalf("refusal\n%s\nwant\n%s", got, want)
	}
	root := filepath.Join(tmp, ".icebath")
	profile := filepath.Join(root, "profiles", "work")
	if err := os.MkdirAll(profile, 0o755); err != nil {
		t.Fatal(err)
	}
	got, _, _ = HeldStoreRefusal(home.WithOverride(context.Background(), profile), dbPath, "prune", "")
	if !strings.Contains(got, "`icebath -p work gateway stop`") || strings.Contains(got, "Override with") {
		t.Fatalf("profile refusal\n%s", got)
	}
}

func TestHeldStoreRefusalIsQuietWithoutHolders(t *testing.T) {
	tmp := t.TempDir()
	installFakeProc(t, tmp, nil, nil)
	got, held, err := HeldStoreRefusal(context.Background(), filepath.Join(tmp, "state.db"), "optimize-storage", "--force")
	if err != nil || held || got != "" {
		t.Fatalf("refusal = %q held = %v err = %v", got, held, err)
	}
}
