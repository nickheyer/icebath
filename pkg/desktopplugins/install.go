package desktopplugins

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/tools"
)

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Windows device names matched with the dotless and dotted i Python folds into the I of CONIN
var windowsDeviceRE = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|CON[Iıİ]N\$|CONOUT\$|CLOCK\$|COM[0-9¹²³]|LPT[0-9¹²³])$`)

// Failed result carrying the error text
func failure(message string) map[string]any {
	return map[string]any{"ok": false, "error": message}
}

// Trimmed name when it can be one plain folder under the root, else an operation error
func checkedName(name string) (string, error) {
	candidate := pystr.Strip(name)
	stem, _, _ := strings.Cut(candidate, ".")
	control := strings.ContainsFunc(candidate, func(r rune) bool { return r < 32 })
	if candidate == "" || strings.HasPrefix(candidate, ".") || strings.HasSuffix(candidate, ".") ||
		strings.ContainsAny(candidate, "/\\:\x00") || control || utf8.RuneCountInString(candidate) > 128 ||
		windowsDeviceRE.MatchString(strings.TrimRight(stem, " ")) {
		return "", &plugins.PluginOperationError{
			Msg: fmt.Sprintf("'%s' is not a desktop plugin folder name: it must be a plain folder name, not a path.", name),
		}
	}
	return candidate, nil
}

// Git url, subdirectory and catalog pin of a source, the catalog repository replacing the identifier when named
func resolveSource(ctx context.Context, identifier, catalogName string) (string, string, string, error) {
	revision := ""
	if catalogName != "" {
		entry := plugins.GetLiveCatalogEntry(ctx, catalogName)
		if entry == nil {
			return "", "", "", &plugins.PluginOperationError{Msg: fmt.Sprintf("'%s' is not in the Icebath plugin catalog.", catalogName)}
		}
		identifier, revision = entry.InstallIdentifier(), entry.Sha
	}
	gitURL, subdir, err := plugins.ResolveGitURL(identifier)
	if err != nil {
		return "", "", "", &plugins.PluginOperationError{Msg: err.Error()}
	}
	candidates := []string{identifier, gitURL}
	if catalogName != "" {
		candidates = append(candidates, catalogName)
	}
	if err := plugins.RaiseIfRemoved(ctx, candidates...); err != nil {
		return "", "", "", err
	}
	return gitURL, subdir, revision, nil
}

// Desktop plugin folder of a clone, refused unless it is a plain tree inside the clone
func desktopTree(pluginRoot string) (string, error) {
	source := DesktopSource(pluginRoot)
	if source == "" {
		source = filepath.Join(pluginRoot, "desktop")
	}
	refused, err := CheckDesktopTree(source, pluginRoot, false, nil)
	if err != nil {
		return "", err
	}
	if refused != "" {
		return "", &plugins.PluginOperationError{Msg: refused}
	}
	return source, nil
}

// Clones the reviewed commit of a source and publishes its desktop half into desktop-plugins on this host
func InstallDesktopPlugin(ctx context.Context, identifier, ref, catalogName string, force bool) (map[string]any, error) {
	sha := strings.ToLower(pystr.Strip(ref))
	result, err := installDesktop(ctx, identifier, sha, catalogName, force)
	if err == nil {
		return result, nil
	}
	var opErr *plugins.PluginOperationError
	if errors.As(err, &opErr) {
		out := failure(opErr.Error())
		var moved *plugins.PluginSourceMoved
		if errors.As(err, &moved) {
			out["source_moved"] = true
		}
		return out, nil
	}
	if pypath.IsOSError(err) {
		return failure("Could not install the desktop plugin: " + errorText(err)), nil
	}
	return nil, err
}

// Install steps whose operation and filesystem errors the caller turns into failed results
func installDesktop(ctx context.Context, identifier, sha, catalogName string, force bool) (result map[string]any, err error) {
	if !commitRE.MatchString(sha) {
		return nil, &plugins.PluginOperationError{Msg: "Installing a desktop plugin needs the commit the install dialog reviewed."}
	}
	gitURL, subdir, catalogSha, err := resolveSource(ctx, identifier, catalogName)
	if err != nil {
		return nil, err
	}
	root, err := Root()
	if err != nil {
		return nil, err
	}
	tmpRoot, err := pypath.Gettempdir()
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(tmpRoot, "icebath-plugin-install-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := tools.RmtreeReadonly(tmp, false); cleanupErr != nil {
			result, err = nil, cleanupErr
		}
	}()
	clone := filepath.Join(tmp, "plugin")
	revision := catalogSha
	if revision == "" {
		revision = sha
	}
	head, err := plugins.ClonePluginRepo(ctx, clone, gitURL, revision, subdir)
	if err != nil {
		return nil, err
	}
	if head != sha {
		return nil, plugins.NewPluginSourceMoved("The catalog's reviewed commit changed since the dialog read it; inspect it again.")
	}
	if err := tools.RmtreeReadonly(filepath.Join(clone, ".git"), false); err != nil {
		return nil, err
	}
	pluginRoot, fallback := clone, ""
	if subdir != "" {
		if pluginRoot, err = plugins.ResolveSubdirWithin(clone, subdir); err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(subdir, "/")
		fallback = trimmed[strings.LastIndex(trimmed, "/")+1:]
	} else {
		fallback = plugins.RepoNameFromURL(gitURL)
	}
	found := plugins.DetectComponents(pluginRoot, fallback)
	if desktop, _ := found["desktop"].(bool); !desktop {
		return failure("This package ships no desktop plugin to install here."), nil
	}
	packageName := ""
	if agent, _ := found["agent"].(bool); agent {
		agentName, _ := found["agent_name"].(string)
		if packageName, err = checkedName(agentName); err != nil {
			return nil, err
		}
	}
	folder := packageName
	if folder == "" {
		if folder, err = checkedName(plugins.DesktopFolderName(gitURL, subdir)); err != nil {
			return nil, err
		}
	}
	sourceDir, err := desktopTree(pluginRoot)
	if err != nil {
		return nil, err
	}
	target := filepath.Join(root, folder)
	var marker *pyjson.Object
	if packageName != "" {
		st, err := os.Stat(filepath.Join(sourceDir, "plugin.js"))
		if err != nil {
			return nil, err
		}
		marker = pyjson.ObjectOf("package", packageName, "repo", gitURL, "source", target, "sourceMtimeMs", MtimeMs(st))
	}
	RootWriter.Lock()
	defer RootWriter.Unlock()
	taken := isLink(target)
	if !taken {
		present, err := pypath.PathlibStat(target)
		if err != nil {
			return nil, err
		}
		taken = present != nil
	}
	if taken && !force {
		return failure(fmt.Sprintf("Desktop plugin '%s' already exists. Enable force reinstall to replace it.", folder)), nil
	}
	if err := PublishDesktopTree(sourceDir, target, marker, nil); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "plugin_name": folder, "path": target, "sha": sha,
		"paired_with_agent": packageName != "", "warnings": []string{}}, nil
}

// Deletes one installed desktop plugin folder, refusing links and a package half unless forced
func RemoveDesktopPlugin(name string, force bool) (map[string]any, error) {
	folder, err := checkedName(name)
	if err != nil {
		return failure(err.Error()), nil
	}
	root, err := Root()
	if err != nil {
		return nil, err
	}
	target := filepath.Join(root, folder)
	if isLink(target) {
		return failure(fmt.Sprintf("'%s' is linked from somewhere else on this host; remove that link where it lives.", folder)), nil
	}
	st, err := pypath.PathlibStat(target)
	if err != nil {
		return nil, err
	}
	if st == nil || !st.IsDir() {
		return failure(fmt.Sprintf("No desktop plugin named '%s' is installed.", folder)), nil
	}
	RootWriter.Lock()
	defer RootWriter.Unlock()
	marker := ReadPackageMarker(target)
	if marker != nil && !force {
		return failure(fmt.Sprintf("'%s' is the desktop half of an agent plugin — uninstall that plugin instead.", folder)), nil
	}
	if err := tools.RmtreeReadonly(target, false); err != nil {
		if !pypath.IsOSError(err) {
			return nil, err
		}
		return failure("Could not remove the desktop plugin: " + errorText(err)), nil
	}
	var unpaired any
	if marker != nil {
		unpaired = marker.GetOr("package", nil)
	}
	return map[string]any{"ok": true, "plugin_name": folder, "path": target, "unpaired_from": unpaired}, nil
}
