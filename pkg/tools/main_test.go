package tools

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/hermetic"
	"github.com/nickheyer/icebath/pkg/state"
)

// Credential fragments of codeexec SecretSubstrings, the list the process wiring installs into pkg/state
var testSecretEnvSubstrings = []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN",
	"WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}

// Answers like codeexec IsSecretEnvName, a credential fragment anywhere in the name in any letter case
func testSecretEnvName(name string) bool {
	upper := strings.ToUpper(name)
	return slices.ContainsFunc(testSecretEnvSubstrings, func(s string) bool { return strings.Contains(upper, s) })
}

// Dispatches the child modes, installs the secret env doubles, then runs the suite
func TestMain(m *testing.M) {
	hermetic.BlockExternalHTTP()
	home.DispatchScratchPruneEntry()
	asyncDelegationChildEntry()
	DispatchDDGSWorkerEntry()
	DispatchCommitGraphRefreshEntry(context.Background())
	ptyTestHelperEntry()
	state.SecretEnvSubstrings = testSecretEnvSubstrings
	state.SecretEnvName = testSecretEnvName
	installContextLengthDoubles()
	installDelegationRegistryDoubles()
	installGitlockDoubles()
	installWorktreeDoubles()
	installProfileStoreDoubles()
	os.Exit(m.Run())
}

// The secret env doubles hold the codeexec fragment list and its case folded substring rule
func TestSecretEnvDoublesAnswerLikeCodeexec(t *testing.T) {
	want := []string{"KEY", "TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "PASSWD", "AUTH", "DSN", "WEBHOOK", "CREDS", "BEARER", "APIKEY", "COOKIE"}
	if !slices.Equal(state.SecretEnvSubstrings, want) {
		t.Fatalf("fragments %q", state.SecretEnvSubstrings)
	}
	for name, secret := range map[string]bool{"my_api_key": true, "GH_TOKEN": true, "Session_Cookie": true, "DATABASE_DSN": true, "PATH": false, "NVM_DIR": false, "MY_TOOLCHAIN_HOME": false} {
		if got := state.SecretEnvName(name); got != secret {
			t.Fatalf("SecretEnvName(%q) = %v", name, got)
		}
	}
}
