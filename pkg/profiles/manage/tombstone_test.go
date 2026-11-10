package manage

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/logging"
	"github.com/nickheyer/icebath/pkg/profiles"
	"github.com/nickheyer/icebath/pkg/state"
)

// Deletes a profile with its gateway service cleanup and backend stop swapped out
func deleteQuietly(t *testing.T, name string) {
	t.Helper()
	oldClean, oldBackends := cleanupGatewayService, stopProfileBackends
	cleanupGatewayService = func(context.Context, string, string) bool { return false }
	stopProfileBackends = func(string, string) {}
	defer func() { cleanupGatewayService, stopProfileBackends = oldClean, oldBackends }()
	if _, err := DeleteProfile(context.Background(), name, true); err != nil {
		t.Fatal(err)
	}
}

// Names of every listed named profile
func namedHomes(t *testing.T) []string {
	t.Helper()
	infos, err := ListProfiles(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range infos {
		if !i.IsDefault {
			out = append(out, i.Name)
		}
	}
	return out
}

// Creates the worker profile without skills
func createWorker(t *testing.T) string {
	t.Helper()
	dir, err := CreateProfile(context.Background(), "worker", CreateOptions{NoSkills: true})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEmptyShellAfterDeleteIsNotListedOrServed(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	deleteQuietly(t, "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state.db"), "")
	if slices.Contains(namedHomes(t), "worker") || slices.Contains(servedNames(t), "worker") {
		t.Fatal("tombstoned shell listed or served")
	}
	if slices.Contains(profiles.ListProfileNames(), "worker") {
		t.Fatal("tombstoned shell in list_profile_names")
	}
}

func TestTombstonedHomeIsNotBootstrapped(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	deleteQuietly(t, "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ICEBATH_HOME", dir)
	err := config.EnsureIcebathHome(context.Background(), dir)
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "Named profile home does not exist") {
		t.Fatalf("ensure home: %v", err)
	}
	if exists(filepath.Join(dir, "sessions")) {
		t.Fatal("sessions bootstrapped in a tombstoned home")
	}
}

func TestCreateAfterDeleteClearsTombstone(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	createWorker(t)
	deleteQuietly(t, "worker")
	recreated := createWorker(t)
	if st, err := os.Stat(recreated); err != nil || !st.IsDir() || !slices.Contains(namedHomes(t), "worker") {
		t.Fatal("recreated profile missing")
	}
}

