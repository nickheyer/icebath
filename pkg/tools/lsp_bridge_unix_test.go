//go:build !windows

package tools

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/lsp"
)

func TestLSPOwnerHelperProcess(t *testing.T) {
	pidFile := os.Getenv("ICEBATH_LSP_OWNER_CHILD_PID")
	if pidFile == "" {
		t.Skip("runs only as the owner process the supervisor tests start")
	}
	installTestEnvHooks(t)
	ws, mock, h := os.Getenv("ICEBATH_LSP_OWNER_WS"), os.Getenv("ICEBATH_LSP_OWNER_MOCK"), os.Getenv("ICEBATH_HOME")
	idx := slices.IndexFunc(lsp.Servers, func(s *lsp.ServerDef) bool { return s.ServerID == "pyright" })
	lsp.Servers[idx] = &lsp.ServerDef{ServerID: "pyright", Extensions: lsp.Servers[idx].Extensions,
		ResolveRoot: func(_, ws string) string { return ws },
		BuildSpawn: func(root string, _ lsp.ServerContext) (*lsp.SpawnSpec, error) {
			return &lsp.SpawnSpec{Command: []string{mock}, WorkspaceRoot: root, Cwd: root,
				Env: map[string]string{"MOCK_LSP_SCRIPT": "slow_tree", "MOCK_LSP_CHILD_PID": pidFile}}, nil
		}}
	env := &Env{Home: h, Secrets: config.NewSecrets(h), Values: &sync.Map{}, Config: config.Tree{"lsp": map[string]any{
		"wait_timeout": 1.0, "install_strategy": "manual", "idle_timeout": 0}}}
	svc, err := LSPServiceFor(env)
	if err != nil || svc == nil {
		t.Fatal(svc, err)
	}
	svc.SnapshotBaseline(filepath.Join(ws, "x.py"), map[string]bool{ws: true})
	os.Stdout.WriteString("ready\n")
	time.Sleep(300 * time.Second)
}
