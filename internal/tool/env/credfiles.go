package env

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/home"
	"github.com/nickheyer/icebath/internal/tool"
)

const (
	DefaultContainerBase  = "/root/.icebath"
	generatedCacheMaxAge  = 24 * time.Hour
	homeRelativeCacheBase = "~/.icebath"
)

type Mount struct {
	HostPath      string
	ContainerPath string
}

type ReadGuard interface {
	ReadBlockError(ctx context.Context, path string) (string, error)
}

type Skills interface {
	ExternalDirs(ctx context.Context) ([]string, error)
	ProjectDirs(ctx context.Context) ([]string, error)
	Excluded(dirName string) bool
}

type CredentialFilesOptions struct {
	Homes    *home.Resolver
	Config   *config.Store
	Guard    ReadGuard
	Skills   Skills
	Terminal TerminalSettings
	Backends *Providers
}

type sessionFiles struct {
	order []string
	byCP  map[string]string
}

type CredentialFiles struct {
	homes    *home.Resolver
	cfg      *config.Store
	guard    ReadGuard
	skills   Skills
	terminal TerminalSettings
	backends *Providers

	mu         sync.Mutex
	registered map[string]*sessionFiles
	byHome     map[string][]Mount
	safeSkills string
}

func NewCredentialFiles(o CredentialFilesOptions) (*CredentialFiles, error) {
	switch {
	case o.Homes == nil:
		return nil, &tool.UnfilledError{Name: "env.CredentialFilesOptions.Homes"}
	case o.Config == nil:
		return nil, &tool.UnfilledError{Name: "env.CredentialFilesOptions.Config"}
	case o.Guard == nil:
		return nil, &tool.UnfilledError{Name: "env.ReadGuard"}
	case o.Skills == nil:
		return nil, &tool.UnfilledError{Name: "env.Skills"}
	case o.Terminal == nil:
		return nil, &tool.UnfilledError{Name: "env.TerminalSettings"}
	case o.Backends == nil:
		return nil, &tool.UnfilledError{Name: "env.CredentialFilesOptions.Backends"}
	}
	return &CredentialFiles{homes: o.Homes, cfg: o.Config, guard: o.Guard, skills: o.Skills, terminal: o.Terminal,
		backends: o.Backends, registered: map[string]*sessionFiles{}, byHome: map[string][]Mount{}}, nil
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func containedHostPath(rel, homeDir, absMsg, traversalMsg string) (string, bool) {
	if filepath.IsAbs(rel) {
		slog.Warn(strings.Replace(absMsg, "%r", quotedPath(rel), 1))
		return "", false
	}
	host := filepath.Join(homeDir, rel)
	if err := tool.WithinDir(host, homeDir); err != nil {
		msg := strings.Replace(traversalMsg, "%r", quotedPath(rel), 1)
		slog.Warn(strings.Replace(msg, "%s", err.Error(), 1))
		return "", false
	}
	resolved, err := resolveLoose(host)
	if err != nil {
		return "", false
	}
	return resolved, true
}

func joinBase(base, rel string) string {
	return strings.TrimRight(base, "/") + "/" + rel
}

func (c *CredentialFiles) RegisterFile(ctx context.Context, session, rel, containerBase string) bool {
	homeDir, err := c.homes.Home(ctx)
	if err != nil {
		slog.Error("credential_files: refusing " + quotedPath(rel) + " — the profile home cannot be resolved: " + err.Error())
		return false
	}
	resolved, ok := containedHostPath(rel, homeDir,
		"credential_files: rejected absolute path %r (must be relative to ICEBATH_HOME)",
		"credential_files: rejected path traversal %r (%s)")
	if !ok {
		return false
	}
	if !isFile(resolved) {
		slog.Debug("credential_files: skipping " + resolved + " (not found)")
		return false
	}
	denied, err := c.guard.ReadBlockError(ctx, resolved)
	if err != nil {
		slog.Error("credential_files: refusing "+quotedPath(rel)+" — read guard raised", "err", err)
		return false
	}
	if denied != "" {
		slog.Warn("credential_files: refused " + quotedPath(rel) + " — it is a credential store the agent " +
			"is denied from reading; a skill may mount its own service token, not the master key files")
		return false
	}
	cp := joinBase(containerBase, rel)
	c.mu.Lock()
	files := c.registered[session]
	if files == nil {
		files = &sessionFiles{byCP: map[string]string{}}
		c.registered[session] = files
	}
	if _, had := files.byCP[cp]; !had {
		files.order = append(files.order, cp)
	}
	files.byCP[cp] = resolved
	c.mu.Unlock()
	slog.Debug("credential_files: registered " + resolved + " -> " + cp)
	return true
}

func (c *CredentialFiles) RegisterFiles(ctx context.Context, session string, entries []string, containerBase string) []string {
	var missing []string
	for _, entry := range entries {
		rel := strings.TrimSpace(entry)
		if rel != "" && !c.RegisterFile(ctx, session, rel, containerBase) {
			missing = append(missing, rel)
		}
	}
	return missing
}

func (c *CredentialFiles) configFiles(ctx context.Context) []Mount {
	homeDir, err := c.homes.Home(ctx)
	if err != nil {
		slog.Warn("Could not read terminal.credential_files from config: " + err.Error())
		return nil
	}
	key, err := c.homes.Key(homeDir)
	if err != nil {
		slog.Warn("Could not read terminal.credential_files from config: " + err.Error())
		return nil
	}
	c.mu.Lock()
	cached, ok := c.byHome[key]
	c.mu.Unlock()
	if ok {
		return cached
	}
	var result []Mount
	cfg, err := c.cfg.Load(ctx)
	if err != nil {
		slog.Warn("Could not read terminal.credential_files from config: " + err.Error())
	} else if node := cfg.Terminal.Extra("credential_files"); node != nil && node.Kind == yaml.SequenceNode {
		for _, item := range node.Content {
			rel := ""
			if item.Kind == yaml.ScalarNode && item.ShortTag() == "!!str" {
				rel = strings.TrimSpace(item.Value)
			}
			if rel == "" {
				continue
			}
			resolved, ok := containedHostPath(rel, homeDir,
				"credential_files: rejected absolute config path %r",
				"credential_files: rejected config path traversal %r (%s)")
			if ok && isFile(resolved) {
				result = append(result, Mount{HostPath: resolved, ContainerPath: DefaultContainerBase + "/" + rel})
			}
		}
	}
	c.mu.Lock()
	c.byHome[key] = result
	c.mu.Unlock()
	return result
}

func (c *CredentialFiles) Mounts(ctx context.Context, session string) []Mount {
	c.mu.Lock()
	var order []string
	byCP := map[string]string{}
	if files := c.registered[session]; files != nil {
		order = append(order, files.order...)
		for k, v := range files.byCP {
			byCP[k] = v
		}
	}
	c.mu.Unlock()
	mounts := map[string]string{}
	var keys []string
	for _, cp := range order {
		if hp := byCP[cp]; isFile(hp) {
			mounts[cp] = hp
			keys = append(keys, cp)
		}
	}
	for _, m := range c.configFiles(ctx) {
		if _, had := mounts[m.ContainerPath]; !had && isFile(m.HostPath) {
			mounts[m.ContainerPath] = m.HostPath
			keys = append(keys, m.ContainerPath)
		}
	}
	out := make([]Mount, len(keys))
	for i, cp := range keys {
		out[i] = Mount{HostPath: mounts[cp], ContainerPath: cp}
	}
	return out
}

func (c *CredentialFiles) Clear(session string) {
	c.mu.Lock()
	delete(c.registered, session)
	c.mu.Unlock()
}

type skillRoot struct {
	host      string
	container string
}

func (c *CredentialFiles) skillRoots(ctx context.Context, containerBase string) ([]skillRoot, error) {
	base := strings.TrimRight(containerBase, "/")
	homeDir, err := c.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	var roots []skillRoot
	if dir := home.Skills.In(homeDir); isDir(dir) {
		roots = append(roots, skillRoot{dir, base + "/skills"})
	}
	external, err := c.skills.ExternalDirs(ctx)
	if err != nil {
		return nil, err
	}
	project, err := c.skills.ProjectDirs(ctx)
	if err != nil {
		return nil, err
	}
	for _, group := range []struct {
		label string
		dirs  []string
	}{{"external_skills", external}, {"project_skills", project}} {
		for idx, d := range group.dirs {
			if isDir(d) {
				roots = append(roots, skillRoot{d, base + "/" + group.label + "/" + strconv.Itoa(idx)})
			}
		}
	}
	return roots, nil
}

type skillDir struct {
	dir   string
	files []string
}

func (c *CredentialFiles) walkSkillTree(root string) []skillDir {
	var out []skillDir
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var files, dirs []string
		for _, e := range entries {
			full := filepath.Join(dir, e.Name())
			if e.Type()&fs.ModeSymlink != 0 {
				continue
			}
			if e.IsDir() {
				if !c.skills.Excluded(e.Name()) {
					dirs = append(dirs, full)
				}
				continue
			}
			if e.Type().IsRegular() {
				files = append(files, full)
			}
		}
		out = append(out, skillDir{dir: dir, files: files})
		sort.Strings(dirs)
		for _, d := range dirs {
			walk(d)
		}
	}
	walk(root)
	return out
}

