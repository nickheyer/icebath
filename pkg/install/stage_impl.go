package install

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/pm"
)

// Install binary of a tree
func InstallBinaryPath(installDir string) string {
	name := "icebath"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(installDir, name)
}

// Release identity an installed binary reports through install-self --release-info
type ReleaseInfo struct {
	Tag    string `json:"tag"`
	Commit string `json:"commit"`
}

// Asks an installed binary which release it is, empty when it cannot answer
func InstalledRelease(ctx context.Context, binary string) ReleaseInfo {
	if !isFile(binary) {
		return ReleaseInfo{}
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "install-self", "--release-info")
	cmd.Dir = filepath.Dir(binary)
	hideConsole(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ReleaseInfo{}
	}
	var info ReleaseInfo
	if json.Unmarshal(bytes.TrimSpace(out), &info) != nil {
		return ReleaseInfo{}
	}
	if !IsFullSHA(info.Commit) {
		info.Commit = ""
	}
	return info
}

func (r *Runner) stagePrerequisites(ctx context.Context) error {
	if err := checkPlatform(); err != nil {
		return err
	}
	parent := filepath.Dir(r.installDir)
	if err := os.MkdirAll(parent, 0o777); err != nil {
		return fail(fmt.Sprintf("cannot create %s: %v", parent, err), "filesystem_error")
	}
	probe, err := os.CreateTemp(parent, ".icebath-install-probe-*")
	if err != nil {
		return fail(fmt.Sprintf("%s is not writable: %v", parent, err), "filesystem_error")
	}
	probe.Close()
	os.Remove(probe.Name())
	goos, goarch := hostKey()
	r.logSuccess(fmt.Sprintf("prerequisites ok (%s-%s)", goos, goarch))
	return nil
}

// Whether stable applies to this run, an explicit channel first then the official main line
func (r *Runner) stableApplies(repo Repo) bool {
	if r.opts.Commit != "" {
		return false
	}
	switch r.opts.Channel {
	case "main":
		return false
	case "stable":
		return true
	}
	return r.opts.Branch == "main" && repo.Official
}

// Release this run installs, nil with no error when a rerun keeps its installed binary
func (r *Runner) resolveTarget(ctx context.Context) (*ReleaseTarget, error) {
	repo, err := ParseRepo(r.opts.RepoURL)
	if err != nil {
		return nil, fail(err.Error(), "git_fetch_failed")
	}
	if r.opts.Commit != "" {
		t, err := releaseForCommit(ctx, repo, r.opts.Commit)
		if err != nil {
			return nil, fail(fmt.Sprintf("could not list releases of %s: %v. Rerun the installer to retry", repo.Web, err), "git_fetch_failed")
		}
		if t == nil {
			return nil, fail(fmt.Sprintf("no published release of %s was built from commit %s", repo.Web, r.opts.Commit), "git_fetch_failed")
		}
		return t, nil
	}
	if r.stableApplies(repo) {
		tag := LatestStableRelease(ctx, repo)
		if tag == "" {
			return nil, r.stableUnreachable("could not look up the latest stable release")
		}
		r.logInfo("Installing the stable release " + tag)
		return targetForTag(ctx, repo, tag), nil
	}
	t, err := releaseForBranch(ctx, repo, r.opts.Branch)
	if err != nil {
		return nil, fail(fmt.Sprintf("could not list releases of %s: %v. Rerun the installer to retry", repo.Web, err), "git_fetch_failed")
	}
	if t == nil {
		return nil, fail(fmt.Sprintf("no published release of %s follows branch %s", repo.Web, r.opts.Branch), "git_fetch_failed")
	}
	r.logInfo(fmt.Sprintf("Installing %s, the newest release built from %s", t.Tag, r.opts.Branch))
	return t, nil
}

// Keeps an installed binary when stable cannot be reached unless stable was asked for by name
func (r *Runner) stableUnreachable(what string) error {
	if _, err := os.Stat(InstallBinaryPath(r.installDir)); err == nil && r.opts.Channel != "stable" {
		r.logWarn(what + "; keeping the installed binary where it is")
		return nil
	}
	return fail(what+". Rerun the installer to retry, or pass --channel main to install main's newest release", "git_fetch_failed")
}

