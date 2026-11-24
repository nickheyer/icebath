//go:build !icebath_core && !icebath_noextras

package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	errorutil "github.com/nickheyer/icebath/pkg/utils/error"
	executil "github.com/nickheyer/icebath/pkg/utils/exec"
)

// Followup a failed configured feature dependency install is owed under
const featureDependenciesFollowup = "feature_dependencies"

// Feature dependency install and memory provider plugin install both update tails run, replaced by tests
var featureRefreshSteps = struct {
	featureDeps     func(ctx context.Context, projectRoot string) error
	memoryProviders func(ctx context.Context, say func(string)) ([]string, error)
}{
	featureDeps:     pm.InstallConfiguredFeaturesMissingDeps,
	memoryProviders: memory.MigrateAllMemoryHomes,
}

// Installs the missing dependencies of configured features, then the plugin of each configured memory provider a profile lacks, only the dependency install failing
func refreshConfiguredFeatures(ctx context.Context, projectRoot string, out io.Writer) error {
	var failure error
	if err := featureRefreshSteps.featureDeps(ctx, projectRoot); err != nil {
		if isInterrupt(err) {
			return err
		}
		text := failureText(err)
		fmt.Fprintf(out, "  ⚠ feature dependencies failed: %s\n", text)
		failure = errors.New("feature dependencies: " + text)
	}
	say := func(message string) { fmt.Fprintln(out, message) }
	if _, err := featureRefreshSteps.memoryProviders(ctx, say); err != nil {
		if isInterrupt(err) {
			return err
		}
		fmt.Fprintf(out, "  ⚠ Could not install the configured memory provider plugins: %v\n", err)
	}
	return failure
}

// Argv and exit status of a failed command, else the exception name and message
func failureText(err error) string {
	var pmCalled *pm.CalledProcessError
	if errors.As(err, &pmCalled) {
		return fmt.Sprintf("%s exited %d", strings.Join(pmCalled.Argv, " "), pmCalled.Code)
	}
	var called *CalledProcessError
	if errors.As(err, &called) {
		return fmt.Sprintf("%s exited %d", strings.Join(called.Cmd, " "), called.ReturnCode)
	}
	var subCalled *executil.StatusError
	if errors.As(err, &subCalled) {
		return fmt.Sprintf("%s exited %d", strings.Join(subCalled.Args, " "), subCalled.ExitCode)
	}
	name := exceptionName(err)
	if text := err.Error(); text != "" {
		return name + ": " + text
	}
	return name
}

// Python exception class an error stands for
func exceptionName(err error) string {
	var named interface{ ErrorKind() string }
	if errors.As(err, &named) {
		return named.ErrorKind()
	}
	var pyErr *pm.PyError
	if errors.As(err, &pyErr) && pyErr.Class != "" {
		return pyErr.Class
	}
	if errorutil.IsSystem(err) {
		return errorutil.Kind(err)
	}
	return "RuntimeError"
}
