package manage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Process table seams answering argv per pid in order, one user, no ancestors and no environment
func fakeProcTable(t *testing.T, pids []int, argv map[int][]string) {
	t.Helper()
	oldList, oldParents, oldUser, oldCmd, oldEnv := listPIDs, procParents, procUser, procCmdline, procEnviron
	t.Cleanup(func() {
		listPIDs, procParents, procUser, procCmdline, procEnviron = oldList, oldParents, oldUser, oldCmd, oldEnv
	})
	listPIDs = func() ([]int, error) { return pids, nil }
	procParents = func(int) ([]int, error) { return nil, nil }
	procUser = func(int) (string, error) { return "me", nil }
	procCmdline = func(pid int) ([]string, error) { return argv[pid], nil }
	procEnviron = func(int) (map[string]string, error) { return map[string]string{}, nil }
}

func coderProfileDir(t *testing.T) string {
	t.Helper()
	manageEnv(t)
	captureOut(t)
	if _, err := CreateProfile(context.Background(), "coder", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	dir, err := profiles.GetProfileDir("coder")
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestBackendScanOnlyMatchesThisProfile(t *testing.T) {
	dir := coderProfileDir(t)
	self := os.Getpid()
	fakeProcTable(t, []int{101, 102, 103, self}, map[int][]string{
		101:  {"python", "-m", "icebath_cli.main", "--profile", "coder", "serve"},
		102:  {"python", "-m", "icebath_cli.main", "--profile", "coder", "chat"},
		103:  {"python", "-m", "icebath_cli.main", "--profile", "other", "serve"},
		self: {"python", "-m", "icebath_cli.main", "--profile", "coder", "serve"},
	})
	if got := profileBoundBackendPids("coder", dir); !reflect.DeepEqual(got, []int{101}) {
		t.Fatalf("got %v", got)
	}
}

func TestDeleteProfileBackendScanMatchesShebangExecOfHermesShim(t *testing.T) {
	dir := coderProfileDir(t)
	fakeProcTable(t, []int{201, 202, 203}, map[int][]string{
		201: {"/usr/bin/python3", "/Users/x/.local/bin/icebath", "--profile", "coder", "serve", "--host", "127.0.0.1", "--port", "0"},
		202: {"/usr/bin/python3", "/Users/x/.local/bin/icebath", "--profile", "other", "serve"},
		203: {"/usr/bin/python3", "/Users/x/some_script.py", "--profile", "coder", "serve"},
	})
	if got := profileBoundBackendPids("coder", dir); !reflect.DeepEqual(got, []int{201}) {
		t.Fatalf("got %v", got)
	}
}

func TestDeleteProfileBackendScanRejectsUnrelatedHermesPrefixedScript(t *testing.T) {
	dir := coderProfileDir(t)
	fakeProcTable(t, []int{301, 302}, map[int][]string{
		301: {"/usr/bin/python3", "/Users/x/scripts/icebath-notes.py", "--profile", "coder", "serve"},
		302: {"/usr/bin/python3", "/Users/x/scripts/icebath-unrelated-tool", "--profile", "coder", "serve"},
	})
	if got := profileBoundBackendPids("coder", dir); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestBackendScanMatchesAllKnownConsoleScriptShims(t *testing.T) {
	dir := coderProfileDir(t)
	fakeProcTable(t, []int{401, 402}, map[int][]string{
		401: {"/usr/bin/python3", "/Users/x/.local/bin/icebath-agent", "--profile", "coder", "serve"},
		402: {"/usr/bin/python3", "/Users/x/.local/bin/icebath-acp", "--profile", "coder", "serve"},
	})
	got := profileBoundBackendPids("coder", dir)
	slices.Sort(got)
	if !reflect.DeepEqual(got, []int{401, 402}) {
		t.Fatalf("got %v", got)
	}
}

// Rename seams answering skip for the alias probe
func skipAliasCollision(t *testing.T) {
	t.Helper()
	old := checkAliasCollision
	t.Cleanup(func() { checkAliasCollision = old })
	checkAliasCollision = func(string) (string, error) { return "skip", nil }
}

func TestRenameDelegatesIdentityMigrationToLiveGateway(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	skipAliasCollision(t)
	oldLive, oldNotify, oldVerb, oldAcquire := liveDefaultMultiplexer, notifyMultiplexer, migrateGatewayIdentity, acquireStateDB
	t.Cleanup(func() {
		liveDefaultMultiplexer, notifyMultiplexer, migrateGatewayIdentity, acquireStateDB = oldLive, oldNotify, oldVerb, oldAcquire
	})
	liveDefaultMultiplexer = func() bool { return true }
	notifyMultiplexer = func(string) {}
	var calls [][]string
	migrateGatewayIdentity = func(root, oldName, newName string) (any, error) {
		calls = append(calls, []string{root, oldName, newName})
		return map[string]any{"ok": true, "rekeyed": 1, "db": map[string]any{}}, nil
	}
	acquired := 0
	acquireStateDB = func(context.Context, string) (*state.DB, error) {
		acquired++
		return nil, errors.New("acquire must not run under a live gateway")
	}
	if _, err := RenameProfile(ctx, "oldname", "newname"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !reflect.DeepEqual(calls[0][1:], []string{"oldname", "newname"}) {
		t.Fatalf("verb calls %v", calls)
	}
	if acquired != 0 {
		t.Fatalf("acquire ran %d times", acquired)
	}
}

func TestRenameAccumulatesPreviousNames(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "firstname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	skipAliasCollision(t)
	for _, pair := range [][2]string{{"firstname", "secondname"}, {"secondname", "thirdname"}} {
		if _, err := RenameProfile(ctx, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	infos, err := ListProfiles(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, info := range infos {
		if info.Name == "thirdname" {
			if !reflect.DeepEqual(info.PreviousNames, []string{"firstname", "secondname"}) {
				t.Fatalf("previous names %v", info.PreviousNames)
			}
			return
		}
	}
	t.Fatal("thirdname is not listed")
}

func TestRenameSucceedsWhenPreviousNameWriteFails(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	ctx := context.Background()
	if _, err := CreateProfile(ctx, "oldname", CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	skipAliasCollision(t)
	old := writeProfileMeta
	t.Cleanup(func() { writeProfileMeta = old })
	writeProfileMeta = func(context.Context, string, profiles.MetaUpdate) error { return errors.New("disk full") }
	newDir, err := RenameProfile(ctx, "oldname", "newname")
	if err != nil {
		t.Fatal(err)
	}
	if !isDir(newDir) {
		t.Fatal("renamed dir is missing")
	}
	if filepath.Base(newDir) != "newname" {
		t.Fatal(newDir)
	}
}
