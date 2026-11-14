package env

import (
	"context"
	"errors"
)

var ErrNoShell = errors.New("No shell found. icebath needs bash (Git for Windows on Windows). " +
	"Run `icebath pm install` or reinstall the bundle.")

var spawnCompatibleShells = map[string]bool{"bash": true, "zsh": true, "sh": true, "dash": true, "ksh": true, "mksh": true}

func (h *Host) FindBash(ctx context.Context) (string, error) {
	bash := findBash(ctx, h.toolchain)
	if bash == "" {
		return "", ErrNoShell
	}
	return bash, nil
}