// Fetches one release asset to dest verified against its sha256, a status line tracking progress on a terminal
func (r *Runner) download(ctx context.Context, label, url, dest, sum string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o777); err != nil {
		return fail(fmt.Sprintf("cannot create %s: %v", filepath.Dir(dest), err), "filesystem_error")
	}
	d := pm.NewDownload([]pm.Source{{URL: url, Dest: dest, SHA256: sum}}, pm.DownloadOptions{PartialsDir: filepath.Join(r.home, "bootstrap-cache", "partials")})
	stop := context.AfterFunc(ctx, d.Pause)
	defer stop()
	if !r.quiet {
		r.logInfo(label)
	}
	last := time.Time{}
	_, err := d.Run(func(done, total int64, _ []pm.RangeSet) error {
		if !r.quiet || time.Since(last) < 200*time.Millisecond {
			return nil
		}
		last = time.Now()
		pct := ""
		if total > 0 {
			pct = fmt.Sprintf(": %d%%", done*100/total)
		}
		fmt.Fprintf(r.stdout, "\r\x1b[K  %s%s", label, pct)
		return nil
	})
	if r.quiet {
		fmt.Fprint(r.stdout, "\r\x1b[K")
	}
	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fail("cancelled by user", "interrupted")
	case isDigestMismatch(err):
		return fail(fmt.Sprintf("%s digest mismatch from %s (expected %s): %v", filepath.Base(dest), url, sum, err), "download_digest_mismatch")
	}
	return fail(fmt.Sprintf("failed to download %s: %v", url, err), "download_failed")
}

// Places the release binary in the install dir and stamps the install method beside it
func (r *Runner) stageRepository(ctx context.Context) error {
	if err := r.placeBinary(ctx); err != nil {
		return err
	}
	if r.opts.InstallMethod == "" {
		return nil
	}
	if err := WriteInstallMethod(r.installDir, r.opts.InstallMethod); err != nil {
		return fail(fmt.Sprintf("cannot stamp the install method in %s: %v", r.installDir, err), "filesystem_error")
	}
	return nil
}

func (r *Runner) placeBinary(ctx context.Context) error {
	dest := InstallBinaryPath(r.installDir)
	if err := os.MkdirAll(r.installDir, 0o777); err != nil {
		return fail(fmt.Sprintf("cannot create %s: %v", r.installDir, err), "filesystem_error")
	}
	if r.opts.Binary != "" {
		if _, err := os.Stat(r.opts.Binary); err != nil {
			return fail(fmt.Sprintf("binary %s is missing: %v", r.opts.Binary, err), "download_failed")
		}
		if !samePath(r.opts.Binary, dest) {
			if err := copyFile(r.opts.Binary, dest, 0o755); err != nil {
				return fail(fmt.Sprintf("cannot place %s: %v", dest, err), "filesystem_error")
			}
		}
		tag := r.opts.Version
		if tag == "" {
			tag = InstalledRelease(ctx, dest).Tag
		}
		r.logSuccess(fmt.Sprintf("Icebath Agent %s installed at %s", displayTag(tag), dest))
		return nil
	}
	t, err := r.resolveTarget(ctx)
	if err != nil || t == nil {
		return err
	}
	goos, goarch := hostKey()
	name := BinaryAssetName(goos, goarch)
	sum, err := t.ExpectedSHA256(ctx, name)
	if err != nil {
		if errors.As(err, new(errDigestMismatch)) {
			return fail(err.Error(), "download_digest_mismatch")
		}
		return fail(err.Error(), "download_failed")
	}
	staged := dest + ".download"
	if err := r.download(ctx, fmt.Sprintf("Downloading Icebath Agent %s (%s-%s)", t.Tag, goos, goarch), t.AssetURL(name), staged, sum); err != nil {
		os.Remove(staged)
		return err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return fail(fmt.Sprintf("cannot mark %s executable: %v", staged, err), "filesystem_error")
	}
	if err := replaceFile(staged, dest); err != nil {
		return fail(fmt.Sprintf("cannot place %s: %v", dest, err), "filesystem_error")
	}
	r.logSuccess(fmt.Sprintf("Icebath Agent %s installed at %s", t.Tag, dest))
	return nil
}

func displayTag(tag string) string {
	if tag == "" {
		return "(unreleased build)"
	}
	return tag
}

func (r *Runner) stageConfig(ctx context.Context) error {
	for _, sub := range []string{"cron", "sessions", "logs", "pairing", "hooks", "image_cache", "audio_cache", "memories", "skills"} {
		if err := os.MkdirAll(filepath.Join(r.home, sub), 0o777); err != nil {
			return fail(fmt.Sprintf("cannot create %s: %v", filepath.Join(r.home, sub), err), "filesystem_error")
		}
	}
	env := filepath.Join(r.home, ".env")
	if _, err := os.Stat(env); err != nil {
		if copyFile(filepath.Join(r.installDir, ".env.example"), env, 0o600) != nil {
			f, err := os.OpenFile(env, os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return fail(fmt.Sprintf("cannot create %s: %v", env, err), "filesystem_error")
			}
			f.Close()
		}
	}
	if err := os.Chmod(env, 0o600); err != nil {
		return fail(fmt.Sprintf("cannot restrict %s: %v", env, err), "filesystem_error")
	}
	cfg := filepath.Join(r.home, "config.yaml")
	example := filepath.Join(r.installDir, "cli-config.yaml.example")
	if _, err := os.Stat(cfg); err != nil {
		if _, err := os.Stat(example); err == nil {
			if err := copyFile(example, cfg, 0o644); err != nil {
				return fail(fmt.Sprintf("cannot create %s: %v", cfg, err), "filesystem_error")
			}
		}
	}
	var declined []string
	if r.opts.SkipBrowser {
		declined = append(declined, pm.CanonicalDefaultName("agent-browser"))
	}
	if r.opts.SkipComputerUse {
		declined = append(declined, "cua-driver")
	}
	if len(declined) > 0 {
		if _, err := pm.RecordDeclined(ctx, declined, nil, r.installDir); err != nil {
			return fail(fmt.Sprintf("could not record the opt-outs %s: %v", strings.Join(declined, ", "), err), "filesystem_error")
		}
		for _, name := range declined {
			r.logSuccess(fmt.Sprintf("%s left out of default installs; undo with 'icebath pm install %s'", name, name))
		}
	}
	r.logSuccess("config prepared in " + r.home)
	return nil
}

