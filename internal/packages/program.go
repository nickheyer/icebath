package packages

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/nickheyer/icebath/internal/config"
)

// Program a feature runs: the one of the command's name on the search path, else the package's in the store, else, when asked and the
// profile allows installs on demand, the package installed now; empty when there is none
func (h *Packages) Program(ctx context.Context, cfg *config.Config, name, command string, install bool) string {
	if found, err := exec.LookPath(command); err == nil {
		return found
	}
	if got, err := h.Installed(name, false); err == nil && got != nil && got.Binary != "" {
		return got.Binary
	}
	if !install {
		return ""
	}
	if _, err := h.Ensure(ctx, cfg, name, Ensuring{}); err != nil {
		slog.Warn(fmt.Sprintf("%s auto-install failed: %v", name, err))
		return ""
	}
	if got, err := h.Installed(name, false); err == nil && got != nil {
		return got.Binary
	}
	return ""
}

// Installs a package as a person asked, its entry hashed again and repaired, and answers its program
func (h *Packages) Provide(ctx context.Context, cfg *config.Config, name string) (string, error) {
	if _, err := h.Ensure(ctx, cfg, name, Ensuring{Explicit: true}); err != nil {
		return "", err
	}
	got, err := h.Installed(name, false)
	if err != nil {
		return "", err
	}
	if got == nil || got.Binary == "" {
		return "", fmt.Errorf("%s was installed but its program is not where the package says", name)
	}
	return got.Binary, nil
}
