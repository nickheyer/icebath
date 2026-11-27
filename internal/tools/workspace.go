package tools

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

// Whether tools may run the project's own toolchain in the folder: it lies in a folder of the trusted folders setting, or in the git
// worktree of one of the folders the user works in, such as where icebath started and the session's folder, unless that worktree holds
// the home folder. A kanban worker works for the board rather than in a folder of the user's, so only the setting trusts for it
func Trusted(cfg *config.Config, root string, working ...string) bool {
	folders, _ := TrustedFolders(cfg)
	for _, folder := range folders {
		if Inside(root, folder) {
			return true
		}
	}
	worktree := Worktree(root)
	if worktree == "" || os.Getenv(KanbanTaskEnv) != "" {
		return false
	}
	home, _ := os.UserHomeDir()
	if cwd, err := os.Getwd(); err == nil {
		working = append(working, cwd)
	}
	for _, dir := range working {
		if own := Worktree(dir); own == worktree && (home == "" || !Inside(home, own)) {
			return true
		}
	}
	return false
}

// Variable a kanban dispatcher sets for the worker process of a task
const KanbanTaskEnv = "ICEBATH_KANBAN_TASK"

// Folders of the trusted folders setting; false when it holds anything but a list of text, which then trusts nothing
func TrustedFolders(cfg *config.Config) ([]string, bool) {
	return Folders(cfg, toolsconfig.TrustedWorkspaces)
}

// Entries of a setting that lists folders or patterns of them, empty ones left out; false when it holds anything but a list of text
func Folders(cfg *config.Config, key string) ([]string, bool) {
	var items []any
	switch list := cfg.Get(key).(type) {
	case nil:
	case []string:
		for _, folder := range list {
			items = append(items, folder)
		}
	case []any:
		items = list
	default:
		return []string{}, false
	}
	folders := []string{}
	for _, item := range items {
		folder, ok := item.(string)
		if !ok {
			return []string{}, false
		}
		if folder = strings.TrimSpace(folder); folder != "" {
			folders = append(folders, folder)
		}
	}
	return folders, true
}

// Absolute clean form of a path with a leading ~ expanded, links left as they are
func normal(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			path = home + path[1:]
		}
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

// Whether the path is the folder or lies under it, links unresolved
func Inside(path, folder string) bool {
	path, folder = normal(path), normal(folder)
	rel, err := filepath.Rel(folder, path)
	return err == nil && filepath.IsLocal(rel)
}

// Nearest folder at or above the path holding a .git file or folder, empty when there is none
func Worktree(path string) string {
	dir := normal(path)
	if st, err := os.Stat(dir); err == nil && st.Mode().IsRegular() {
		dir = filepath.Dir(dir)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		} else if !os.IsNotExist(err) {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Variable that marks a process running a scheduled job
const CronSessionEnv = "ICEBATH_CRON_SESSION"

// Whether the session runs as a scheduled job, which no one points at a folder: started on the cron surface, or in a process the variable
// marks as one
func Scheduled(platform string) bool {
	if platform == "cron" {
		return true
	}
	return config.Yes(os.Getenv(CronSessionEnv))
}
