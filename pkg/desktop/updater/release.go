package updater

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const (
	releaseDownloadTimeout = 30 * time.Minute
	feedReadTimeout        = 30 * time.Second
	stagingIDFile          = ".updaterId"
)

var (
	feedChannelNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	uuidRE            = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// The generic update feed descriptor electron-builder publishes as channel-mac.yml
type feedDescriptor struct {
	Version string `yaml:"version"`
	Files   []struct {
		URL    string `yaml:"url"`
		SHA512 string `yaml:"sha512"`
		Size   int64  `yaml:"size"`
	} `yaml:"files"`
	Path                 string   `yaml:"path"`
	SHA512               string   `yaml:"sha512"`
	StagingPercentage    *float64 `yaml:"stagingPercentage"`
	MinimumSystemVersion string   `yaml:"minimumSystemVersion"`
}

// One release the desktop app can move to, its archive and the digest that proves it
type releaseCandidate struct {
	version  string
	url      string
	sum      string
	hashKind string
	size     int64
	name     string
	descr    *feedDescriptor
}

// The self update of the packaged desktop app from its release feed, the archive swapped over the running app
type releaseStrategy struct {
	service         *Service
	channel         string
	light           bool
	feedBaseURL     string
	appVersion      string
	feedURL         string
	feedChannel     string
	expectedVersion string
	verifyDownload  func(files []string) error

	mu       sync.Mutex
	applying bool
}

func (*releaseStrategy) Mechanism() string { return "electron-updater" }

// Strategy bound to a macOS channel target, its feed descriptor and artifact pinned
func (r *releaseStrategy) pinTo(target *ChannelTarget) (UpdaterStrategy, error) {
	if target.Package.Platform != "darwin" {
		return nil, errors.New("Expected macOS channel target")
	}
	pinned := &releaseStrategy{service: r.service, channel: target.Channel.Name, light: r.light, feedBaseURL: target.Manifest.Request.PublicBase,
		appVersion: r.appVersion, feedURL: target.FeedURL, feedChannel: target.Package.FeedChannel, expectedVersion: target.Package.Version,
		verifyDownload: func(files []string) error { return verifyChannelDownload(files, target.Package.Artifact) }}
	if _, err := pinned.feedDirectory(); err != nil {
		return nil, err
	}
	return pinned, nil
}

// Feed directory and descriptor name, empty when the app reads its release from GitHub
func (r *releaseStrategy) feedDirectory() (string, error) {
	if err := validateDarwinFeedChannel(r.channel); err != nil {
		return "", err
	}
	channel := r.channel
	if r.feedURL != "" {
		channel = r.feedChannel
		base, err := channelPublicBase(r.feedBaseURL)
		if err != nil {
			return "", err
		}
		checked, err := channelPublicBase(r.feedURL)
		if err != nil {
			return "", err
		}
		if !strings.HasPrefix(r.feedURL, base+"/") || urlOrigin(checked) != urlOrigin(base) {
			return "", errors.New("Native feed authority mismatch")
		}
		parsed, _ := url.Parse(checked)
		if !feedChannelNameRE.MatchString(channel) || !strings.HasSuffix(parsed.Path, "/"+channel+"-mac.yml") {
			return "", errors.New("Invalid macOS feed descriptor")
		}
		return checked[:strings.LastIndex(checked, "/")+1], nil
	}
	if r.feedBaseURL == "" {
		return "", nil
	}
	base, err := channelPublicBase(r.feedBaseURL)
	if err != nil {
		return "", err
	}
	light := ""
	if r.light {
		light = "light/"
	}
	return base + "/releases/darwin/" + light + channel + "/", nil
}

func (r *releaseStrategy) descriptorName() string {
	if r.feedURL != "" {
		return r.feedChannel + "-mac.yml"
	}
	return r.channel + "-mac.yml"
}

// Version the feed compares against, zero for a pinned channel feed whose sequence decides availability
func (r *releaseStrategy) currentVersion() string {
	if r.feedURL != "" {
		return "0.0.0"
	}
	return r.appVersion
}

func (r *releaseStrategy) Check(bool) (UpdaterStatus, error) {
	r.mu.Lock()
	applying := r.applying
	r.mu.Unlock()
	if applying {
		return UpdaterStatus{}, errors.New("An update is already in progress.")
	}
	status, _, err := r.checkRelease()
	return status, err
}

// Reads the release the channel offers and whether it is newer than the running app
func (r *releaseStrategy) checkRelease() (UpdaterStatus, *releaseCandidate, error) {
	candidate, err := r.latest()
	if err != nil {
		r.service.log("macOS updater: " + err.Error())
		return UpdaterStatus{}, nil, err
	}
	if r.expectedVersion != "" && candidate.version != r.expectedVersion {
		return UpdaterStatus{}, nil, errors.New("Native macOS feed does not match the pinned channel version")
	}
	available, err := r.isUpdateAvailable(candidate)
	if err != nil {
		return UpdaterStatus{}, nil, err
	}
	latest := "v" + candidate.version
	return UpdaterStatus{Supported: true, Mechanism: r.Mechanism(), CurrentVersion: r.appVersion, Channel: r.channel, LatestTag: &latest, UpdateAvailable: &available, FetchedAt: time.Now().UnixMilli()}, candidate, nil
}

// Newest release of the feed, or of the GitHub releases when no feed is configured
func (r *releaseStrategy) latest() (*releaseCandidate, error) {
	directory, err := r.feedDirectory()
	if err != nil {
		return nil, err
	}
	if directory == "" {
		return r.latestGitHub()
	}
	ctx, cancel := context.WithTimeout(context.Background(), feedReadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, directory+r.descriptorName(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := noRedirectClient(0).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("Cannot find channel %q update info: HttpError: %d", r.descriptorName(), resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, channelReadLimit))
	if err != nil {
		return nil, err
	}
	var descriptor feedDescriptor
	if err := yaml.Unmarshal(body, &descriptor); err != nil {
		return nil, fmt.Errorf("Cannot parse update info from %s: %w", r.descriptorName(), err)
	}
	if descriptor.Version == "" {
		return nil, errors.New("Update info has no version")
	}
	for _, file := range descriptor.Files {
		if strings.HasSuffix(file.URL, ".zip") {
			resolved, err := url.Parse(directory)
			if err != nil {
				return nil, err
			}
			ref, err := url.Parse(file.URL)
			if err != nil {
				return nil, err
			}
			return &releaseCandidate{version: descriptor.Version, url: resolved.ResolveReference(ref).String(), sum: file.SHA512, hashKind: "sha512", size: file.Size, name: filepath.Base(ref.Path), descr: &descriptor}, nil
		}
	}
	return nil, errors.New("ZIP file not provided: " + descriptor.Path)
}

func (r *releaseStrategy) latestGitHub() (*releaseCandidate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), feedReadTimeout)
	defer cancel()
	repo, err := install.ParseRepo("https://github.com/" + selfupdate.OfficialRepository)
	if err != nil {
		return nil, err
	}
	target, err := install.LatestDesktopRelease(ctx, repo, r.channel)
	if err != nil {
		return nil, err
	}
	name := install.DesktopAssetName(runtime.GOOS, runtime.GOARCH)
	sum, err := target.ExpectedSHA256(ctx, name)
	if err != nil {
		return nil, err
	}
	return &releaseCandidate{version: strings.TrimPrefix(target.Tag, "v"), url: target.AssetURL(name), sum: sum, hashKind: "sha256", name: name}, nil
}

