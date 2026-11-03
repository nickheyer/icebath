package plugin

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/shell"
)

// Entries a copied plugin directory may hold at most
const copyEntries = 20000

// Runs git with the arguments in the directory, returning its output
func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	var out, errs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errs
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errs.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Clones or updates a repository into the directory at the ref, a sparse checkout when paths are given
func cloneOrUpdate(ctx context.Context, repoURL, ref, sha, dir string, sparse []string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", errors.New("git is required to fetch repositories and is not on the path")
	}
	fresh := !exists(filepath.Join(dir, ".git"))
	if fresh {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		os.RemoveAll(dir)
		args := []string{"clone", "--quiet", "--filter=blob:none"}
		if len(sparse) > 0 {
			args = append(args, "--sparse")
		}
		if ref != "" && sha == "" {
			args = append(args, "--branch", ref)
		}
		args = append(args, repoURL, dir)
		if _, err := git(ctx, "", args...); err != nil {
			return "", err
		}
		if len(sparse) > 0 {
			if _, err := git(ctx, dir, append([]string{"sparse-checkout", "set", "--no-cone"}, sparse...)...); err != nil {
				return "", err
			}
		}
	} else {
		if _, err := git(ctx, dir, "fetch", "--quiet", "--all", "--tags", "--prune"); err != nil {
			return "", err
		}
		target := "origin/HEAD"
		switch {
		case sha != "":
			target = sha
		case ref != "":
			if _, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "origin/"+ref); err == nil {
				target = "origin/" + ref
			} else {
				target = ref
			}
		}
		if _, err := git(ctx, dir, "checkout", "--quiet", "--force", target); err != nil {
			return "", err
		}
	}
	if sha != "" {
		if _, err := git(ctx, dir, "checkout", "--quiet", "--force", sha); err != nil {
			return "", err
		}
	}
	return git(ctx, dir, "rev-parse", "HEAD")
}

// Commit of the last change under a path of a repository, empty outside one
func commitOf(ctx context.Context, dir, path string) string {
	out, err := git(ctx, dir, "log", "-1", "--format=%H", "--", path)
	if err != nil {
		return ""
	}
	return out
}

// Git url of a GitHub repository
func githubURL(repo string) string { return "https://github.com/" + repo + ".git" }

// Downloads a url into memory with the headers, refusing loopback, link local and metadata hosts
func download(ctx context.Context, rawURL string, headers map[string]string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("%s is not https", rawURL)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return nil, fmt.Errorf("%s points at a local address", rawURL)
	}
	if host == "localhost" || host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") {
		return nil, fmt.Errorf("%s points at a local or metadata host", rawURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, fmt.Errorf("%s returned status %d", rawURL, res.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", rawURL, limit)
	}
	return data, nil
}

// Downloads an archive, checks its digest and extracts it into the directory, the plugin root being the top or one down
func fetchArchive(ctx context.Context, src Source, dir string, headers map[string]string) (string, error) {
	data, err := download(ctx, src.URL, headers, bundleLimit)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), src.SHA256) {
		return "", fmt.Errorf("archive %s has digest %s, the catalog expects %s", src.URL, hex.EncodeToString(sum[:]), src.SHA256)
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := dir + ".archive"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	switch {
	case bytes.HasPrefix(data, []byte("PK")):
		if err := extractZip(tmp, dir); err != nil {
			return "", err
		}
	default:
		if err := extractTarGz(bytes.NewReader(data), dir, 0); err != nil {
			return "", err
		}
	}
	os.Remove(filepath.Join(dir, ".extracted"))
	return pluginRootIn(dir), nil
}

// Root of a plugin inside an extracted directory, the directory or its one subdirectory
func pluginRootIn(dir string) string {
	if hasManifest(dir) {
		return dir
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") || e.IsDir() && e.Name() == ".claude-plugin" {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 1 && hasManifest(filepath.Join(dir, dirs[0])) {
		return filepath.Join(dir, dirs[0])
	}
	if len(dirs) == 1 && !hasManifest(dir) {
		return filepath.Join(dir, dirs[0])
	}
	return dir
}

// Reports whether a directory holds a manifest or any default component folder
func hasManifest(dir string) bool {
	for _, p := range []string{ClaudeManifest, CodexManifest, PortableManifest, skillsDir, commandsDir, agentsDir, hooksFile, serversFile, "SKILL.md"} {
		if exists(filepath.Join(dir, p)) {
			return true
		}
	}
	return false
}

// Extracts a gzipped tar into the directory, stripping leading path parts and refusing escaping entries
func extractTarGz(r io.Reader, dir string, strip int) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.FromSlash(h.Name)
		parts := strings.Split(filepath.ToSlash(name), "/")
		if len(parts) <= strip {
			continue
		}
		name = filepath.Join(parts[strip:]...)
		target := filepath.Join(dir, name)
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("entry %q escapes the directory", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode).Perm()|0o600)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, bundleLimit-total+1))
			out.Close()
			total += n
			if total > bundleLimit {
				return errors.New("archive is larger than 256 MiB")
			}
			if err != nil {
				return err
			}
		}
	}
}