func (c *CredentialFiles) SkillMounts(ctx context.Context, containerBase string) ([]Mount, error) {
	roots, err := c.skillRoots(ctx, containerBase)
	if err != nil {
		return nil, err
	}
	out := make([]Mount, 0, len(roots))
	for _, r := range roots {
		host, err := c.safeSkillsPath(r.host)
		if err != nil {
			return nil, err
		}
		out = append(out, Mount{HostPath: host, ContainerPath: r.container})
	}
	return out, nil
}

func (c *CredentialFiles) safeSkillsPath(skillsDir string) (string, error) {
	var links []string
	_ = filepath.WalkDir(skillsDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && p != skillsDir && d.Type()&fs.ModeSymlink != 0 {
			links = append(links, p)
		}
		return nil
	})
	if len(links) == 0 {
		return skillsDir, nil
	}
	for _, link := range links {
		target, _ := os.Readlink(link)
		slog.Warn("credential_files: skipping symlink in skills dir: " + link + " -> " + target)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.safeSkills != "" && isDir(c.safeSkills) {
		_ = os.RemoveAll(c.safeSkills)
	}
	safe, err := os.MkdirTemp("", "icebath-skills-safe-")
	if err != nil {
		return "", err
	}
	c.safeSkills = safe
	for _, sd := range c.walkSkillTree(skillsDir) {
		rel, err := filepath.Rel(skillsDir, sd.dir)
		if err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Join(safe, rel), 0o777); err != nil {
			return "", err
		}
		for _, item := range sd.files {
			relFile, err := filepath.Rel(skillsDir, item)
			if err != nil {
				return "", err
			}
			if err := copyPreserving(item, filepath.Join(safe, relFile)); err != nil {
				return "", err
			}
		}
	}
	slog.Info("credential_files: created symlink-safe skills copy at " + safe)
	return safe, nil
}