// Whether the candidate is newer than the running version, staging and minimum system version honoured
func (r *releaseStrategy) isUpdateAvailable(candidate *releaseCandidate) (bool, error) {
	latest, err := semver.StrictNewVersion(candidate.version)
	if err != nil {
		return false, fmt.Errorf("This file could not be downloaded, or the latest version (from update server) does not have a valid semver version: %q", candidate.version)
	}
	current, err := semver.NewVersion(r.currentVersion())
	if err != nil {
		return false, err
	}
	if latest.Equal(current) {
		return false, nil
	}
	if candidate.descr != nil && candidate.descr.MinimumSystemVersion != "" {
		if minimum, err := semver.NewVersion(candidate.descr.MinimumSystemVersion); err == nil {
			if osVersion, err := semver.NewVersion(kernelRelease()); err == nil && osVersion.LessThan(minimum) {
				r.service.log(fmt.Sprintf("macOS updater: current OS version %s is less than the minimum OS version required %s", osVersion, minimum))
				return false, nil
			}
		}
	}
	if candidate.descr != nil && candidate.descr.StagingPercentage != nil {
		if !r.isStagingMatch(*candidate.descr.StagingPercentage) {
			return false, nil
		}
	}
	return latest.GreaterThan(current), nil
}

// Whether this install falls inside a staged rollout, by the persisted staging id
func (r *releaseStrategy) isStagingMatch(percentage float64) bool {
	file := filepath.Join(r.service.opts.UserData, stagingIDFile)
	id := ""
	if raw, err := os.ReadFile(file); err == nil && uuidRE.MatchString(strings.TrimSpace(string(raw))) {
		id = strings.TrimSpace(string(raw))
	}
	if id == "" {
		var b [16]byte
		_, _ = rand.Read(b[:])
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		id = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
		_ = os.WriteFile(file, []byte(id), 0o666)
	}
	raw, _ := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	value := binary.BigEndian.Uint32(raw[12:16])
	userPercentage := float64(value) / 0xffffffff * 100
	return userPercentage < percentage
}