// Metadata of an npm package version, its tarball and digest
type npmVersion struct {
	Version string `json:"version"`
	Dist    struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
		SHA1      string `json:"shasum"`
	} `json:"dist"`
}

// Hosts npm tarball links are refused from
var refusedTarballHosts = []string{"github.com", "gist.github.com", "gitlab.com", "bitbucket.org", "git.sr.ht"}

// Downloads an npm package version from its registry and extracts it, never running its scripts
func fetchNPM(ctx context.Context, src Source, dir string) (string, string, error) {
	pkg, version := src.Package, src.Version
	if strings.HasPrefix(pkg, "git") || strings.HasPrefix(pkg, "file:") || strings.HasPrefix(pkg, "npm:") {
		return "", "", fmt.Errorf("npm package %q must be a registry package", pkg)
	}
	var tarball string
	switch {
	case strings.HasPrefix(pkg, "https://"):
		u, err := url.Parse(pkg)
		if err != nil {
			return "", "", err
		}
		for _, host := range refusedTarballHosts {
			if u.Hostname() == host && !strings.HasPrefix(pkg, "https://gitlab.com/api/v4/") {
				return "", "", fmt.Errorf("npm tarball links on %s are refused", host)
			}
		}
		tarball = pkg
		version = "unknown"
	default:
		if i := strings.LastIndex(pkg, "@"); i > 0 {
			pkg, version = pkg[:i], pkg[i+1:]
		}
		if version == "" {
			version = "latest"
		}
		registry := src.Registry
		if registry == "" {
			registry = "https://registry.npmjs.org"
		}
		meta, err := download(ctx, strings.TrimSuffix(registry, "/")+"/"+strings.ReplaceAll(pkg, "/", "%2F")+"/"+url.PathEscape(version), nil, 4<<20)
		if err != nil {
			if strings.ContainsAny(version, "^~><=*x") {
				return "", "", fmt.Errorf("npm version range %q: %w, give a version or a dist tag", version, err)
			}
			return "", "", err
		}
		var v npmVersion
		if err := json.Unmarshal(meta, &v); err != nil {
			return "", "", fmt.Errorf("npm %s: %w", pkg, err)
		}
		if v.Dist.Tarball == "" {
			return "", "", fmt.Errorf("npm %s@%s names no tarball", pkg, version)
		}
		tarball, version = v.Dist.Tarball, v.Version
	}
	if !strings.HasPrefix(tarball, "https://") {
		return "", "", fmt.Errorf("npm tarball %s is not https", tarball)
	}
	data, err := download(ctx, tarball, nil, bundleLimit)
	if err != nil {
		return "", "", err
	}
	os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	if err := extractTarGz(bytes.NewReader(data), dir, 1); err != nil {
		return "", "", err
	}
	return dir, version, nil
}

// Runs the command of a command source from the home directory and reads the directory it prints
func runCommandSource(ctx context.Context, src Source) (string, error) {
	timeout := time.Duration(src.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	home, _ := os.UserHomeDir()
	var out, errs bytes.Buffer
	code, err := shell.Cmd{Line: src.Command, Dir: home, Stdout: &out, Stderr: &errs}.Run(cctx)
	if err != nil {
		return "", fmt.Errorf("command source: %w", err)
	}
	if code != 0 {
		return "", fmt.Errorf("command source exited %d: %s", code, strings.TrimSpace(errs.String()))
	}
	path := strings.TrimSpace(out.String())
	if path == "" || !filepath.IsAbs(path) || strings.Contains(path, "\n") {
		return "", fmt.Errorf("command source must print one absolute directory path, got %q", path)
	}
	if !isDir(path) {
		return "", fmt.Errorf("command source printed %s, which is not a directory", path)
	}
	return path, nil
}

// Copies a directory tree, following no symlinks out of it, refusing one over the entry or size limit
func copyTree(src, dst string) error {
	os.RemoveAll(dst)
	entries, total := 0, int64(0)
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if d.Name() == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		entries++
		if entries > copyEntries {
			return fmt.Errorf("%s holds more than %d entries", src, copyEntries)
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			resolved := link
			if !filepath.IsAbs(link) {
				resolved = filepath.Join(filepath.Dir(path), link)
			}
			if r, err := filepath.Rel(src, filepath.Clean(resolved)); err != nil || strings.HasPrefix(r, "..") {
				return nil
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			total += info.Size()
			if total > bundleLimit {
				return fmt.Errorf("%s is larger than 256 MiB", src)
			}
			return copyFile(path, target, info.Mode().Perm())
		}
		return nil
	})
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode|0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
