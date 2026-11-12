package plugins

import (
	"context"

	"github.com/nickheyer/icebath/pkg/home"
)

// Clones a plugin source into tmpClone at revision under the context home and returns its HEAD commit
func ClonePluginRepo(ctx context.Context, tmpClone, gitURL, revision, subdir string) (string, error) {
	return clonePluginRepo(ctx, home.Dir(ctx), tmpClone, gitURL, revision, subdir)
}

// Git url and subdirectory of an install identifier, an error for one that is neither url nor shorthand
func ResolveGitURL(identifier string) (string, string, error) { return resolveGitURL(identifier) }

// Subdirectory resolved inside a clone, an operation error when it escapes, is missing or is a file
func ResolveSubdirWithin(cloneRoot, subdir string) (string, error) {
	return resolveSubdirWithin(cloneRoot, subdir)
}

// Repository name of a git url, the last path part with an ssh host prefix dropped
func RepoNameFromURL(url string) string { return repoNameFromURL(url) }

// Agent and desktop halves a source tree ships with the agent half name
func DetectComponents(root, fallbackName string) map[string]any {
	return detectComponents(root, fallbackName)
}

// Desktop folder name of a source, the last real subdirectory segment else the repository name
func DesktopFolderName(gitURL, subdir string) string { return desktopFolderName(gitURL, subdir) }

// Operation error telling the client to read the source again because it moved since review
func NewPluginSourceMoved(msg string) *PluginSourceMoved { return newPluginSourceMoved(msg) }
