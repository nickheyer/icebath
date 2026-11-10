package observability

import (
	"context"
	"os"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Dispatches the scratch prune entry and installs empty gateway and profile secret sources the terminal tool reads
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry()
	tools.ConfigEnvTableKeys = func() []string { return nil }
	tools.RegisteredAdapterSecretEnv = func(*tools.Env) map[string]bool { return map[string]bool{} }
	tools.ProfileSecrets = emptyProfileSecrets{}
	os.Exit(m.Run())
}

// Profile secret source with no managed keys and no secret scope
type emptyProfileSecrets struct{}

func (emptyProfileSecrets) ManagedDotenvKeys() []string   { return nil }
func (emptyProfileSecrets) SourceSuppliedNames() []string { return nil }
func (emptyProfileSecrets) BuildProfileSecretScope(string) map[string]string {
	return map[string]string{}
}
func (emptyProfileSecrets) CurrentSecretScope(context.Context) map[string]string {
	return map[string]string{}
}
