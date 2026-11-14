package config

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/home"
)

// Marks the process a safe worker, which reads no installed plugin manifest
func WithSafeWorker() Option {
	return func(s *Store) { s.envs.safeWorker = true }
}

// Manifest file of one platform plugin installed under a home, Err set when its directory cannot be searched
type platformManifestFile struct {
	Dir         string
	Path        string
	RequireKind bool
	Info        fs.FileInfo
	Err         error
}

// Reports a directory name that can hold a plugin, anything but a double underscore name
func isPluginDirName(name string) bool {
	return !(strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__"))
}

// Manifest files of the platform plugins installed under the home of ctx, in its plugins folder and platforms subfolder
func (e *Env) installedPlatformManifests(ctx context.Context) ([]platformManifestFile, error) {
	h, err := e.s.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	plugins := home.Plugins.In(h)
	var out []platformManifestFile
	for _, root := range []struct {
		dir         string
		requireKind bool
	}{{home.PlatformPlugins.In(h), false}, {plugins, true}} {
		if fi, err := os.Stat(root.dir); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
			continue
		}
		entries, err := os.ReadDir(root.dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			out = append(out, platformManifestFile{Dir: root.dir, RequireKind: root.requireKind, Err: err})
			continue
		}
		for _, entry := range entries {
			dir := filepath.Join(root.dir, entry.Name())
			if fi, err := os.Stat(dir); !isPluginDirName(entry.Name()) || err != nil || !fi.IsDir() {
				continue
			}
			for _, name := range []string{"plugin.yaml", "plugin.yml"} {
				path := filepath.Join(dir, name)
				fi, err := os.Stat(path)
				if errors.Is(err, fs.ErrPermission) {
					out = append(out, platformManifestFile{Dir: entry.Name(), RequireKind: root.requireKind, Err: err})
					break
				}
				if err != nil || !fi.Mode().IsRegular() {
					continue
				}
				out = append(out, platformManifestFile{Dir: entry.Name(), Path: path, RequireKind: root.requireKind, Info: fi})
				break
			}
		}
	}
	return out, nil
}

// Change detection key over the installed platform manifests of the home of ctx, new after any add, removal or edit
func (e *Env) InstalledPlatformStamp(ctx context.Context) (string, error) {
	files, err := e.installedPlatformManifests(ctx)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, f := range files {
		signature := "<" + errorClass(f.Err) + ">"
		if f.Path != "" {
			signature = fmt.Sprint(fileSignature(f.Info))
		}
		fmt.Fprintf(&b, "%s\x00%s\x00%s\n", f.Dir, f.Path, signature)
	}
	return b.String(), nil
}

// Variables each installed platform plugin of the home of ctx declares, with the manifests skipped as unreadable or unparseable
func (e *Env) InstalledPlatforms(ctx context.Context, strict bool) (sets []PlatformEnvSet, skipped []string, err error) {
	if e.s.envs.safeWorker {
		return nil, nil, nil
	}
	files, err := e.installedPlatformManifests(ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range files {
		if f.Path == "" {
			slog.Warn(fmt.Sprintf("Skipping unreadable plugin directory %s: %v", f.Dir, f.Err))
			skipped = append(skipped, f.Dir)
			continue
		}
		data, err := os.ReadFile(f.Path)
		if err != nil {
			if strict && !f.RequireKind {
				return nil, nil, err
			}
			slog.Warn(fmt.Sprintf("Skipping unreadable plugin manifest %s: %v", f.Path, err))
			skipped = append(skipped, f.Path)
			continue
		}
		var manifest yaml.Node
		if err := LoadYAML(data, &manifest); err != nil {
			skipped = append(skipped, f.Path)
			continue
		}
		declares := manifest.Kind != 0 && isSet(&manifest)
		if (declares && !isMapping(&manifest)) || (f.RequireKind && orText(childOf(&manifest, "kind")) != "platform") {
			continue
		}
		sets = append(sets, PlatformManifestEnv(f.Dir, &manifest))
	}
	return sets, skipped, nil
}

// Upper case secret variables the installed platform plugins of the home declare, complete false when a manifest was skipped
func (e *Env) InstalledPlatformSecrets(ctx context.Context) (names []string, complete bool, err error) {
	sets, skipped, err := e.InstalledPlatforms(ctx, true)
	if err != nil {
		return nil, false, err
	}
	return e.s.envs.registry.SecretPlatformEnv(sets), len(skipped) == 0, nil
}

// Adds the variables the installed platform plugins of the home of ctx declare to the registry
func (e *Env) RegisterInstalledPlatforms(ctx context.Context) error {
	sets, _, err := e.InstalledPlatforms(ctx, false)
	if err != nil {
		return err
	}
	r := e.s.envs.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, set := range sets {
		for _, v := range set.Vars {
			r.add(v.Name, v.Var)
		}
	}
	return nil
}
