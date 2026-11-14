package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/hostinfo"
	"github.com/nickheyer/icebath/pkg/internal/pyfs"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyrich"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const CatalogSidecar = ".icebath-catalog.json"

const (
	LoginBackendRule = "password-manager login backends are accepted only from plugins bundled with Icebath or " +
		"installed from the Icebath plugin catalog at tier verified or official (published by the " +
		"password-manager vendor itself or by Nous)"
	BrowserPageBackendRule = "browser page backends (which carry browser_vault fill scripts) are accepted only from " +
		"plugins bundled with Icebath or installed from the Icebath plugin catalog at tier " +
		"verified or official"
)

var LoginBackendTiers = map[string]bool{"official": true, "verified": true}

var nonCatalogSources = map[string]string{"project": "a project .icebath/plugins directory", "entrypoint": "a pip entry point"}

var handoffRepos = map[string]string{"github.com/nousresearch/hermes-plugin-holographic": "holographic"}

var platformAliases = map[string]string{"windows": "win32", "macos": "darwin"}

var noGitRevisionFiles = map[string]bool{"plugin.yaml": true, "plugin.yml": true, "plugin.json": true, "mcp.json": true,
	"pyproject.toml": true, "package.json": true, "package-lock.json": true, "uv.lock": true}

var noGitRevisionDirs = map[string]bool{"dashboard": true, "desktop": true, "skills": true, "sidecar": true, "node_modules": true}

var noGitRevisionExtensions = map[string]bool{".mjs": true, ".cjs": true, ".jsx": true, ".tsx": true}

var surfaceKeys = []string{"capabilities", "tools", "hooks", "python_dependencies", "desktop", "requires_auth", "oauth"}

var surfaceLabels = map[string]string{"capabilities": "host capabilities", "tools": "tools", "hooks": "hooks",
	"python_dependencies": "Python dependencies", "desktop": "Desktop UI half",
	"requires_auth": "your sign-in for", "oauth": "your OAuth sign-in for"}

var presenceSurfaces = map[string]bool{"desktop": true}

// Context whose home is h, for helpers that resolve the live catalog or lock metadata of that home
func homeContext(h string) context.Context { return home.WithOverride(context.Background(), h) }

