package codeexec

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune entry so a prune child never reruns this suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry(context.Background())
	kernelHostEntry()
	kernelOwnerEntry()
	os.Exit(m.Run())
}

// Tool env over a temp home with an empty passthrough allowlist restored after the test
func testEnv(t *testing.T) *tools.Env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("ICEBATH_HOME", dir)
	tools.ClearEnvPassthrough()
	tools.ResetEnvPassthroughConfigCache()
	bundled, table, adapters := tools.BundledPlatformSecretEnvs, tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv
	tools.BundledPlatformSecretEnvs = func() []string { return nil }
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	t.Cleanup(func() {
		tools.ClearEnvPassthrough()
		tools.ResetEnvPassthroughConfigCache()
		tools.BundledPlatformSecretEnvs, tools.ConfigEnvTableKeys, tools.RegisteredAdapterSecretEnv = bundled, table, adapters
	})
	return &tools.Env{Home: dir, Config: config.Tree{}, Values: &sync.Map{}, Cwd: dir}
}

// Host python3 standing in for the managed runtime interpreter
func hostRuntime(t *testing.T) interpreter {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required: %v", err)
	}
	out, err := exec.Command(python, "-c", "import sys; print(sys.prefix)").Output()
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := pypath.Realpath(strings.TrimSpace(string(out)), pypath.Lax)
	if err != nil {
		t.Fatal(err)
	}
	rt := interpreter{Python: python, Prefix: prefix}
	prev := runtimeInterpreter
	runtimeInterpreter = func(context.Context, *tools.Env) (interpreter, error) { return rt, nil }
	t.Cleanup(func() { runtimeInterpreter = prev })
	return rt
}

// Fixed configured timezone for the duration of a test
func withTimezone(t *testing.T, name string) {
	t.Helper()
	prev := timezoneName
	timezoneName = func(context.Context) string { return name }
	t.Cleanup(func() { timezoneName = prev })
}

// Real bash transport standing in for a remote terminal backend
type shellEnv struct {
	root string
	bash string
	env  []string
}

func newShellEnv(t *testing.T, root string) *shellEnv {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash is required: %v", err)
	}
	return &shellEnv{root: root, bash: bash, env: os.Environ()}
}

func (s *shellEnv) Execute(ctx context.Context, command string, opts tools.ExecOptions) (*tools.ExecResult, error) {
	cmd := exec.CommandContext(ctx, s.bash, "-c", command)
	cmd.Dir = s.root
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = s.env
	if opts.Stdin != nil {
		cmd.Stdin = strings.NewReader(*opts.Stdin)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			return nil, err
		}
		code = ee.ExitCode()
	}
	return &tools.ExecResult{Output: string(out), ReturnCode: code}, nil
}

func (s *shellEnv) Cleanup(bool) error       { return nil }
func (s *shellEnv) EnvType() string          { return "local" }
func (s *shellEnv) Cwd() string              { return s.root }
func (s *shellEnv) SetCwd(string)            {}
func (s *shellEnv) TempDir() string          { return s.root }
func (s *shellEnv) HostCwd() string          { return s.root }
func (s *shellEnv) HostCwdMount() string     { return "" }
func (s *shellEnv) SandboxHome() string      { return "" }
func (s *shellEnv) IsPersistent() bool       { return false }
func (s *shellEnv) IsSessionScoped() bool    { return false }
func (s *shellEnv) UsesHostFilesystem() bool { return true }
func (s *shellEnv) FetchFile(ctx context.Context, remotePath, localDest string, maxBytes int) error {
	return nil
}
func (s *shellEnv) FetchRealpath(ctx context.Context, remotePath string) string {
	return filepath.Clean(remotePath)
}
