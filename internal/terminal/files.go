package terminal

import (
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/config"
	terminalconfig "github.com/nickheyer/icebath/internal/terminal/config"
)

// Where a sandbox keeps the profile folder's files when its backend says nothing else
const SandboxProfile = "/root/.icebath"

// A host file and where a sandbox sees it
type Mount struct {
	Host    string
	Sandbox string
}

// Credential files the profile hands its remote sandboxes: the files the credential files setting names under the profile folder that
// exist there, each under SandboxProfile; an absolute path or one leaving the folder is refused
func CredentialFiles(cfg *config.Config) []Mount {
	dir := cfg.ProfileDir(cfg.Profile())
	var mounts []Mount
	for _, rel := range cfg.GetStringSlice(terminalconfig.CredentialFiles) {
		if rel = strings.TrimSpace(rel); rel == "" {
			continue
		}
		host, ok := profileFile(dir, rel, "credential_files: rejected absolute config path", "credential_files: rejected config path traversal")
		if !ok {
			continue
		}
		mount := Mount{Host: host, Sandbox: SandboxProfile + "/" + filepath.ToSlash(rel)}
		if !slices.ContainsFunc(mounts, func(m Mount) bool { return m.Sandbox == mount.Sandbox }) {
			mounts = append(mounts, mount)
		}
	}
	return mounts
}

// Regular file at a path relative to the profile folder dir, resolved through links; an absolute path or one leaving the folder is
// refused with the message
func profileFile(dir, rel, absolute, traversal string) (string, bool) {
	if filepath.IsAbs(rel) {
		slog.Warn(absolute, "path", rel)
		return "", false
	}
	host := filepath.Join(dir, rel)
	if name, err := filepath.Rel(dir, host); err != nil || !filepath.IsLocal(name) {
		slog.Warn(traversal, "path", rel)
		return "", false
	}
	if real, err := filepath.EvalSymlinks(host); err == nil {
		host = real
	}
	if st, err := os.Stat(host); err != nil || !st.Mode().IsRegular() {
		return "", false
	}
	return host, true
}

// What the skills a session can use need of a sandbox
type Skills struct {
	// The profile's own skill folder, the extra ones and the trusted project's
	Own      string
	External []string
	Project  []string
	// Credential files of the profile folder the session's skills read, by path in it
	Files []string
}

// Caches of secret values and the sidecar token, relative to the profile folder
var SecretCaches = []string{"bws_cache.json", "bws_cache.enc.json", "op_cache.json", "photon-sidecar.json", "cache/bws_cache.json", "cache/bws_cache.enc.json",
	"cache/op_cache.json", "runtime/photon-sidecar.json"}

// Credential stores of the profile folder a sandbox is never handed for a skill, which mounts its own service token, never the master keys,
// and the file tools never read
var CredentialStores = append([]string{"auth.json", "auth.lock", ".anthropic_oauth.json", ".env", ".op.env", "webhook_subscriptions.json", "auth/google_oauth.json",
	"vault/vault.key", "vault/vault.json.enc", "vault/vault.keyref", "proxy/ca.key", "proxy/management.token"}, SecretCaches...)

// Whether the resolved host path is one of the profile folder's credential stores
func credentialStore(dir, host string) bool {
	for _, name := range CredentialStores {
		if host == resolved(filepath.Join(dir, filepath.FromSlash(name))) {
			return true
		}
	}
	return false
}

// Files and folders of the profile a sandbox is handed read only, and where it sees each under base, its profile folder. Files: the
// credential files the session's skills read, then those the settings name. Folders: the skill folders, the profile's own as skills, the
// extra ones as external_skills/<n> and the trusted project's as project_skills/<n>, then the cache folders of CacheDirs, made when missing
func Handed(cfg *config.Config, base string, skills Skills) (files, folders []Mount) {
	base = strings.TrimRight(base, "/")
	dir := cfg.ProfileDir(cfg.Profile())
	add := func(list *[]Mount, mount Mount) {
		if !slices.ContainsFunc(files, func(m Mount) bool { return m.Sandbox == mount.Sandbox }) &&
			!slices.ContainsFunc(folders, func(m Mount) bool { return m.Sandbox == mount.Sandbox }) {
			*list = append(*list, mount)
		}
	}
	needed := slices.Clone(skills.Files)
	slices.Sort(needed)
	for _, rel := range slices.Compact(needed) {
		if rel = strings.TrimSpace(rel); rel == "" {
			continue
		}
		host, ok := profileFile(dir, rel, "credential_files: rejected absolute path (must be relative to the profile folder)", "credential_files: rejected path traversal")
		if !ok {
			continue
		}
		if credentialStore(dir, host) {
			slog.Warn("credential_files: refused a credential store the agent is denied from reading; a skill may mount its own service token, not the master key files", "path", rel)
			continue
		}
		add(&files, Mount{Host: host, Sandbox: base + "/" + filepath.ToSlash(rel)})
	}
	for _, mount := range CredentialFiles(cfg) {
		add(&files, Mount{Host: mount.Host, Sandbox: strings.Replace(mount.Sandbox, SandboxProfile, base, 1)})
	}
	folder := func(host, sandbox string) {
		if st, err := os.Stat(host); host != "" && err == nil && st.IsDir() {
			add(&folders, Mount{Host: host, Sandbox: sandbox})
		}
	}
	folder(skills.Own, base+"/skills")
	for i, external := range skills.External {
		folder(external, fmt.Sprintf("%s/external_skills/%d", base, i))
	}
	for i, project := range skills.Project {
		folder(project, fmt.Sprintf("%s/project_skills/%d", base, i))
	}
	for _, cache := range CacheDirs {
		host := filepath.Join(dir, filepath.FromSlash(cache))
		if err := os.MkdirAll(host, 0o755); err != nil {
			slog.Debug("Could not make a cache folder a sandbox is handed", "path", host, "error", err)
			continue
		}
		folder(host, base+"/"+cache)
	}
	return files, folders
}

// Folders of the profile, relative to its folder, that a sandbox is handed under the backend's Home: the caches tools save files in for
// the agent to read, such as the full text of a web page
var CacheDirs = []string{"cache/documents", "cache/images", "cache/audio", "cache/videos", "cache/screenshots", "cache/web", "cache/delegation",
	"cache/spillover", "cache/terminal-output", "cache/generated", "cache/exec", "cache/computer_use", "images", "attachments", "composer-pastes"}

// Path the commands of the profile's backend see a file of the profile's caches at: where the backend's Home holds the cache folder in
// a sandbox, its sandboxes that keep copies brought up to date first; the host path itself where host paths hold or the file is in no
// cache folder
func (t *Terminal) Visible(cfg *config.Config, host string) string {
	backend, err := t.Configured(cfg)
	if err != nil || backend.Home == "" {
		return host
	}
	dir := cfg.ProfileDir(cfg.Profile())
	for _, candidate := range []string{host, resolved(host)} {
		for _, cache := range CacheDirs {
			root := filepath.Join(dir, filepath.FromSlash(cache))
			for _, base := range []string{root, resolved(root)} {
				if rel, err := filepath.Rel(base, candidate); err == nil && filepath.IsLocal(rel) {
					t.push(cfg)
					return path.Join(backend.Home, cache, filepath.ToSlash(rel))
				}
			}
		}
	}
	return host
}
