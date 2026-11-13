package selfupdate

import (
	"context"
	"fmt"
	"io"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/memory"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
)

// Installs the configured features' missing dependencies, then migrates the memory and plugin layout of every home
func PrepareUpdatedInstall(ctx context.Context, w io.Writer, projectRoot string) error {
	PublishStage("Updating feature dependencies")
	err := pm.InstallConfiguredFeaturesMissingDeps(ctx, projectRoot)
	if err != nil {
		fmt.Fprintf(w, "  ⚠ feature dependencies failed: %s\n", launchErrorText(err))
		err = fmt.Errorf("feature dependencies: %s", launchErrorText(err))
	}
	say := func(message string) { fmt.Fprintln(w, message) }
	if _, migrateErr := memory.MigrateAllMemoryHomes(ctx, say); migrateErr != nil {
		fmt.Fprintf(w, "  ⚠ Memory provider migration skipped: %v\n", migrateErr)
	}
	if _, migrateErr := provider.MigrateAllLeftCoreHomes(ctx, say); migrateErr != nil {
		fmt.Fprintf(w, "  ⚠ Plugin migration skipped: %v\n", migrateErr)
	}
	return err
}

// Reports a desktop app this machine already runs, installed under the home or built in the checkout
func desktopAppPresent(root string) bool {
	return install.DesktopProductPresent(install.InstallRootOf(install.Home())) || install.CheckoutDesktopExe(root) != ""
}