func (r *releaseStrategy) Apply() (ApplyResult, error) {
	r.mu.Lock()
	if r.applying {
		r.mu.Unlock()
		return ApplyResult{}, errors.New("An update is already in progress.")
	}
	r.applying = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.applying = false
		r.mu.Unlock()
	}()
	s := r.service
	return s.applyPackagedHandoff(s.teardownBundledBackend, s.restoreBundledBackend, nil, func(stop func() error) (ApplyResult, error) {
		status, candidate, err := r.checkRelease()
		if err != nil {
			return ApplyResult{}, err
		}
		if !*status.UpdateAvailable {
			return ApplyResult{OK: true, Mechanism: r.Mechanism()}, nil
		}
		archive, err := r.download(candidate)
		if err != nil {
			return ApplyResult{}, err
		}
		if r.verifyDownload != nil {
			if err := r.verifyDownload([]string{archive}); err != nil {
				return ApplyResult{}, err
			}
		}
		s.emitUpdateProgress("prepare", "Verifying the signed macOS update.", nil)
		staged, target, err := r.prepareInstall(archive)
		if err != nil {
			return ApplyResult{}, err
		}
		if err := stop(); err != nil {
			os.RemoveAll(filepath.Dir(staged))
			return ApplyResult{}, err
		}
		s.emitUpdateProgress("restart", "Restarting Icebath to install the update.", percentOf(100))
		if err := r.quitAndInstall(staged, target); err != nil {
			return ApplyResult{}, err
		}
		return ApplyResult{OK: true, Bundled: true, HandedOff: true, Mechanism: r.Mechanism()}, nil
	})
}