func TestDeleteReleasesThisProcessSessionDB(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	defer state.CloseAll()
	ctx := context.Background()
	dir := createWorker(t)
	held, err := state.Acquire(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.SessionIDsWithModelConfigKey("model"); err != nil {
		t.Fatalf("held connection not open: %v", err)
	}
	deleteQuietly(t, "worker")
	if _, err := held.SessionIDsWithModelConfigKey("model"); err == nil {
		t.Fatal("delete left this process's session db connection open")
	}
	recreated := createWorker(t)
	again, err := state.Acquire(ctx, filepath.Join(recreated, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := again.SessionIDsWithModelConfigKey("model"); err != nil {
		t.Fatalf("reacquired connection not open: %v", err)
	}
	state.Release(again)
}

func TestProfileExistsIsFalseForTombstonedShell(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	if !profiles.ProfileExists("worker") {
		t.Fatal("live profile not found")
	}
	deleteQuietly(t, "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if profiles.ProfileExists("worker") {
		t.Fatal("tombstoned shell exists")
	}
	if err := profiles.SetActiveProfile("worker"); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("set active: %v", err)
	}
	if _, err := profiles.ResolveProfileEnv("worker"); !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("resolve env: %v", err)
	}
}

func TestBackfillSkipsTombstonedDirectory(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	deleteQuietly(t, "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmp, ".icebath", ".env"), "OPENROUTER_API_KEY=root-key\n")
	backfilled, err := BackfillProfileEnvs(true)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(backfilled, "worker") || exists(filepath.Join(dir, ".env")) {
		t.Fatalf("backfilled %v", backfilled)
	}
}

func TestMarkerLessShellIsNotAProfile(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	writeFile(t, filepath.Join(tmp, ".icebath", ".env"), "OPENAI_API_KEY=sk-real\n")
	shell := filepath.Join(tmp, ".icebath", "profiles", "ghost")
	writeFile(t, filepath.Join(shell, "cron", "ticker_heartbeat"), "1\n")
	legacy := filepath.Join(tmp, ".icebath", "profiles", "legacy")
	writeFile(t, filepath.Join(legacy, "state.db"), "")
	backfilled, err := BackfillProfileEnvs(true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(backfilled, []string{"legacy"}) || exists(filepath.Join(shell, ".env")) {
		t.Fatalf("backfilled %v", backfilled)
	}
	if got := namedHomes(t); !slices.Equal(got, []string{"legacy"}) {
		t.Fatalf("named %v", got)
	}
	if got := servedNames(t); !slices.Equal(got, []string{"default", "legacy"}) {
		t.Fatalf("served %v", got)
	}
	if profiles.ProfileExists("ghost") {
		t.Fatal("ghost exists")
	}
	if _, err := profiles.ResolveProfileEnv("ghost"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("resolve ghost: %v", err)
	}
	if got, err := profiles.ResolveProfileEnv("legacy"); err != nil || got != legacy {
		t.Fatalf("resolve legacy %q %v", got, err)
	}
	skill := filepath.Join(shell, "skills", "my-skill", "SKILL.md")
	writeFile(t, skill, "# mine\n")
	_, err = CreateProfile(context.Background(), "ghost", CreateOptions{NoSkills: true})
	if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), shell) {
		t.Fatalf("create ghost: %v", err)
	}
	if !exists(skill) || slices.Contains(namedHomes(t), "ghost") {
		t.Fatal("stray dir touched or listed")
	}
}

func TestDanglingSymlinkMarkerIsStillIdentity(t *testing.T) {
	tmp := manageEnv(t)
	legacy := filepath.Join(tmp, ".icebath", "profiles", "legacy")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "gone", "config.yaml"), filepath.Join(legacy, "config.yaml")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if !profiles.ProfileExists("legacy") {
		t.Fatal("dangling marker profile not found")
	}
	if got, err := profiles.ResolveProfileEnv("legacy"); err != nil || got != legacy {
		t.Fatalf("resolve legacy %q %v", got, err)
	}
}

func TestCreateAfterDeleteReplacesEmptyShell(t *testing.T) {
	manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	deleteQuietly(t, "worker")
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	recreated := createWorker(t)
	if st, err := os.Stat(recreated); err != nil || !st.IsDir() || !slices.Contains(namedHomes(t), "worker") {
		t.Fatal("recreated profile missing")
	}
}

func TestCreateAfterDeleteRefusesWhenIdentityFilesRemain(t *testing.T) {
	for _, leftover := range []string{"config.yaml", ".env"} {
		t.Run(leftover, func(t *testing.T) {
			manageEnv(t)
			captureOut(t)
			dir := createWorker(t)
			deleteQuietly(t, "worker")
			path := filepath.Join(dir, leftover)
			writeFile(t, path, "keep-me\n")
			_, err := CreateProfile(context.Background(), "worker", CreateOptions{NoSkills: true})
			if !errors.Is(err, fs.ErrExist) || !strings.Contains(err.Error(), "already exists") {
				t.Fatalf("create: %v", err)
			}
			if readText(t, path) != "keep-me\n" {
				t.Fatal("leftover changed")
			}
		})
	}
}

func TestDeleteThenLoggingSetupDoesNotRecreateHome(t *testing.T) {
	tmp := manageEnv(t)
	captureOut(t)
	dir := createWorker(t)
	deleteQuietly(t, "worker")
	if pathExists(dir) || slices.Contains(namedHomes(t), "worker") {
		t.Fatal("deleted profile still present")
	}
	t.Setenv(home.EnvHome, dir)
	_, err := logging.SetupLogging(context.Background(), logging.Options{Home: dir, Force: true})
	if !errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), "Named profile home does not exist") {
		t.Fatalf("setup logging %v", err)
	}
	if pathExists(dir) {
		t.Fatal("logging setup recreated the deleted home")
	}
	t.Setenv(home.EnvHome, filepath.Join(tmp, ".icebath"))
	if slices.Contains(namedHomes(t), "worker") {
		t.Fatal("deleted profile listed again")
	}
}