func (r *Runner) stageProducts(ctx context.Context) error {
	desktop := r.opts.IncludeDesktop || DesktopProductPresent(r.installDir)
	if _, err := os.Stat(InstallBinaryPath(r.installDir)); err != nil {
		return fail("app products or command publication failed: no icebath binary in "+r.installDir, "products_build_failed")
	}
	res, err := pm.ExposeCLI(ctx, r.installDir, true)
	if err != nil {
		return fail("app products or command publication failed: "+err.Error(), "products_build_failed")
	}
	if !res.OK {
		return fail("app products or command publication failed: "+res.Error, "products_build_failed")
	}
	for _, name := range res.Written {
		r.logSuccess("published " + name)
	}
	if desktop {
		if err := r.installDesktop(ctx); err != nil {
			return err
		}
	}
	if err := r.wireShellPath(); err != nil {
		return err
	}
	r.logSuccess("app products and icebath command ready")
	return nil
}

// Opens the controlling terminal for an interactive child, false where none can be opened
func openTerminal() (*os.File, bool) {
	name := "/dev/tty"
	if runtime.GOOS == "windows" {
		name = "CONIN$"
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, false
	}
	return f, true
}

// Runs the installed binary with the terminal as its stdin and the runner streams as its output
func (r *Runner) runInteractive(tty *os.File, args ...string) error {
	cmd := exec.Command(InstallBinaryPath(r.installDir), args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, r.stdout, r.stderr
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+r.home)
	return cmd.Run()
}

// Provisions the tool runtimes through the installed command's pm install, honouring the browser and computer use opt outs
func (r *Runner) stageRuntimes(ctx context.Context) error {
	args := []string{"pm", "install"}
	if r.opts.SkipBrowser {
		args = append(args, "--without", "agent-browser")
	}
	if r.opts.SkipComputerUse {
		args = append(args, "--without", "cua-driver")
	}
	r.logInfo("Installing dependencies (hash-verified)")
	cmd := exec.CommandContext(ctx, InstallBinaryPath(r.installDir), args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = r.installDir, r.stdout, r.stderr
	cmd.Env = append(os.Environ(), "ICEBATH_HOME="+r.home)
	if err := cmd.Run(); err != nil {
		return fail("pm install failed", "deps_install_failed")
	}
	r.logSuccess("dependencies installed")
	return nil
}

func (r *Runner) stageSetup(ctx context.Context) error {
	tty, ok := openTerminal()
	if !ok {
		r.logInfo("setup skipped (no terminal); run 'icebath setup' after install")
		return nil
	}
	defer tty.Close()
	if err := r.runInteractive(tty, "setup"); err != nil {
		return fail("setup failed", "setup_failed")
	}
	return nil
}

func (r *Runner) stageGateway(ctx context.Context) error {
	tty, ok := openTerminal()
	if !ok {
		r.logInfo("gateway setup skipped (no terminal); run 'icebath gateway install' after install")
		return nil
	}
	defer tty.Close()
	if err := r.runInteractive(tty, "gateway", "install", "--if-missing"); err != nil {
		return fail("gateway installation failed", "gateway_failed")
	}
	return nil
}

func (r *Runner) stageComplete(ctx context.Context) error {
	commit := r.opts.Commit
	if commit == "" {
		commit = InstalledRelease(ctx, InstallBinaryPath(r.installDir)).Commit
	}
	if commit != "" {
		branch := r.opts.Branch
		if _, err := writeMarker(r.installDir, &commit, &branch, time.Now().UTC().Truncate(time.Second)); err != nil {
			return fail(err.Error(), "filesystem_error")
		}
	}
	if r.opts.Channel != "" {
		cmd := exec.CommandContext(ctx, InstallBinaryPath(r.installDir), "update", "--set-channel", r.opts.Channel)
		cmd.Env = append(os.Environ(), "ICEBATH_HOME="+r.home)
		if out, err := cmd.CombinedOutput(); err != nil {
			for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
				if line != "" {
					r.line(Stderr, line)
				}
			}
			return fail(fmt.Sprintf("could not save the %s update channel", r.opts.Channel), "other")
		}
	}
	r.logSuccess("Icebath Agent install complete. Run: icebath")
	return nil
}