// Downloads the release archive into the updates directory, reporting percent and checking its digest
func (r *releaseStrategy) download(candidate *releaseCandidate) (string, error) {
	directory := filepath.Join(r.service.opts.UserData, "updates")
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return "", err
	}
	file := filepath.Join(directory, candidate.name)
	ctx, cancel := context.WithTimeout(context.Background(), releaseDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, candidate.url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: systemProxy}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("Cannot download %q, status %d", candidate.url, resp.StatusCode)
	}
	total := resp.ContentLength
	if candidate.size > 0 {
		total = candidate.size
	}
	partial := file + ".partial"
	handle, err := os.Create(partial)
	if err != nil {
		return "", err
	}
	var digest hash.Hash = sha256.New()
	if candidate.hashKind == "sha512" {
		digest = sha512.New()
	}
	var done int64
	buffer := make([]byte, 256*1024)
	lastPercent := -1.0
	for {
		n, readErr := resp.Body.Read(buffer)
		if n > 0 {
			if _, err := handle.Write(buffer[:n]); err != nil {
				handle.Close()
				os.Remove(partial)
				return "", err
			}
			digest.Write(buffer[:n])
			done += int64(n)
			if total > 0 {
				if percent := float64(done) * 100 / float64(total); percent-lastPercent >= 1 || done == total {
					lastPercent = percent
					r.service.emitUpdateProgress("fetch", "Downloading the Icebath update.", percentOf(percent))
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			handle.Close()
			os.Remove(partial)
			return "", readErr
		}
	}
	if err := handle.Close(); err != nil {
		os.Remove(partial)
		return "", err
	}
	got := hex.EncodeToString(digest.Sum(nil))
	if candidate.hashKind == "sha512" {
		got = base64.StdEncoding.EncodeToString(digest.Sum(nil))
	}
	if got != candidate.sum {
		os.Remove(partial)
		return "", fmt.Errorf("%s checksum mismatch, expected %s, got: %s", candidate.hashKind, candidate.sum, got)
	}
	if err := os.Rename(partial, file); err != nil {
		return "", err
	}
	return file, nil
}

// Directory the running app occupies, the .app bundle on macOS and the executable's directory elsewhere
func runningAppRoot() (string, error) {
	if isMacOS() {
		if bundle := runningAppBundle(); bundle != "" {
			return bundle, nil
		}
		return "", errors.New("The macOS updater is not active for this app.")
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// Unpacks the archive beside the running app and checks the new build is signed by the same team on macOS
func (r *releaseStrategy) prepareInstall(archive string) (string, string, error) {
	target, err := runningAppRoot()
	if err != nil {
		return "", "", err
	}
	stagingRoot, err := os.MkdirTemp(filepath.Dir(target), "."+filepath.Base(target)+".new-")
	if err != nil {
		return "", "", err
	}
	if err := pm.Extract(archive, stagingRoot); err != nil {
		os.RemoveAll(stagingRoot)
		return "", "", err
	}
	staged := stagingRoot
	if isMacOS() {
		matches, _ := filepath.Glob(filepath.Join(stagingRoot, "*.app"))
		if len(matches) != 1 {
			os.RemoveAll(stagingRoot)
			return "", "", errors.New("The update archive holds no single app bundle")
		}
		staged = matches[0]
		if err := verifySameSigner(target, staged); err != nil {
			os.RemoveAll(stagingRoot)
			return "", "", err
		}
	}
	return staged, target, nil
}

// Refuses a staged bundle whose code signature is broken or names another team than the running app
func verifySameSigner(running, staged string) error {
	if _, _, err := runChannelCommand("/usr/bin/codesign", []string{"--verify", "--deep", "--strict", staged}, channelCommandTimeout, ""); err != nil {
		return err
	}
	team := func(app string) (string, error) {
		_, stderr, err := runChannelCommand("/usr/bin/codesign", []string{"-dv", "--verbose=4", app}, channelCommandTimeout, "")
		if err != nil {
			return "", err
		}
		if m := teamIdentifierRE.FindStringSubmatch(stderr); m != nil {
			return m[1], nil
		}
		return "", nil
	}
	want, err := team(running)
	if err != nil {
		return err
	}
	got, err := team(staged)
	if err != nil {
		return err
	}
	if want == "" || got != want {
		return errors.New("Code signature at URL file://" + staged + " did not pass validation: the update is not signed by the running app's team")
	}
	return nil
}

// Swaps the staged build over the running app, then relaunches into it and quits
func (r *releaseStrategy) quitAndInstall(staged, target string) error {
	stagingRoot := staged
	if isMacOS() {
		stagingRoot = filepath.Dir(staged)
	}
	defer os.RemoveAll(stagingRoot)
	old := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".old")
	os.RemoveAll(old)
	if err := install.SwapInNewBundle(staged, target, old); err != nil {
		return err
	}
	var args []string
	if len(r.service.opts.Args) > 1 {
		args = append(args, r.service.opts.Args[1:]...)
	}
	if err := r.service.host.Relaunch(args); err != nil {
		return err
	}
	r.service.host.Quit()
	return nil
}