func copyPreserving(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func (c *CredentialFiles) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.safeSkills == "" {
		return nil
	}
	err := os.RemoveAll(c.safeSkills)
	c.safeSkills = ""
	return err
}

func (c *CredentialFiles) SkillFiles(ctx context.Context, containerBase string) ([]Mount, error) {
	roots, err := c.skillRoots(ctx, containerBase)
	if err != nil {
		return nil, err
	}
	var out []Mount
	for _, r := range roots {
		for _, sd := range c.walkSkillTree(r.host) {
			for _, item := range sd.files {
				rel, err := filepath.Rel(r.host, item)
				if err != nil {
					return nil, err
				}
				out = append(out, Mount{HostPath: item, ContainerPath: r.container + "/" + filepath.ToSlash(rel)})
			}
		}
	}
	return out, nil
}

type cacheDir struct {
	sub    string
	legacy *home.LegacyEntry
	entry  home.Entry
}

func legacy(l home.LegacyEntry) *home.LegacyEntry { return &l }

var cacheDirs = []cacheDir{
	{"cache/documents", legacy(home.DocumentCache), ""},
	{"cache/images", legacy(home.ImageCache), ""},
	{"cache/audio", legacy(home.AudioCache), ""},
	{"cache/videos", legacy(home.VideoCache), ""},
	{"cache/screenshots", legacy(home.ScreenshotCache), ""},
	{"cache/web", legacy(home.WebCache), ""},
	{"cache/delegation", legacy(home.DelegationCache), ""},
	{"cache/spillover", nil, home.Spillover},
	{"cache/generated", nil, home.GeneratedMedia},
	{"images", nil, home.Images},
	{"attachments", nil, home.Attachments},
	{"composer-pastes", nil, home.ComposerPastes},
}

