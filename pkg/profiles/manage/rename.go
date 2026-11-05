package manage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/profiles"
)

// Appends the old slug to the renamed profile's previous names history, logging instead of failing
func recordProfileRename(newDir, oldCanon string) {
	history := append([]string{}, profiles.ReadProfileMeta(newDir).PreviousNames...)
	if !slices.Contains(history, oldCanon) {
		history = append(history, oldCanon)
	}
	if err := profiles.WriteProfileMeta(newDir, profiles.MetaUpdate{PreviousNames: &history}); err != nil {
		slog.Default().Debug(fmt.Sprintf("profile rename: could not record previous name %s in %s: %v", pyrepr.Repr(oldCanon), newDir, err))
	}
}

// Refuses a rename before any side effect when the config backend keys settings by the profile name
func refuseRenameWithoutFileTooling(ctx context.Context, oldCanon string) error {
	backend, err := config.GetConfigBackend(ctx)
	if err != nil {
		return err
	}
	if !backend.SupportsFileTooling() {
		return &profiles.ValueError{Msg: fmt.Sprintf("Cannot rename profile '%s': its config lives in the %s config "+
			"backend under that name, and renaming it there is not supported. Nothing was changed.", oldCanon, pyrepr.Repr(backend.Name()))}
	}
	return nil
}

// Renames a profile directory with its wrapper, service, identity and active pointer, or sets the default's display name
func RenameProfile(ctx context.Context, oldName, newName string) (string, error) {
	oldCanon, err := profiles.CanonValid(oldName)
	if err != nil {
		return "", err
	}
	if oldCanon == "default" {
		cleaned, err := profiles.SetProfileDisplayName("default", newName)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(stdout, "✓ Display name set: %s (canonical id remains 'default')\n", cleaned)
		return profiles.DefaultHome(), nil
	}
	newCanon, err := profiles.CanonValid(newName)
	if err != nil {
		return "", err
	}
	if newCanon == "default" {
		return "", &profiles.ValueError{Msg: "Cannot rename to 'default' — it is reserved."}
	}
	if err := refuseRenameWithoutFileTooling(ctx, oldCanon); err != nil {
		return "", err
	}
	oldDir, err := profiles.GetProfileDir(oldCanon)
	if err != nil {
		return "", err
	}
	newDir, err := profiles.GetProfileDir(newCanon)
	if err != nil {
		return "", err
	}
	if !isDir(oldDir) {
		return "", profiles.UnknownProfileError(oldCanon)
	}
	if pathExists(newDir) {
		return "", profiles.ProfileExistsError(newCanon)
	}
	gwRunning := checkGatewayRunning(oldDir)
	serviceRemoved := cleanupGatewayService(ctx, oldCanon, oldDir)
	maybeUnregisterGatewayService(oldCanon)
	if gwRunning {
		stopGatewayProcess(oldDir)
	}
	stopBotDesktop(ctx, oldDir)
	liveMux := liveDefaultMultiplexer()
	if liveMux {
		if err := home.MarkProfileDeleted(oldDir); err != nil {
			return "", err
		}
		notifyMultiplexer(oldCanon)
	}
	key := home.Key(oldDir)
	mcp.ShutdownMCPServers(ctx, &key, nil, 15*time.Second)
	if err := moveProfileDir(liveMux, oldDir, newDir); err != nil {
		if liveMux {
			home.ClearProfileDeleted(oldDir)
			notifyMultiplexer(oldCanon)
		}
		maybeRegisterGatewayService(oldCanon)
		if serviceRemoved {
			fmt.Fprintf(stdout, "⚠ The gateway service was removed. Reinstall it with: icebath -p %s gateway install\n", oldCanon)
		}
		return "", err
	}
	fmt.Fprintf(stdout, "✓ Renamed %s → %s\n", filepath.Base(oldDir), filepath.Base(newDir))
	if liveMux {
		if err := home.ClearProfileDeleted(oldDir); err != nil {
			return "", err
		}
	}
	recordProfileRename(newDir, oldCanon)
	if err := migrateHonchoProfileHost(oldCanon, newCanon, newDir); err != nil {
		return "", err
	}
	if _, err := profiles.RemoveWrapperScript(oldCanon); err != nil {
		return "", err
	}
	collision, err := profiles.CheckAliasCollision(newCanon)
	if err != nil {
		return "", err
	}
	if collision == "" {
		if _, err := profiles.CreateWrapperScript(newCanon, ""); err != nil {
			return "", err
		}
		fmt.Fprintf(stdout, "✓ Alias updated: %s\n", newCanon)
	} else {
		fmt.Fprintf(stdout, "⚠ Cannot create alias '%s' — %s\n", newCanon, collision)
	}
	profiles.RetargetActiveProfile(oldCanon, newCanon, "✓ Active profile updated: "+newCanon)
	migrateProfileIdentity(ctx, oldCanon, newCanon, liveMux)
	if liveMux {
		notifyMultiplexer(newCanon)
	}
	maybeRegisterGatewayService(newCanon)
	if serviceRemoved {
		fmt.Fprintf(stdout, "⚠ The gateway service was removed. Reinstall it with: icebath -p %s gateway install\n", newCanon)
	}
	return newDir, nil
}

// Moves the profile dir after a live multiplexer released it, the rename step whose failure undoes the unroute
func moveProfileDir(liveMux bool, oldDir, newDir string) error {
	if liveMux {
		unowned, err := WaitProfileUnowned(oldDir, nil)
		if err != nil {
			return err
		}
		if !unowned {
			return &RuntimeError{Msg: "a running gateway still owns this profile; retry the rename once it settles"}
		}
	}
	return os.Rename(oldDir, newDir)
}
