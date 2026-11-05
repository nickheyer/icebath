package pm

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Checkout selecting one generation and a venv double counting the extras each build prepares
func csrWorld(t *testing.T) (string, string, *[][]string) {
	t.Helper()
	tmp := svRooted(t)
	svKeepProcess(t)
	ctx := context.Background()
	repo := sttMkdir(t, filepath.Join(tmp, "repo"))
	sttMkdir(t, filepath.Join(repo, "venv"))
	peSwap(t, &repoRoot, func() string { return repo })
	peSwap(t, &processPrefix, func() (string, error) { return filepath.Join(tmp, "foreign-python"), nil })
	selected := svGeneration(t, ctx, repo, "first")
	data, _ := json.Marshal(map[string]any{"schema": LockSchema, "packages": map[string]any{"venv": map[string]any{"environment": selected}}})
	writeFile(t, EnvironmentRuntimeFactsPath(ctx, repo), data, 0o666)
	prepared := &[][]string{}
	pubSwapVenv(t, &pubVenv{
		stamp: func(context.Context, []string, []MemberDir) (string, error) { return "stamp", nil },
		prepare: func(ctx context.Context, extras []string, _ VenvApplyOptions) (*VenvGeneration, error) {
			*prepared = append(*prepared, append([]string{}, extras...))
			environment := filepath.Join(InstallStateDir(ctx, repo), "environments", "next", "venv")
			if err := os.MkdirAll(environment, 0o777); err != nil {
				return nil, err
			}
			return &VenvGeneration{Environment: environment}, os.WriteFile(filepath.Join(environment, "pyvenv.cfg"), []byte("home = fixture\n"), 0o666)
		},
	})
	return tmp, repo, prepared
}

// Fails unless err is the lazy refusal naming the extras and the foreign prefix
func csrWantRefusal(t *testing.T, err error, extras, prefix string) {
	t.Helper()
	var ie *InstallError
	want := "not installed and lazy installs are disabled: " + extras + ": this process is not running from the install's dependency environment (" + prefix +
		"); only an explicit install may change what later processes boot into"
	if !errors.As(err, &ie) || ie.Package != "venv" || !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %v", err)
	}
}

// A lazy extra from a process outside the selected environment refuses before anything is built or selected
func TestClientSyncVenvRefusesLazyExtrasOutsideTheSelectedEnvironment(t *testing.T) {
	tmp, repo, prepared := csrWorld(t)
	ctx := context.Background()
	facts := EnvironmentRuntimeFactsPath(ctx, repo)
	before := peRead(t, facts)

	csrWantRefusal(t, SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock", "voice"}}), "['bedrock', 'voice']", filepath.Join(tmp, "foreign-python"))
	if len(*prepared) != 0 || peRead(t, facts) != before {
		t.Fatalf("refused sync built %v or moved the selection", *prepared)
	}

	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}, Repair: true}); err == nil ||
		!strings.Contains(err.Error(), "repair restores the recorded environment; it cannot change features or plugins") || len(*prepared) != 0 {
		t.Fatalf("repair passed the lazy check into the engine refusal %v %v", err, *prepared)
	}
	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}, Explicit: true}); err != nil {
		t.Fatal(err)
	}
	if len(*prepared) != 1 || !reflect.DeepEqual((*prepared)[0], []string{"bedrock"}) {
		t.Fatalf("explicit and repair syncs prepared %v", *prepared)
	}
}

// A lazy extra extends the environment this process runs from and judges a foreign project root by its own selection
func TestClientSyncVenvLazyExtrasRunFromTheSelectedEnvironment(t *testing.T) {
	tmp, repo, prepared := csrWorld(t)
	ctx := context.Background()
	site, err := SitePackages(resolvePath(filepath.Join(InstallStateDir(ctx, repo), "environments", "first", "venv")))
	if err != nil {
		t.Fatal(err)
	}
	importPath.Lock()
	importPath.entries = append([]string{site}, importPath.entries...)
	importPath.Unlock()
	if err := SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}}); err != nil {
		t.Fatal(err)
	}
	if len(*prepared) != 1 || !reflect.DeepEqual((*prepared)[0], []string{"bedrock"}) {
		t.Fatalf("lazy sync from the selected environment prepared %v", *prepared)
	}
	other := sttMkdir(t, filepath.Join(tmp, "other"))
	csrWantRefusal(t, SyncVenv(ctx, SyncVenvOptions{Extras: []string{"bedrock"}, ProjectRoot: other}), "['bedrock']", filepath.Join(tmp, "foreign-python"))
	if len(*prepared) != 1 {
		t.Fatalf("foreign root refusal prepared %v", *prepared)
	}
}
