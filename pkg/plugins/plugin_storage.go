package plugins

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/home"
)

var pluginStorageNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Plugin name the install command accepts, a ValueError for separators, traversal or overlong names
func validateStorageName(name string) (string, error) {
	if !pluginStorageNameRe.MatchString(name) || strings.Contains(name, "..") {
		return "", &HostBuiltinError{Type: "ValueError", Message: fmt.Sprintf("invalid plugin name for storage: %s", errchain.Show(name))}
	}
	return name, nil
}

// Creates and returns the plugin data directory under the active home, never the install tree
func PluginStorageDir(ctx context.Context, name string) (string, error) {
	clean, err := validateStorageName(name)
	if err != nil {
		return "", err
	}
	root := filepath.Join(home.PluginDataDir(home.Dir(ctx)), clean)
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	return root, nil
}