func (c *CredentialFiles) cacheRoots(ctx context.Context, containerBase string, create bool) ([]skillRoot, error) {
	base := strings.TrimRight(containerBase, "/")
	homeDir, err := c.homes.Home(ctx)
	if err != nil {
		return nil, err
	}
	var out []skillRoot
	for _, d := range cacheDirs {
		host := d.entry.In(homeDir)
		if d.legacy != nil {
			host = d.legacy.In(homeDir)
		}
		if !isDir(host) {
			if !create {
				continue
			}
			if err := os.MkdirAll(host, 0o777); err != nil {
				continue
			}
		}
		out = append(out, skillRoot{host, base + "/" + d.sub})
	}
	return out, nil
}

func (c *CredentialFiles) CacheMounts(ctx context.Context, containerBase string) ([]Mount, error) {
	roots, err := c.cacheRoots(ctx, containerBase, true)
	if err != nil {
		return nil, err
	}
	out := make([]Mount, len(roots))
	for i, r := range roots {
		out[i] = Mount{HostPath: r.host, ContainerPath: r.container}
	}
	return out, nil
}

func relativeTo(p, root string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

func posixJoin(root, rel string) string {
	rel = filepath.ToSlash(rel)
	if root == "" || strings.HasSuffix(root, "/") {
		return root + rel
	}
	return root + "/" + rel
}

func (c *CredentialFiles) MapCachePathToContainer(ctx context.Context, hostPath, containerBase string) (string, bool, error) {
	mounts, err := c.CacheMounts(ctx, containerBase)
	if err != nil {
		return "", false, err
	}
	for _, m := range mounts {
		if rel, ok := relativeTo(hostPath, m.HostPath); ok {
			return posixJoin(m.ContainerPath, rel), true, nil
		}
	}
	real, err := resolveLoose(hostPath)
	if err != nil {
		return "", false, nil
	}
	for _, m := range mounts {
		root, err := resolveLoose(m.HostPath)
		if err != nil {
			return "", false, nil
		}
		if rel, ok := relativeTo(real, root); ok {
			return posixJoin(m.ContainerPath, rel), true, nil
		}
	}
	return "", false, nil
}

func (c *CredentialFiles) terminalBackend(ctx context.Context) (string, error) {
	v, err := c.terminal.TerminalEnv(ctx, "TERMINAL_ENV")
	if err != nil {
		return "", err
	}
	if v == "" {
		v = "local"
	}
	return strings.ToLower(strings.TrimSpace(v)), nil
}

func (c *CredentialFiles) FromAgentVisibleCachePath(ctx context.Context, containerPath, containerBase string) (string, error) {
	backend, err := c.terminalBackend(ctx)
	if err != nil || backend != "docker" {
		return containerPath, err
	}
	mounts, err := c.CacheMounts(ctx, containerBase)
	if err != nil {
		return "", err
	}
	for _, m := range mounts {
		if rel, ok := relativeTo(containerPath, m.ContainerPath); ok {
			return filepath.Join(m.HostPath, rel), nil
		}
	}
	return containerPath, nil
}

func (c *CredentialFiles) ToAgentVisibleCachePath(ctx context.Context, hostPath, containerBase string) (string, error) {
	backend, err := c.terminalBackend(ctx)
	if err != nil {
		return "", err
	}
	switch backend {
	case "ssh", "daytona", "vercel_sandbox":
		containerBase = homeRelativeCacheBase
	case "docker", "modal":
	default:
		flags, ok, err := c.backends.Flags(ctx, backend, "")
		if err != nil || !ok || flags.CachePathBase == "" {
			return hostPath, nil
		}
		containerBase = flags.CachePathBase
	}
	mapped, ok, err := c.MapCachePathToContainer(ctx, hostPath, containerBase)
	if err != nil {
		return "", err
	}
	if !ok {
		return hostPath, nil
	}
	return mapped, nil
}

func (c *CredentialFiles) CacheFiles(ctx context.Context, containerBase string) ([]Mount, error) {
	roots, err := c.cacheRoots(ctx, containerBase, false)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-generatedCacheMaxAge)
	genRoot := strings.TrimRight(containerBase, "/") + "/cache/generated"
	var out []Mount
	for _, r := range roots {
		_ = filepath.WalkDir(r.host, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == r.host {
				return nil
			}
			info, err := os.Lstat(p)
			if err != nil || !info.Mode().IsRegular() {
				return nil
			}
			if r.container == genRoot && info.ModTime().Before(cutoff) {
				return nil
			}
			rel, err := filepath.Rel(r.host, p)
			if err != nil {
				return nil
			}
			out = append(out, Mount{HostPath: p, ContainerPath: r.container + "/" + rel})
			return nil
		})
	}
	return out, nil
}
