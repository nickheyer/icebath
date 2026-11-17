package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pysubprocess"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Followup a failed configured feature dependency install is owed under
const featureDependenciesFollowup = "feature_dependencies"

// Feature dependency install and home migrations both update tails run, replaced by tests
var featureRefreshSteps = struct {
	featureDeps     func(ctx context.Context, projectRoot string) error
	memoryMigrate   func(ctx context.Context, say func(string)) ([]string, error)
	leftCoreMigrate func(ctx context.Context, say func(string)) ([]string, error)
}{
	featureDeps:     pm.InstallConfiguredFeaturesMissingDeps,
	memoryMigrate:   memory.MigrateAllMemoryHomes,
	leftCoreMigrate: provider.MigrateAllLeftCoreHomes,
}

// Installs missing configured feature dependencies then migrates memory providers and plugins that left core, only the install failing
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
	if _, err := featureRefreshSteps.memoryMigrate(ctx, say); err != nil {
		if isInterrupt(err) {
			return err
		}
		fmt.Fprintf(out, "  ⚠ Memory provider migration skipped: %v\n", err)
	}
	if _, err := featureRefreshSteps.leftCoreMigrate(ctx, say); err != nil {
		if isInterrupt(err) {
			return err
		}
		fmt.Fprintf(out, "  ⚠ Plugin migration skipped: %v\n", err)
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
	var subCalled *pysubprocess.CalledProcessError
	if errors.As(err, &subCalled) {
		return fmt.Sprintf("%s exited %d", strings.Join(subCalled.Cmd, " "), subCalled.Returncode)
	}
	name := exceptionName(err)
	if text := err.Error(); text != "" {
		return name + ": " + text
	}
	return name
}

// Python exception class an error stands for
func exceptionName(err error) string {
	var named interface{ PyTypeName() string }
	if errors.As(err, &named) {
		return named.PyTypeName()
	}
	var pyErr *pm.PyError
	if errors.As(err, &pyErr) && pyErr.Class != "" {
		return pyErr.Class
	}
	if pypath.IsOSError(err) {
		return pypath.OSErrorClass(err)
	}
	return "RuntimeError"
}