// Bare lowercase catalog token rather than a url, owner/repo shorthand or path
func LooksLikeCatalogName(identifier string) bool {
	return identifier != "" && !strings.ContainsAny(identifier, `/\`) && !hasURLScheme(identifier) && catalogNameRe.MatchString(identifier)
}

// Operation error when any candidate name or repo is on the kill list
func raiseIfRemoved(ctx context.Context, candidates ...string) error {
	for _, candidate := range candidates {
		removed := FindRemoved(ctx, candidate, "")
		if removed == nil {
			continue
		}
		detail := removed.Reason
		if detail == "" {
			detail = "no reason recorded"
		}
		if removed.Date != "" {
			detail += fmt.Sprintf(" (removed %s)", removed.Date)
		}
		return &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' was removed from the Icebath plugin catalog and is blocked from "+
			"installation: %s", removed.Name, detail), FailureClass: "removed_from_catalog"}
	}
	return nil
}

// Live catalog entry of a bare name, or exit 1 pointing at search
func resolveCatalogName(ctx context.Context, identifier string, console *Console) (*PluginCatalogEntry, error) {
	entry := GetLiveCatalogEntry(ctx, identifier)
	if entry == nil {
		return nil, fail(console, fmt.Sprintf("[red]Error:[/red] '%s' is not in the Icebath plugin catalog and is not a Git URL or "+
			"owner/repo shorthand. Browse entries with `icebath plugins search`.", identifier))
	}
	return entry, nil
}

// Writes the convenience catalog sidecar copy inside the install dir, logging a write failure
func writeCatalogSidecarRecord(target string, catalog map[string]any, sha string) {
	tier := orEmpty(catalog["tier"])
	if tier == "" {
		tier = "community"
	}
	doc := pyjson.NewObject()
	doc.Set("catalog_name", catalog["name"])
	doc.Set("repo", catalog["repo"])
	doc.Set("sha", sha)
	doc.Set("tier", tier)
	doc.Set("installed_at", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	text, err := pyjson.Dumps(doc, pyjson.Indent(2))
	if err == nil {
		err = os.WriteFile(filepath.Join(target, CatalogSidecar), []byte(text+"\n"), 0o666)
	}
	if err != nil {
		logWarn(fmt.Sprintf("Failed to write catalog sidecar in %s: %s", target, pyfs.OSErrorText(err)))
	}
}

func writeCatalogSidecar(target string, entry *PluginCatalogEntry, sha string) {
	if sha == "" {
		sha = entry.Sha
	}
	writeCatalogSidecarRecord(target, map[string]any{"name": entry.Name, "repo": entry.Repo, "tier": entry.Tier}, sha)
}

// Moves one trusted installer record onto the nested catalog block contract
func writeCatalogBlock(ctx context.Context, h, pluginDir string, block map[string]any) (map[string]any, error) {
	err := updateInstallRecord(ctx, h, filepath.Base(pluginDir), func(current map[string]any) map[string]any {
		if current == nil {
			return nil
		}
		migrated := make(map[string]any, len(current)+1)
		for k, v := range current {
			migrated[k] = v
		}
		migrated["catalog"] = block
		delete(migrated, "catalog_name")
		delete(migrated, "catalog_tier")
		return migrated
	})
	return block, err
}

// Adopts a pre provenance in tree sidecar once, only when the installer record agrees with it
func adoptLegacySidecar(ctx context.Context, h, pluginDir string, record map[string]any) (map[string]any, error) {
	path := filepath.Join(pluginDir, CatalogSidecar)
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(stripBOM(raw)) {
		return nil, nil
	}
	value, err := pyjson.Loads(string(stripBOM(raw)))
	if err != nil {
		return nil, nil
	}
	data, ok := pyjson.Plain(value).(map[string]any)
	if !ok || !jsonTruthy(data["catalog_name"]) {
		return nil, nil
	}
	sha := strings.ToLower(pyStrOr(data["sha"]))
	entry := GetLiveCatalogEntry(ctx, pyrepr.Str(data["catalog_name"]))
	if pinned, _ := record["pinned"].(bool); entry == nil || !pinned || record["revision"] != sha {
		return nil, nil
	}
	source, _, _ := strings.Cut(pyStrOr(record["source"]), "#")
	if normalizeRepo(source) != normalizeRepo(entry.Repo) {
		return nil, nil
	}
	tier := pyStrOr(data["tier"])
	if tier == "" {
		tier = entry.Tier
	}
	return writeCatalogBlock(ctx, h, pluginDir, map[string]any{"name": entry.Name, "repo": entry.Repo, "tier": tier, "pin": sha, "sha": sha})
}

// Adopts a url install of an archived handoff repo as the catalog entry that superseded it
func adoptHandoffInstall(ctx context.Context, h, pluginDir string, record map[string]any) (map[string]any, error) {
	source, _, _ := strings.Cut(pyStrOr(record["source"]), "#")
	entry := GetLiveCatalogEntry(ctx, handoffRepos[normalizeRepo(source)])
	if entry == nil {
		return nil, nil
	}
	sha := strings.ToLower(pyStrOr(record["revision"]))
	return writeCatalogBlock(ctx, h, pluginDir, map[string]any{"name": entry.Name, "repo": source, "tier": entry.Tier, "pin": sha, "sha": sha})
}

// Text of a JSON value the way str(value or "") reads it
func pyStrOr(v any) string {
	if !jsonTruthy(v) {
		return ""
	}
	return pyrepr.Str(v)
}

// Catalog provenance of an installed plugin from the installer owned record, nil for a non catalog install
func readCatalogSidecar(h, pluginDir string) (map[string]any, error) {
	if pluginDir == "" {
		return nil, nil
	}
	ctx := homeContext(h)
	record := installRecord(h, pluginDir)
	if record == nil {
		return nil, nil
	}
	block, ok := record["catalog"].(map[string]any)
	if !ok {
		var err error
		if legacy := record["catalog_name"]; jsonTruthy(legacy) {
			sha := strings.ToLower(pyStrOr(record["revision"]))
			repo, _, _ := strings.Cut(pyStrOr(record["source"]), "#")
			tier := pyStrOr(record["catalog_tier"])
			if tier == "" {
				tier = "community"
			}
			block, err = writeCatalogBlock(ctx, h, pluginDir, map[string]any{"name": pyrepr.Str(legacy), "repo": repo, "tier": tier, "pin": sha, "sha": sha})
		} else if block, err = adoptLegacySidecar(ctx, h, pluginDir, record); err == nil && block == nil {
			block, err = adoptHandoffInstall(ctx, h, pluginDir, record)
		}
		if err != nil {
			return nil, err
		}
	}
	if len(block) == 0 || !jsonTruthy(block["name"]) {
		return nil, nil
	}
	tier := block["tier"]
	if !jsonTruthy(tier) {
		tier = "community"
	}
	return map[string]any{"catalog_name": block["name"], "repo": valueOr(block, "repo", ""), "sha": valueOr(block, "sha", ""),
		"tier": tier, "pin": valueOr(block, "pin", "")}, nil
}

func valueOr(m map[string]any, key string, def any) any {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// Install satisfies the catalog pin when HEAD is that commit or the pin is the verified tag object
func atCatalogPin(sidecar map[string]any, entrySha string) bool {
	return entrySha != "" && (entrySha == strings.ToLower(pyStrOr(sidecar["sha"])) || entrySha == strings.ToLower(pyStrOr(sidecar["pin"])))
}

func catalogInstallRecord(h, pluginDir string) (map[string]any, error) {
	return readCatalogSidecar(h, pluginDir)
}

// Why a plugin may not register a browser page backend, empty when it may
func browserPageBackendProvenanceError(h string, m *Manifest) string {
	return loginBackendProvenanceErrorRule(h, m, BrowserPageBackendRule)
}

// Why a plugin may not register a login backend, empty when it may
func loginBackendProvenanceError(h string, m *Manifest) string {
	return loginBackendProvenanceErrorRule(h, m, LoginBackendRule)
}

// Provenance verdict from the installer record of the current home, never from the tree
func loginBackendProvenanceErrorRule(h string, m *Manifest, rule string) string {
	if m.Source == "bundled" {
		return ""
	}
	if where, ok := nonCatalogSources[m.Source]; ok {
		return fmt.Sprintf("%s; this plugin was loaded from %s", rule, where)
	}
	var record map[string]any
	if m.Source == "user" && m.Path != "" {
		record = installRecord(h, m.Path)
	}
	block, ok := record["catalog"].(map[string]any)
	if !ok || !jsonTruthy(block["name"]) {
		return rule + "; this plugin was not installed from the Icebath plugin catalog (a local directory or Git URL install)"
	}
	if jsonTruthy(record["allow_removed"]) {
		return rule + "; this plugin was installed with --allow-removed"
	}
	tier := pyStrOr(block["tier"])
	if tier == "" {
		tier = "community"
	}
	if !LoginBackendTiers[tier] {
		return fmt.Sprintf("%s; catalog entry '%s' is tier %s", rule, pyrepr.Str(block["name"]), tier)
	}
	if !jsonTruthy(block["pin"]) {
		return rule + "; this install is not at the catalog's reviewed pin (installed with --ref)"
	}
	return ""
}

// Source column annotation catalog tier at short sha of a catalog install, empty otherwise
func catalogAnnotation(h, dir string) (string, error) {
	sidecar, err := catalogInstallRecord(h, dir)
	if sidecar == nil {
		return "", err
	}
	tier := pyStrOr(sidecar["tier"])
	if tier == "" {
		tier = "community"
	}
	sha := pyStrOr(sidecar["sha"])
	return fmt.Sprintf("catalog:%s@%s", tier, sha[:min(8, len(sha))]), nil
}

// Kill list reason of an installed plugin matched by name, catalog name or repo, empty when not removed
func removedAnnotation(h, name, dir string, removedEntries []RemovedEntry) (string, bool, error) {
	sidecar, err := catalogInstallRecord(h, dir)
	if err != nil {
		return "", false, err
	}
	for _, candidate := range []any{name, sidecar["catalog_name"], sidecar["repo"]} {
		if !jsonTruthy(candidate) {
			continue
		}
		if removed := MatchRemoved(pyrepr.Str(candidate), removedEntries); removed != nil {
			if removed.Reason == "" {
				return "no reason recorded", true, nil
			}
			return removed.Reason, true, nil
		}
	}
	return "", false, nil
}

// Catalog platform names in host os family vocabulary
func normalizedPlatforms(platforms []string) map[string]bool {
	out := map[string]bool{}
	for _, p := range platforms {
		lower := strings.ToLower(p)
		if alias, ok := platformAliases[lower]; ok {
			lower = alias
		}
		out[lower] = true
	}
	return out
}

func refuseUnsupportedCatalogPlatform(entry *PluginCatalogEntry) error {
	if len(entry.Platforms) == 0 {
		return nil
	}
	current := hostinfo.OSFamily()
	if !normalizedPlatforms(entry.Platforms)[current] {
		return &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is unavailable on %s; supported platforms: %s.",
			entry.Name, current, strings.Join(entry.Platforms, ", ")), FailureClass: "incompatible"}
	}
	return nil
}

// Needs a newer icebath sentence when the entry requires_icebath rejects the running version, empty otherwise
func catalogIcebathError(entry *PluginCatalogEntry) string {
	if entry.RequiresIcebath == "" || RequiresVersionError(entry.RequiresIcebath) == "" {
		return ""
	}
	return fmt.Sprintf("%s needs Icebath %s; this is %s.", entry.Name, entry.RequiresIcebath, RunningVersion)
}

// Operation error when the catalog says the entry cannot run here by platform or version
func refuseIncompatibleCatalogEntry(ctx context.Context, entry *PluginCatalogEntry) error {
	if err := refuseUnsupportedCatalogPlatform(entry); err != nil {
		return err
	}
	if reason := catalogIcebathError(entry); reason != "" {
		update, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
		if err != nil {
			return err
		}
		return &PluginOperationError{Msg: fmt.Sprintf("%s Update Icebath (`%s`) and install again.", reason, update), FailureClass: "incompatible"}
	}
	return nil
}

// Operation error naming the first candidate name or repo on the kill list, for the mcp catalog
func RaiseIfRemoved(ctx context.Context, candidates ...string) error {
	return raiseIfRemoved(ctx, candidates...)
}

// Operation error when the entry cannot run here by platform or icebath version, for the mcp catalog
func RefuseIncompatibleCatalogEntry(ctx context.Context, entry *PluginCatalogEntry) error {
	return refuseIncompatibleCatalogEntry(ctx, entry)
}

// Options of a catalog entry install beyond the entry itself
type CatalogInstallOptions struct {
	Force        bool
	Ref          string
	AllowRemoved bool
	ScanDecision func(scan any) bool
	BeforeSwap   func(manifest map[string]any, tree string) ([]string, error)
	InspectedSha string
}

// Installs the entry at its catalog pin, or the explicit ref, recording catalog provenance on the install record
func installCatalogEntry(ctx context.Context, entry *PluginCatalogEntry, opts CatalogInstallOptions) (string, map[string]any, string, error) {
	if !opts.AllowRemoved {
		if err := raiseIfRemoved(ctx, entry.Name, entry.Repo); err != nil {
			return "", nil, "", err
		}
	}
	if err := refuseIncompatibleCatalogEntry(ctx, entry); err != nil {
		return "", nil, "", err
	}
	ref := opts.Ref
	if ref == "" {
		ref = entry.Sha
	}
	return installPluginCore(ctx, entry.InstallIdentifier(), installCoreOptions{
		Force: opts.Force, Ref: ref, ScanDecision: opts.ScanDecision, ReviewedPin: entry.Sha,
		AllowRemoved: opts.AllowRemoved, BeforeSwap: opts.BeforeSwap,
		AppTitle: entry.Title, InspectedSha: opts.InspectedSha,
		Catalog: map[string]any{"name": entry.Name, "repo": entry.Repo, "tier": entry.Tier, "pin": entry.Sha},
	})
}

// Operation error form of the installed plugin kill list verdict for update and enable
func refuseIfInstalledRemoved(h, name, pluginDir string) error {
	removed := InstalledPluginRemoval(h, name, pluginDir)
	if removed == nil {
		return nil
	}
	reason := removed.Reason
	if reason == "" {
		reason = "no reason recorded"
	}
	return &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' was removed from the Icebath plugin catalog: %s. Remove it with "+
		"`icebath plugins remove %s`, or reinstall with `icebath plugins install <source> --force --allow-removed` if you trust it.",
		name, reason, name)}
}

// Installer and cache names that are never user state
func skipPreserve(name string) bool {
	return name == "__pycache__" || name == ".git" || name == CatalogSidecar || strings.HasSuffix(name, ".pyc")
}

// Plugin code and control surfaces an update without git must never resurrect from the old tree
func revisionOwnedWithoutGit(rel string) bool {
	suffix := strings.ToLower(pathSuffix(rel))
	parts := strings.Split(filepath.ToSlash(rel), "/")
	return codeFileExtensions[suffix] || noGitRevisionExtensions[suffix] || noGitRevisionFiles[filepath.ToSlash(rel)] ||
		(len(parts) > 0 && noGitRevisionDirs[parts[0]])
}

// Suffix of the final path component as pathlib reads it, empty for dot files
func pathSuffix(p string) string {
	name := filepath.Base(p)
	i := strings.LastIndex(name, ".")
	if i <= 0 || i == len(name)-1 {
		return ""
	}
	return name[i:]
}

// Untracked or ignored and modified tracked paths of a git checkout, untracked nil without a checkout
func localChanges(ctx context.Context, target string) ([]string, []string, error) {
	gitExe := resolveGitExecutable()
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		return nil, nil, nil
	}
	if gitExe == "" {
		return nil, nil, &PluginOperationError{Msg: fmt.Sprintf("Could not inspect local changes for '%s': git executable is unavailable.", filepath.Base(target))}
	}
	status, err := runPluginGit(ctx, gitExe, target, 30*time.Second, "", "status", "--porcelain", "--ignored", "-z", "--untracked-files=all", "--ignored=matching")
	if err != nil {
		return nil, nil, err
	}
	if status.Code != 0 {
		detail := status.Stderr
		if detail == "" {
			detail = status.Stdout
		}
		if detail == "" {
			detail = "git status failed"
		}
		return nil, nil, &PluginOperationError{Msg: fmt.Sprintf("Could not inspect local changes for '%s': %s", filepath.Base(target), pystr.Strip(detail))}
	}
	local, modified := []string{}, []string{}
	for _, item := range strings.Split(status.Stdout, "\x00") {
		if utf8.RuneCountInString(item) < 4 {
			continue
		}
		code, rel := item[:2], item[3:]
		if slices.ContainsFunc(pathParts(rel), skipPreserve) {
			continue
		}
		if code == "??" || code == "!!" {
			local = append(local, rel)
		} else {
			modified = append(modified, rel)
		}
	}
	return local, modified, nil
}

// Components of a relative path as pathlib parts, empty and dot components dropped
func pathParts(rel string) []string {
	var out []string
	for _, p := range strings.Split(filepath.ToSlash(rel), "/") {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// Copies the listed regular files of the tree into the backup stash, keeping their metadata
func stashLocalFiles(target string, rels []string, stash string) error {
	for _, rel := range rels {
		src := filepath.Join(target, rel)
		if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
			continue
		}
		dst := filepath.Join(stash, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			return err
		}
		if err := pyfs.Copy2(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// Rel and each of its parents down to the dot path, as pathlib parents lists them
func selfAndParents(rel string) []string {
	parts := pathParts(rel)
	out := []string{strings.Join(parts, "/")}
	for i := len(parts) - 1; i > 0; i-- {
		out = append(out, strings.Join(parts[:i], "/"))
	}
	return append(out, ".")
}

// Carries user owned files into the staged replacement without reviving old plugin code
func carryUserFiles(old, newTree string, local []string, gitTree bool) ([]string, error) {
	keep := map[string]bool{}
	for _, rel := range local {
		if parts := pathParts(rel); len(parts) > 0 {
			keep[strings.Join(parts, "/")] = true
		} else {
			keep["."] = true
		}
	}
	kept := func(rel string) bool {
		return slices.ContainsFunc(selfAndParents(rel), func(p string) bool { return keep[p] })
	}
	var linked, carried []string
	userLink := func(rel string) {
		if gitTree && kept(rel) {
			linked = append(linked, rel)
		}
	}
	conflict := func(rel, reason string) error {
		if reason == "" {
			reason = "its destination conflicts with the updated plugin"
		}
		return &PluginOperationError{Msg: fmt.Sprintf("Cannot preserve user file '%s': %s. The installed plugin was left unchanged.", filepath.FromSlash(rel), reason)}
	}
	var walk func(dir, relDir string) error
	walk = func(dir, relDir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return &PluginOperationError{Msg: fmt.Sprintf("Could not preserve user files from '%s': %s", old, pyfs.OSErrorText(err))}
		}
		join := func(name string) string {
			if relDir == "" {
				return name
			}
			return relDir + "/" + name
		}
		var dirs, files []string
		for _, e := range entries {
			if st, err := os.Stat(filepath.Join(dir, e.Name())); err == nil && st.IsDir() {
				dirs = append(dirs, e.Name())
			} else {
				files = append(files, e.Name())
			}
		}
		var descend []string
		for _, name := range dirs {
			if skipPreserve(name) || excludedDirs[name] || (!gitTree && relDir == "" && noGitRevisionDirs[name]) {
				continue
			}
			full := filepath.Join(dir, name)
			if st, err := os.Lstat(full); (err == nil && st.Mode()&fs.ModeSymlink != 0) || pm.IsJunction(full) {
				userLink(join(name))
				continue
			}
			descend = append(descend, name)
		}
		for _, name := range files {
			if skipPreserve(name) {
				continue
			}
			src := filepath.Join(dir, name)
			rel := join(name)
			if !gitTree {
				if revisionOwnedWithoutGit(rel) {
					continue
				}
			} else if !kept(rel) {
				continue
			}
			info, err := os.Lstat(src)
			if err != nil {
				return &PluginOperationError{Msg: fmt.Sprintf("Could not preserve user file '%s': %s", filepath.FromSlash(rel), pyfs.OSErrorText(err))}
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				userLink(rel)
				continue
			}
			if !info.Mode().IsRegular() {
				continue
			}
			dst := filepath.Join(newTree, filepath.FromSlash(rel))
			if dstInfo, err := os.Lstat(dst); err == nil {
				if pm.IsJunction(dst) {
					return conflict(rel, "")
				}
				if dstInfo.IsDir() {
					return conflict(rel, "the updated plugin now has a directory at that path")
				}
				if !gitTree {
					continue
				}
			}
			parent, sourceParent := newTree, old
			parts := pathParts(rel)
			for _, part := range parts[:len(parts)-1] {
				parent, sourceParent = filepath.Join(parent, part), filepath.Join(sourceParent, part)
				if pinfo, err := os.Lstat(parent); err == nil {
					if pm.IsJunction(parent) || pinfo.Mode()&fs.ModeSymlink != 0 || !pinfo.IsDir() {
						return conflict(rel, "")
					}
					continue
				}
				sinfo, err := os.Lstat(sourceParent)
				if err != nil {
					return conflict(rel, "its destination could not be prepared")
				}
				if pm.IsJunction(sourceParent) || !sinfo.IsDir() {
					return conflict(rel, "its source path changed during the update")
				}
				mode := sinfo.Mode() & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
				if err := os.Mkdir(parent, mode); err != nil {
					return conflict(rel, "its destination could not be prepared")
				}
				if err := os.Chmod(parent, mode); err != nil {
					return conflict(rel, "its destination could not be prepared")
				}
			}
			if dinfo, err := os.Lstat(dst); err == nil && (dinfo.Mode()&fs.ModeSymlink != 0 || dinfo.Mode().IsRegular()) {
				if err := os.Remove(dst); err != nil {
					return err
				}
			}
			if err := pyfs.Copy2(src, dst); err != nil {
				return err
			}
			carried = append(carried, rel)
		}
		for _, name := range descend {
			if err := walk(filepath.Join(dir, name), join(name)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(old, ""); err != nil {
		return nil, err
	}
	if len(linked) > 0 {
		slices.Sort(linked)
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Cannot preserve symlinked user file(s) %s: links are not followed "+
			"into an update. Replace each with a regular file (or remove it) and retry. The installed plugin was left unchanged.",
			strings.Join(linked, ", "))}
	}
	return carried, nil
}

// Outcome of a catalog re-pin
type RepinResult struct {
	Sha           string
	Changed       bool
	InstalledName string
	Warnings      []string
}

// Surfaces an installed tree exposes, host capabilities, tools, hooks, Python deps, sign ins and a Desktop half
func pluginSurface(manifest map[string]any, tree string) (map[string]map[string]bool, error) {
	if manifest == nil {
		manifest = map[string]any{}
	}
	svc := installedServices()
	if svc == nil || svc.OAuthSurface == nil {
		return nil, &ServiceMissingError{Name: "OAuthSurface"}
	}
	list := func(keys ...string) map[string]bool {
		out := map[string]bool{}
		for _, k := range keys {
			raw, ok := manifest[k].([]any)
			if !ok {
				continue
			}
			for _, x := range raw {
				switch x.(type) {
				case string, bool, int, int64, float64:
					out[pyrepr.Str(x)] = true
				}
			}
			return out
		}
		return out
	}
	name := pyStrOr(manifest["name"])
	if name == "" {
		name = "?"
	}
	return map[string]map[string]bool{
		"capabilities": toSet(declaredCapabilitiesFromManifest(manifest, name)),
		"tools":        list("provides_tools"), "hooks": list("provides_hooks", "hooks"),
		"python_dependencies": list("python_dependencies"), "requires_auth": list("requires_auth"),
		"oauth":   svc.OAuthSurface(manifest),
		"desktop": desktopSurface(tree),
	}, nil
}

// Desktop entry the tree ships by the installers layout rule, root or desktop folder
func desktopSurface(tree string) map[string]bool {
	source := desktopSource(tree)
	if source == "" {
		return map[string]bool{}
	}
	rel, err := filepath.Rel(tree, filepath.Join(source, "plugin.js"))
	if err != nil {
		return map[string]bool{}
	}
	return map[string]bool{filepath.ToSlash(rel): true}
}

// Folder holding the desktop plugin of a package, the root or its desktop folder, empty when neither does
func desktopSource(packageDir string) string {
	for _, source := range []string{packageDir, filepath.Join(packageDir, "desktop")} {
		if st, err := os.Stat(filepath.Join(source, "plugin.js")); err == nil && st.Mode().IsRegular() {
			return source
		}
	}
	return ""
}

func addedSurface(surface string, old, newSet map[string]bool) map[string]bool {
	if presenceSurfaces[surface] {
		if len(old) > 0 {
			return map[string]bool{}
		}
		return newSet
	}
	out := map[string]bool{}
	for k := range newSet {
		if !old[k] {
			out[k] = true
		}
	}
	return out
}

// Added identifiers per surface the new tree widens, empty when nothing widened
func surfaceDelta(old, newSurface map[string]map[string]bool) map[string][]string {
	delta := map[string][]string{}
	for _, k := range surfaceKeys {
		if added := sortedSet(addedSurface(k, old[k], newSurface[k])); len(added) > 0 {
			delta[k] = added
		}
	}
	return delta
}

func surfaceDeltaLines(delta map[string][]string) []string {
	var out []string
	for _, k := range surfaceKeys {
		if v, ok := delta[k]; ok {
			out = append(out, surfaceLabels[k]+": "+strings.Join(v, ", "))
		}
	}
	return out
}

// New pin widens the plugin surface without consent, nothing changed on disk
type RepinConsentRequired struct {
	Name, Sha string
	Delta     map[string][]string
}

func (e *RepinConsentRequired) Error() string {
	return fmt.Sprintf("Updating '%s' to %s adds %s. Confirm to continue.", e.Name, e.Sha[:min(8, len(e.Sha))], strings.Join(surfaceDeltaLines(e.Delta), "; "))
}

// Re-pins a catalog install to the current catalog sha through the pm update transaction, never a pull
func repinCatalogPlugin(ctx context.Context, target string, sidecar map[string]any, interactive bool, consent func(map[string][]string) bool) (*RepinResult, error) {
	h := home.Dir(ctx)
	catalogName := pyrepr.Str(sidecar["catalog_name"])
	entry := GetLiveCatalogEntry(ctx, catalogName)
	if entry == nil {
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Plugin '%s' is no longer in the catalog — it may have been removed. "+
			"See `icebath plugins info` and the removed blocklist.", catalogName)}
	}
	if err := refuseIfInstalledRemoved(h, catalogName, target); err != nil {
		return nil, err
	}
	if atCatalogPin(sidecar, entry.Sha) {
		return &RepinResult{Sha: entry.Sha, InstalledName: filepath.Base(target), Warnings: []string{}}, nil
	}
	if held := catalogIcebathError(entry); held != "" {
		update, err := config.RecommendedUpdateCommand(ctx, h)
		if err != nil {
			return nil, err
		}
		installed := pyStrOr(sidecar["sha"])
		installed = installed[:min(8, len(installed))]
		if installed == "" {
			installed = "its installed version"
		}
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Update of %s to %s not applied: %s %s stays at %s; the update is available after "+
			"you update Icebath (`%s`).", catalogName, entry.Sha[:min(8, len(entry.Sha))], held, catalogName, installed, update), FailureClass: "incompatible"}
	}
	local, modified, err := localChanges(ctx, target)
	if err != nil {
		return nil, err
	}
	gitTree := local != nil
	oldSha8 := pyStrOr(sidecar["sha"])
	if oldSha8 == "" {
		oldSha8 = "old"
	}
	oldSha8 = oldSha8[:min(8, len(oldSha8))]
	installedManifest, err := readManifest(target)
	if err != nil {
		return nil, err
	}
	installedSurface, err := pluginSurface(installedManifest, target)
	if err != nil {
		return nil, err
	}
	dir, err := pluginsDir(h)
	if err != nil {
		return nil, err
	}
	if err := previewRepin(ctx, h, dir, entry, catalogName, installedSurface, consent); err != nil {
		return nil, err
	}
	backup := filepath.Join(filepath.Dir(dir), "plugins-backup", filepath.Base(target)+"-"+oldSha8)
	if err := stashLocalFiles(target, modified, backup); err != nil {
		return nil, err
	}
	if _, err := updatePlugin(ctx, target, updatePluginOptions{CatalogEntry: entry,
		CarryUserFiles: func(staged string) ([]string, error) { return carryUserFiles(target, staged, local, gitTree) }}); err != nil {
		return nil, err
	}
	metadata, err := readInstallMetadata(h)
	if err != nil {
		return nil, err
	}
	var matches []string
	for _, installedName := range sortedKeys(metadata) {
		row, ok := metadata[installedName].(map[string]any)
		if !ok {
			continue
		}
		if block, ok := row["catalog"].(map[string]any); ok && block["name"] == entry.Name && atCatalogPin(block, entry.Sha) {
			matches = append(matches, installedName)
		}
	}
	if len(matches) != 1 {
		shown := "missing"
		if len(matches) > 0 {
			shown = pyrepr.Repr(stringsAny(matches))
		}
		return nil, &PluginOperationError{Msg: fmt.Sprintf("Catalog update published but its install record is ambiguous: %s.", shown)}
	}
	installedName := matches[0]
	newTarget := filepath.Join(filepath.Dir(target), installedName)
	warnings := []string{}
	if len(modified) > 0 {
		warnings = append(warnings, fmt.Sprintf("Local edits to %d tracked file(s) were not carried over; copies are under "+
			"%s (the previous version's files, re-apply by hand).", len(modified), backup))
	}
	if _, statErr := os.Stat(target); newTarget != target && statErr == nil {
		enabled, disabled := getEnabledSet(h), getDisabledSet(h)
		changed := false
		for _, selected := range []map[string]bool{enabled, disabled} {
			if selected[filepath.Base(target)] {
				delete(selected, filepath.Base(target))
				selected[installedName] = true
				changed = true
			}
		}
		if changed {
			if err := admitAndSavePluginSets(ctx, h, enabled, disabled, admitSetOptions{Action: fmt.Sprintf("Rename plugin '%s' to '%s'", filepath.Base(target), installedName), Plugin: installedName}); err != nil {
				return nil, err
			}
		}
		if err := removePluginCore(ctx, h, target); err != nil {
			return nil, err
		}
		warnings = append(warnings, fmt.Sprintf("Plugin renamed itself from '%s' to '%s'; the old directory was removed.", filepath.Base(target), installedName))
	}
	return &RepinResult{Sha: entry.Sha, Changed: true, InstalledName: installedName, Warnings: warnings}, nil
}

// Clones the immutable pin into a preview dir and refuses a widened surface without consent
func previewRepin(ctx context.Context, h, dir string, entry *PluginCatalogEntry, catalogName string, installed map[string]map[string]bool, consent func(map[string][]string) bool) (err error) {
	previewTmp, err := os.MkdirTemp(dir, ".repin-preview-")
	if err != nil {
		return err
	}
	defer pyfs.CloseTempDir(previewTmp, &err)
	previewRoot := filepath.Join(previewTmp, "plugin")
	gitURL, subdir, err := resolveGitURL(entry.InstallIdentifier())
	if err != nil {
		return err
	}
	if _, err := clonePluginRepo(ctx, h, previewRoot, gitURL, entry.Sha, ""); err != nil {
		return err
	}
	previewTarget := previewRoot
	if subdir != "" {
		if previewTarget, err = resolveSubdirWithin(previewRoot, subdir); err != nil {
			return err
		}
	}
	manifest, err := ReadManifestForInstall(previewTarget)
	if err != nil {
		return err
	}
	surface, err := pluginSurface(manifest, previewTarget)
	if err != nil {
		return err
	}
	if delta := surfaceDelta(installed, surface); len(delta) > 0 && !(consent != nil && consent(delta)) {
		return &RepinConsentRequired{Name: catalogName, Sha: entry.Sha, Delta: delta}
	}
	return nil
}

// Re-pins a catalog install printing progress, true when the pin moved
func cmdUpdateCatalog(ctx context.Context, name, target string, sidecar map[string]any, console *Console, interactive bool) (bool, error) {
	h := home.Dir(ctx)
	console.Print(fmt.Sprintf("[dim]Checking catalog pin for %s...[/dim]", name))
	confirm := func(delta map[string][]string) bool {
		console.Print(fmt.Sprintf("\n  [yellow]The new pin of [bold]%s[/bold] adds:[/yellow]", name))
		for _, line := range surfaceDeltaLines(delta) {
			console.Print("    " + line)
		}
		if !interactive || !isTTY() {
			console.Print("  [yellow]Non-interactive session: update NOT applied (fail closed). " +
				"Re-run `icebath plugins update` in a terminal to review and confirm.[/yellow]")
			return false
		}
		return askYes("  Apply this update? [y/N]: ", nil)
	}
	result, err := repinCatalogPlugin(ctx, target, sidecar, interactive, confirm)
	if err != nil {
		return false, err
	}
	verb := "is already at catalog pin"
	if result.Changed {
		verb = "updated to"
	}
	console.Print(fmt.Sprintf("[green]✓[/green] Plugin [bold]%s[/bold] %s %s.", result.InstalledName, verb, result.Sha[:min(8, len(result.Sha))]))
	for _, warning := range result.Warnings {
		console.Print(fmt.Sprintf("[yellow]⚠ %s[/yellow]", warning))
	}
	if result.Changed {
		newTarget := filepath.Join(filepath.Dir(target), result.InstalledName)
		newManifest, err := readManifest(newTarget)
		if err != nil {
			return false, err
		}
		declared := declaredCapabilitiesFromManifest(newManifest, result.InstalledName)
		if len(declared) > 0 {
			cfg := loadConfigOrEmpty(h)
			if len(PendingCapabilities(cfg, result.InstalledName, declared)) > 0 || DeclaredSetChanged(cfg, result.InstalledName, declared) {
				if interactive {
					if err := runCapabilityConsent(ctx, console, result.InstalledName, declared, "update"); err != nil {
						return false, err
					}
				} else {
					console.Print(fmt.Sprintf("[yellow]Plugin %s has new capabilities; review them with "+
						"`icebath plugins capabilities %s`.[/yellow]", result.InstalledName, result.InstalledName))
				}
			}
		}
	}
	return result.Changed, nil
}

func capabilityCounts(entry *PluginCatalogEntry) string {
	caps := entry.Capabilities
	var parts []string
	for _, c := range []struct {
		items []string
		label string
	}{{caps.ProvidesTools, "tool"}, {caps.ProvidesHooks, "hook"}, {caps.ProvidesMiddleware, "middleware"}} {
		if len(c.items) == 0 {
			continue
		}
		plural := ""
		if len(c.items) != 1 && c.label != "middleware" {
			plural = "s"
		}
		parts = append(parts, fmt.Sprintf("%d %s%s", len(c.items), c.label, plural))
	}
	if len(caps.RequiresEnv) > 0 {
		parts = append(parts, fmt.Sprintf("%d env", len(caps.RequiresEnv)))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, ", ")
}

// Version label at short sha when the entry carries a version, else the short sha
func pinLabel(entry *PluginCatalogEntry) string {
	short := entry.Sha[:min(8, len(entry.Sha))]
	if entry.Version != "" {
		return entry.Version + " @ " + short
	}
	return short
}

func renderEntries(entries []*PluginCatalogEntry, console *Console) {
	table := &pyrich.Table{Title: "Icebath Plugin Catalog (curated)", Columns: []pyrich.Column{{Header: "Name", Style: "bold"},
		{Header: "Category"}, {Header: "Tier"}, {Header: "Description"}, {Header: "Pinned", Style: "dim"}, {Header: "Capabilities", Style: "dim"}}}
	rank := map[string]int{}
	for i, t := range CatalogTiers {
		rank[t] = i
	}
	tierRank := func(t string) int {
		if r, ok := rank[t]; ok {
			return r
		}
		return len(rank)
	}
	style := map[string]string{"official": "cyan", "verified": "green", "community": "magenta"}
	sorted := slices.Clone(entries)
	slices.SortStableFunc(sorted, func(a, b *PluginCatalogEntry) int {
		if c := strings.Compare(a.Category, b.Category); c != 0 {
			return c
		}
		if c := tierRank(a.Tier) - tierRank(b.Tier); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	for _, e := range sorted {
		s, ok := style[e.Tier]
		if !ok {
			s = "magenta"
		}
		tier := fmt.Sprintf("[%s]%s[/%s]", s, e.Tier, s)
		desc := e.Description
		if utf8.RuneCountInString(desc) > 60 {
			desc = string([]rune(desc)[:57]) + "..."
		}
		name := e.Name
		if catalogIcebathError(e) != "" {
			name += fmt.Sprintf("\n[yellow]needs Icebath %s[/yellow]", e.RequiresIcebath)
		}
		table.AddRow(name, e.Category, tier, desc, pinLabel(e), capabilityCounts(e))
	}
	console.Print("")
	fmt.Fprintln(console.Out, table.Render(console.Color))
	console.Print("")
	console.Print("[dim]Details:[/dim] icebath plugins info <name>    [dim]Install:[/dim] icebath plugins install <name>")
}

// Ordered catalog entry mapping in the hermes to_dict key order
func entryObject(e *PluginCatalogEntry) *pyjson.Object {
	d := e.ToDict()
	o := pyjson.NewObject()
	for _, k := range []string{"name", "repo", "sha", "description", "maintainer", "tier", "category", "requires_icebath", "subdir",
		"docs_url", "version", "image", "screenshots", "readme", "platforms", "title", "onboarding"} {
		o.Set(k, d[k])
	}
	caps := pyjson.NewObject()
	for _, k := range []string{"provides_tools", "provides_hooks", "provides_middleware", "requires_env"} {
		caps.Set(k, d["capabilities"].(map[string]any)[k])
	}
	o.Set("capabilities", caps)
	o.Set("known_issues", d["known_issues"])
	return o
}

// Searches the curated catalog by name, description and declared tools, an empty term browsing everything
func CmdSearch(ctx context.Context, term string, jsonOutput bool, out io.Writer) error {
	matches := FilterEntries(LoadCatalogLive(ctx), term)
	if jsonOutput {
		results := make([]any, 0, len(matches))
		for _, e := range matches {
			o := entryObject(e)
			if held := catalogIcebathError(e); held != "" {
				o.Set("needs_newer_icebath", held)
			} else {
				o.Set("needs_newer_icebath", nil)
			}
			results = append(results, o)
		}
		doc := pyjson.NewObject()
		doc.Set("query", term)
		doc.Set("results", results)
		text, err := pyjson.Dumps(doc, pyjson.Indent(2))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, text)
		return err
	}
	console := NewConsole(out)
	if len(matches) == 0 {
		if term != "" {
			console.Print(fmt.Sprintf("[yellow]No catalog entries matched '%s'[/yellow]", term))
		} else {
			console.Print("[dim]No catalog entries available.[/dim]")
		}
		return nil
	}
	renderEntries(matches, console)
	return nil
}

// Full catalog entry of a name, the installed plugin details for a non catalog name
func CmdInfo(ctx context.Context, name string, out io.Writer) error {
	entry := GetLiveCatalogEntry(ctx, name)
	console := NewConsole(out)
	if entry == nil {
		return cmdShow(ctx, name, console)
	}
	caps := entry.Capabilities
	console.Print("")
	console.Print(fmt.Sprintf("[bold]%s[/bold] [cyan]\\[%s][/cyan]", entry.Name, entry.Tier))
	if entry.Description != "" {
		console.Print(entry.Description)
	}
	console.Print("")
	requires := ""
	if entry.RequiresIcebath != "" {
		requires = "icebath " + entry.RequiresIcebath
	}
	rows := [][2]string{{"Repo", entry.Repo}, {"Subdir", entry.Subdir}, {"Version", entry.Version}, {"Pinned SHA", entry.Sha},
		{"Image", entry.Image}, {"Maintainer", entry.Maintainer}, {"Requires", requires},
		{"Platforms", strings.Join(entry.Platforms, ", ")}, {"Docs", entry.DocsURL}}
	for _, row := range rows {
		if row[1] != "" {
			console.Print(fmt.Sprintf("[dim]%-12s[/dim] %s", row[0]+":", row[1]))
		}
	}
	console.Print("")
	for _, row := range []struct {
		label string
		items []string
	}{{"Tools", caps.ProvidesTools}, {"Hooks", caps.ProvidesHooks}, {"Middleware", caps.ProvidesMiddleware}, {"Env vars", caps.RequiresEnv}} {
		items := strings.Join(row.items, ", ")
		if items == "" {
			items = "(none)"
		}
		console.Print(fmt.Sprintf("[dim]%-12s[/dim] %s", row.label+":", items))
	}
	console.Print("")
	removed := FindRemoved(ctx, entry.Name, "")
	if removed == nil {
		removed = FindRemoved(ctx, entry.Repo, "")
	}
	if removed != nil {
		reason := removed.Reason
		if reason == "" {
			reason = "no reason recorded"
		}
		date := ""
		if removed.Date != "" {
			date = fmt.Sprintf(" (%s)", removed.Date)
		}
		console.Print(fmt.Sprintf("[red bold]✗ REMOVED from catalog: %s%s[/red bold]", reason, date))
		console.Print("")
	}
	if held := catalogIcebathError(entry); held != "" {
		update, err := config.RecommendedUpdateCommand(ctx, home.Dir(ctx))
		if err != nil {
			return err
		}
		console.Print(fmt.Sprintf("[yellow]⚠ %s Update Icebath (`%s`) to install it.[/yellow]", held, update))
		console.Print("")
	}
	console.Print("[dim]Install:[/dim]     icebath plugins install " + entry.Name)
	console.Print("")
	return nil
}

// Catalog admission validation of a plugin directory, the CI gate, exiting 0 or 1
func CmdValidate(ctx context.Context, path string, asJSON bool, out, errOut io.Writer) error {
	report, err := validatePluginDir(ctx, path)
	if err != nil {
		return err
	}
	if asJSON {
		text, err := pyjson.Dumps(report.ToDict(), pyjson.Indent(2))
		if err != nil {
			return err
		}
		fmt.Fprintln(out, text)
		return &CommandExit{Code: report.ExitCode()}
	}
	console := NewConsole(out)
	console.Print("")
	for _, check := range report.Checks {
		mark := "[red]✗[/red]"
		if check.OK {
			mark = "[green]✓[/green]"
		}
		line := mark + " " + check.Name
		if check.Detail != "" {
			line += fmt.Sprintf(" [dim]— %s[/dim]", check.Detail)
		}
		console.Print(line)
	}
	for _, warning := range report.Warnings {
		console.Print(fmt.Sprintf("[yellow]⚠ %s[/yellow]", warning))
	}
	if report.Isolation != nil {
		iso := report.Isolation
		mark := "[dim]◇[/dim]"
		if iso.HostReady() {
			mark = "[green]◆[/green]"
		}
		console.Print(fmt.Sprintf("%s Isolation [dim]— %s[/dim]", mark, iso.Summary()))
		for _, note := range iso.Notes {
			console.Print(fmt.Sprintf("  [dim]· %s[/dim]", note))
		}
	}
	console.Print("")
	if report.OK() {
		console.Print("[green bold]Validation passed.[/green bold]")
	} else {
		console.Print("[red bold]Validation failed.[/red bold]")
	}
	return &CommandExit{Code: report.ExitCode()}
}

// Catalog entries merged with local install state for the dashboard, matched by catalog name first
func InstalledCatalogState(ctx context.Context, installed map[string]map[string]any, order []string) (map[string]any, error) {
	h := home.Dir(ctx)
	byCatalogName := map[string]map[string]any{}
	for _, key := range order {
		local := installed[key]
		sidecar, err := catalogInstallRecord(h, orEmpty(local["dir"]))
		if err != nil {
			return nil, err
		}
		if sidecar != nil {
			merged := map[string]any{}
			for k, v := range local {
				merged[k] = v
			}
			merged["sidecar"] = sidecar
			byCatalogName[pyrepr.Str(sidecar["catalog_name"])] = merged
		}
	}
	entries := []any{}
	for _, entry := range LoadCatalogLive(ctx) {
		local, ok := byCatalogName[entry.Name]
		if !ok {
			local, ok = installed[entry.Name]
		}
		var sidecar map[string]any
		if ok {
			sidecar, _ = local["sidecar"].(map[string]any)
		}
		var installedSha any
		if sidecar != nil && jsonTruthy(sidecar["sha"]) {
			installedSha = pyrepr.Str(sidecar["sha"])
		}
		row := entry.ToDict()
		row["sha_short"] = entry.Sha[:min(7, len(entry.Sha))]
		row["capability_summary"] = EntryCapabilitySummary(entry)
		row["installed"] = ok
		row["installed_sha"] = installedSha
		row["update_available"] = installedSha != nil && !atCatalogPin(sidecar, entry.Sha)
		row["runtime_status"] = nil
		if ok {
			row["runtime_status"] = local["runtime_status"]
		}
		config.SetMappingKeys(row, append(config.MappingKeys(entry.ToDict()), "sha_short", "capability_summary", "installed",
			"installed_sha", "update_available", "runtime_status"))
		entries = append(entries, row)
	}
	removed := []any{}
	for _, r := range ResolvedRemovedEntries(ctx) {
		gone := map[string]any{"name": r.Name, "repo": r.Repo, "reason": r.Reason, "date": r.Date}
		config.SetMappingKeys(gone, []string{"name", "repo", "reason", "date"})
		removed = append(removed, gone)
	}
	out := map[string]any{"entries": entries, "removed": removed, "generated_at": utcISONow()}
	config.SetMappingKeys(out, []string{"entries", "removed", "generated_at"})
	return out, nil
}

// Current UTC time as datetime isoformat with Z, microseconds only when nonzero
func utcISONow() string {
	now := time.Now().UTC()
	text := now.Format("2006-01-02T15:04:05")
	if us := now.Nanosecond() / 1000; us != 0 {
		text += fmt.Sprintf(".%06d", us)
	}
	return text + "Z"
}

// Provenance fields of one installed plugin row with the current pin and update flag when the pins know it
func CatalogRowFields(h, dir string, pins, versions map[string]string) (map[string]any, error) {
	sidecar, err := catalogInstallRecord(h, dir)
	if sidecar == nil {
		return map[string]any{}, err
	}
	installedSha := strings.ToLower(pyStrOr(sidecar["sha"]))
	tier := pyStrOr(sidecar["tier"])
	if tier == "" {
		tier = "community"
	}
	row := map[string]any{"catalog_name": sidecar["catalog_name"], "catalog_tier": tier, "installed_sha": installedSha}
	name := pyrepr.Str(sidecar["catalog_name"])
	if pin := pins[name]; pin != "" {
		row["catalog_sha"] = pin
		row["catalog_version"] = nil
		if v := versions[name]; v != "" {
			row["catalog_version"] = v
		}
		row["update_available"] = installedSha != "" && !atCatalogPin(sidecar, pin)
	}
	return row, nil
}

// Catalog name to pinned sha from the live catalog
func CatalogPins(ctx context.Context) map[string]string {
	pins, _, _ := CatalogRowsMaps(ctx)
	return pins
}

// Catalog name to display title for entries carrying one
func CatalogTitles(ctx context.Context) map[string]string {
	_, _, titles := CatalogRowsMaps(ctx)
	return titles
}

// Catalog name to version label for entries carrying one
func CatalogVersions(ctx context.Context) map[string]string {
	_, versions, _ := CatalogRowsMaps(ctx)
	return versions
}

// Pins, versions and titles maps from one live catalog resolution, all empty when it fails
func CatalogRowsMaps(ctx context.Context) (map[string]string, map[string]string, map[string]string) {
	pins, versions, titles := map[string]string{}, map[string]string{}, map[string]string{}
	var entries []*PluginCatalogEntry
	err := callGuarded(func() error { entries = LoadCatalogLive(ctx); return nil })
	if interrupt := (*UserInterrupt)(nil); errors.As(err, &interrupt) {
		panic(interrupt)
	}
	if err != nil {
		return pins, versions, titles
	}
	for _, e := range entries {
		pins[e.Name] = e.Sha
		if e.Version != "" {
			versions[e.Name] = e.Version
		}
		if e.Title != "" {
			titles[e.Name] = e.Title
		}
	}
	return pins, versions, titles
}
