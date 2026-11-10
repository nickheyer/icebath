package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	fleetCheckoutLog = slog.Default().With("logger", "hermes_cli.update_cmd_fleet_checkout")
	fleetProjectRoot = pm.RepoRoot
)

// Git environment that never prompts and never asks a promisor remote for a missing commit
func offlineGitEnv() []string {
	env := tools.NoninteractiveGitEnv(nil)
	env["GIT_NO_LAZY_FETCH"] = "1"
	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out
}

// True when sha is an ancestor of or equal to the code this checkout runs, false on any probe failure
func CheckoutContains(ctx context.Context, sha string) (bool, error) {
	identity, err := fleetCodeIdentity(false)
	if err != nil {
		return false, err
	}
	if identity != nil {
		if source, _ := identity.Get("source"); source == "build-file" {
			stamped := ""
			if v, _ := identity.Get("sha"); pyrepr.Truthy(v) {
				stamped = pyrepr.Str(v)
			}
			return stamped != "" && (stamped == sha || strings.HasPrefix(stamped, sha) || strings.HasPrefix(sha, stamped)), nil
		}
	}
	result, err := RunGit(ctx, []string{"git"}, []string{"merge-base", "--is-ancestor", sha, "HEAD"},
		ChildOptions{Dir: fleetProjectRoot(), Capture: true, Text: true, Timeout: 10 * time.Second, Env: offlineGitEnv()})
	if err != nil {
		fleetCheckoutLog.Debug(fmt.Sprintf("Checkout ancestry probe for %s failed: %s", pystr.Slice(sha, 0, 10), err))
		return false, nil
	}
	return result.ReturnCode == 0, nil
}
