package updater

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/app"
	"github.com/nickheyer/icebath/pkg/selfupdate"
)

const (
	appInstallerCheckTimeout = 20 * time.Second
	storeApplyTimeout        = 1830 * time.Second
	descriptorLimit          = 1024 * 1024
	descriptorTimeout        = 30 * time.Second
)

var (
	windowsVersionRE = regexp.MustCompile(`^\d+\.\d+\.\d+\.\d+$`)
	darwinChannelRE  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	reservedNameRE   = regexp.MustCompile(`^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)
)

// Exit code and stdout of one hidden verb run of the bundled icebath binary
type checkerResult struct {
	code   int
	stdout string
}

func checkerFailure(message string) checkerResult {
	body, _ := json.Marshal(map[string]any{"available": nil, "error": message})
	return checkerResult{code: 1, stdout: string(body)}
}

// Runs a hidden update verb of the payload binary, a timeout or spawn failure answered as an unknown result
func runAppInstallerChecker(binary, verb string, args []string, env []string, timeout time.Duration, waitForExit bool, onStderr func(string)) checkerResult {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{verb}, args...)...)
	cmd.Env = env
	hideConsole(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return checkerFailure(err.Error())
	}
	go func() { done <- cmd.Wait() }()
	var deadline <-chan time.Time
	if !waitForExit {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	select {
	case <-deadline:
		return checkerFailure(fmt.Sprintf("checker timed out after %dms", timeout.Milliseconds()))
	case err := <-done:
		if stderr.Len() > 0 && onStderr != nil {
			onStderr(stderr.String())
		}
		if ctx.Err() != nil {
			return checkerFailure(fmt.Sprintf("checker timed out after %dms", timeout.Milliseconds()))
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return checkerResult{code: exitErr.ExitCode(), stdout: stdout.String()}
		}
		if err != nil {
			return checkerFailure(err.Error())
		}
		return checkerResult{code: 0, stdout: stdout.String()}
	}
}

// Environment of the Store verb, Python interpreter pins of the parent removed
func payloadVerbEnv() []string {
	env := os.Environ()
	for _, key := range []string{"PYTHONHOME", "VIRTUAL_ENV", "PYTHONEXECUTABLE", "__PYVENV_LAUNCHER__"} {
		env = withoutEnv(env, key)
	}
	return env
}

// StoreResult is one Microsoft Store verb answer
type storeResult struct {
	available *bool
	packages  []string
	ok        bool
	err       string
}

func parseStoreResult(code int, stdout string) storeResult {
	unknown := func(message string) storeResult { return storeResult{err: message} }
	var value any
	if json.Unmarshal([]byte(stdout), &value) != nil {
		return unknown(fmt.Sprintf("Microsoft Store returned invalid JSON (exit %d)", code))
	}
	row, isObject := value.(map[string]any)
	if !isObject || row == nil {
		return unknown("Microsoft Store returned no result")
	}
	ok, _ := row["ok"].(bool)
	available, availableBool := row["available"].(bool)
	items, isList := row["packages"].([]any)
	packages := []string{}
	allText := isList
	for _, item := range items {
		text, isText := item.(string)
		allText = allText && isText
		packages = append(packages, text)
	}
	if (code != 0 && code != 2) || !ok || !availableBool || !allText {
		if message, isText := row["error"].(string); isText {
			return unknown(message)
		}
		return unknown(fmt.Sprintf("Microsoft Store request failed (exit %d)", code))
	}
	return storeResult{ok: true, available: &available, packages: packages}
}

// The Microsoft Store update owner, the payload binary's Store verb doing check, download and install
type storeStrategy struct {
	service    *Service
	binary     string
	appVersion string
}

func (*storeStrategy) Mechanism() string { return "microsoft-store" }

func (st *storeStrategy) run(mode string) (storeResult, error) {
	handle := st.service.host.WindowHandle()
	if mode != "check" && handle == 0 {
		return storeResult{}, errors.New("A desktop window is required for Microsoft Store consent")
	}
	args := []string{"--mode", mode}
	if handle != 0 {
		args = append(args, "--hwnd", strconv.FormatUint(uint64(handle), 10))
	}
	timeout := appInstallerCheckTimeout
	if mode != "check" {
		timeout = storeApplyTimeout
	}
	result := runAppInstallerChecker(st.binary, app.WindowsStoreUpdateModuleEntry, args, payloadVerbEnv(), timeout, mode != "check", nil)
	if mode != "check" && result.code != 0 {
		failed := parseStoreResult(result.code, result.stdout)
		failed.ok = false
		if failed.err == "" {
			failed.err = fmt.Sprintf("Microsoft Store %s did not complete", mode)
		}
		return failed, nil
	}
	return parseStoreResult(result.code, result.stdout), nil
}

func (st *storeStrategy) Check(bool) (UpdaterStatus, error) {
	result, err := st.run("check")
	if err != nil {
		return UpdaterStatus{}, err
	}
	available := result.ok && result.available != nil && *result.available
	status := UpdaterStatus{Supported: true, Mechanism: st.Mechanism(), CurrentVersion: st.appVersion, UpdateAvailable: &available, FetchedAt: time.Now().UnixMilli()}
	if !result.ok || result.available == nil {
		status.Error = result.err
		if status.Error == "" {
			status.Error = "Microsoft Store check unavailable"
		}
	}
	return status, nil
}

func (st *storeStrategy) Apply() (ApplyResult, error) {
	s := st.service
	relaunch := &packagedRelaunch{
		register: func() (relaunchRegistration, error) {
			return s.registerUpdateRelaunch(st.appVersion, func() (*relaunchWaiter, error) {
				return s.startUpdateRelaunchWaiter(s.opts.StoreMsixIdentityName, 1860)
			})
		},
		onManual: func() error { return errors.New("Could not register automatic relaunch for the Store update") },
	}
	return s.applyPackagedHandoff(s.teardownBundledBackend, s.restoreBundledBackend, relaunch, func(stop func() error) (ApplyResult, error) {
		s.emitUpdateProgress("fetch", "Downloading the update from Microsoft Store.", nil)
		downloaded, err := st.run("download")
		if err != nil {
			return ApplyResult{}, err
		}
		if !downloaded.ok || downloaded.available == nil {
			return ApplyResult{}, errors.New(firstNonEmpty(downloaded.err, "Microsoft Store download did not complete"))
		}
		if !*downloaded.available {
			none := false
			return ApplyResult{OK: true, UpdateAvailable: &none, Mechanism: st.Mechanism()}, nil
		}
		if err := stop(); err != nil {
			return ApplyResult{}, err
		}
		s.emitUpdateProgress("restart", "Microsoft Store is installing the update. Icebath will reopen.", nil)
		installed, err := st.run("install")
		if err != nil {
			return ApplyResult{}, err
		}
		if !installed.ok || installed.available == nil || !*installed.available {
			return ApplyResult{}, errors.New(firstNonEmpty(installed.err, "Microsoft Store did not confirm installation"))
		}
		s.host.Quit()
		return ApplyResult{OK: true, Bundled: true, HandedOff: true, Mechanism: st.Mechanism()}, nil
	})
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// App Installer check answer of the payload's checker verb
type appInstallerCheck struct {
	available *bool
	err       string
	sourceURI string
}

func parseCheckOutput(code int, stdout string) appInstallerCheck {
	var parsed map[string]any
	if strings.TrimSpace(stdout) != "" {
		if json.Unmarshal([]byte(stdout), &parsed) != nil {
			if code != 0 {
				return appInstallerCheck{err: fmt.Sprintf("checker exited %d", code)}
			}
			return appInstallerCheck{err: "checker returned invalid JSON"}
		}
	}
	message, _ := parsed["error"].(string)
	if available, ok := parsed["available"].(bool); ok {
		source, _ := parsed["source_uri"].(string)
		return appInstallerCheck{available: &available, err: message, sourceURI: source}
	}
	if message == "" {
		message = "checker returned no availability"
		if code != 0 {
			message = fmt.Sprintf("checker exited %d", code)
		}
	}
	return appInstallerCheck{err: message}
}

// Feed directory of a Windows App Installer channel, the light variant under its own prefix
func win32AppInstallerFeedPath(channel string, light bool) (string, error) {
	if err := validateDarwinFeedChannel(channel); err != nil {
		return "", err
	}
	variant := ""
	if light {
		variant = "light/"
	}
	return "win32/" + variant + channel + "/", nil
}

func validateDarwinFeedChannel(channel string) error {
	if !darwinChannelRE.MatchString(channel) || len(channel) > 32 || reservedNameRE.MatchString(channel) {
		return errors.New("Invalid channel name")
	}
	return nil
}

// Pinned channel feed an App Installer strategy installs, verified once prepared
type pinnedFeed struct {
	url            string
	version        string
	verifyPrepared func(file string) error
}

// The App Installer update owner of a sideloaded MSIX package
type appInstallerStrategy struct {
	service     *Service
	binary      string
	channel     string
	light       bool
	feedBaseURL string
	appVersion  string
	feed        *pinnedFeed
}

func (*appInstallerStrategy) Mechanism() string { return "app-installer" }

// Strategy bound to a Windows channel target, its feed and publisher pinned
func (a *appInstallerStrategy) pinTo(target *ChannelTarget) (UpdaterStrategy, error) {
	if target.Package.Platform != "win32" {
		return nil, errors.New("Expected Windows channel target")
	}
	pinned := *a
	pinned.channel = target.Channel.Name
	pinned.feedBaseURL = target.Manifest.Request.PublicBase
	pinned.feed = &pinnedFeed{url: target.FeedURL, version: target.Package.Version, verifyPrepared: func(file string) error {
		return a.service.verifyPreparedChannelInstaller(file, target)
	}}
	return &pinned, nil
}

func (a *appInstallerStrategy) runChecker() appInstallerCheck {
	result := runAppInstallerChecker(a.binary, selfupdate.WindowsAppInstallerUpdateEntry, nil, os.Environ(), appInstallerCheckTimeout, false, func(stderr string) {
		fmt.Fprintf(os.Stderr, "[app-installer] checker stderr: %s\n", truncateRunes(stderr, 400))
	})
	return parseCheckOutput(result.code, result.stdout)
}

func truncateRunes(text string, limit int) string {
	units := []rune(text)
	if len(units) > limit {
		return string(units[:limit])
	}
	return text
}

func (a *appInstallerStrategy) Check(bool) (UpdaterStatus, error) {
	status := UpdaterStatus{Supported: true, Mechanism: a.Mechanism(), CurrentVersion: a.appVersion, FetchedAt: time.Now().UnixMilli()}
	if a.feed != nil {
		newer, err := newerWindowsVersion(a.feed.version, a.appVersion)
		if err != nil {
			return UpdaterStatus{}, err
		}
		status.UpdateAvailable = &newer
		return status, nil
	}
	check := a.runChecker()
	status.UpdateAvailable = check.available
	if check.available == nil {
		status.Error = firstNonEmpty(check.err, "update check unavailable")
	}
	return status, nil
}

func (a *appInstallerStrategy) Apply() (ApplyResult, error) {
	s := a.service
	feedBase := a.feedBaseURL
	sourceURI := ""
	if a.feed != nil {
		sourceURI = a.feed.url
	}
	if sourceURI != "" {
		if _, err := channelPublicBase(sourceURI); err != nil {
			return ApplyResult{}, err
		}
		base, err := channelPublicBase(feedBase)
		if err != nil {
			return ApplyResult{}, err
		}
		if !strings.HasPrefix(sourceURI, base+"/") || urlOrigin(sourceURI) != urlOrigin(base) {
			return ApplyResult{}, errors.New("Native feed authority mismatch")
		}
		newer, err := newerWindowsVersion(a.feed.version, a.appVersion)
		if err != nil {
			return ApplyResult{}, err
		}
		if !newer {
			return ApplyResult{OK: true, Mechanism: a.Mechanism()}, nil
		}
	}
	if feedBase == "" && sourceURI == "" {
		sourceURI = a.runChecker().sourceURI
		if sourceURI != "" {
			if _, err := channelPublicBase(sourceURI); err != nil {
				return ApplyResult{}, err
			}
		}
	}
	if feedBase == "" && sourceURI == "" {
		s.emitUpdateProgress("manual", "bundled install: update by installing the new app release", nil)
		return ApplyResult{OK: true, Manual: true, Bundled: true, Mechanism: a.Mechanism()}, nil
	}
	s.emitUpdateProgress("restart", "Applying the Icebath update — the window will close and the App Installer will finish.", percentOf(100))
	relaunch := &packagedRelaunch{
		register: func() (relaunchRegistration, error) {
			return s.registerUpdateRelaunch(a.appVersion, func() (*relaunchWaiter, error) {
				return s.startUpdateRelaunchWaiter(s.opts.MsixAppIDWithOrg, 0)
			})
		},
		onManual: func() error {
			s.emitUpdateProgress("restart", "Automatic relaunch could not be registered. Reopen Icebath after App Installer finishes.", percentOf(100))
			return nil
		},
	}
	return s.applyPackagedHandoff(s.teardownBundledBackend, s.restoreBundledBackend, relaunch, func(stop func() error) (ApplyResult, error) {
		appinstallerURL := sourceURI
		if appinstallerURL == "" {
			feedPath, err := win32AppInstallerFeedPath(a.channel, a.light)
			if err != nil {
				return ApplyResult{}, err
			}
			appinstallerURL = strings.TrimRight(feedBase, "/") + "/" + feedPath + a.channel + ".appinstaller"
		}
		file, err := stageAppInstallerFile(appinstallerURL, filepath.Join(s.opts.UserData, "updates"))
		if err != nil {
			return ApplyResult{}, err
		}
		if a.feed != nil {
			if err := a.feed.verifyPrepared(file); err != nil {
				return ApplyResult{}, err
			}
		}
		if err := stop(); err != nil {
			return ApplyResult{}, err
		}
		if message := s.host.OpenPath(file); message != "" {
			return ApplyResult{}, errors.New("App Installer could not open: " + message)
		}
		s.host.Quit()
		return ApplyResult{OK: true, Bundled: true, HandedOff: true, Mechanism: a.Mechanism()}, nil
	})
}

func urlOrigin(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// Whether a four part Windows package version is newer than current
func newerWindowsVersion(target, current string) (bool, error) {
	parse := func(version string) ([]int, error) {
		if !windowsVersionRE.MatchString(version) {
			return nil, errors.New("Windows channel updates require native numeric versions")
		}
		parts := make([]int, 4)
		for i, part := range strings.Split(version, ".") {
			n, err := strconv.Atoi(part)
			if err != nil || n > 65535 {
				return nil, errors.New("Invalid Windows native version")
			}
			parts[i] = n
		}
		return parts, nil
	}
	left, err := parse(target)
	if err != nil {
		return false, err
	}
	right, err := parse(current)
	if err != nil {
		return false, err
	}
	for i := 0; i < 4; i++ {
		if left[i] != right[i] {
			return left[i] > right[i], nil
		}
	}
	return false, nil
}

// Downloads an App Installer descriptor of at most 1 MiB to directory/update.appinstaller without redirects
func stageAppInstallerFile(rawURL, directory string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), descriptorTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirectClient(0).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("App Installer descriptor download failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, descriptorLimit+1))
	if err != nil {
		return "", err
	}
	if len(body) > descriptorLimit {
		return "", errors.New("App Installer descriptor exceeds 1 MiB")
	}
	if len(body) == 0 {
		return "", errors.New("App Installer descriptor is empty")
	}
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return "", err
	}
	target := filepath.Join(directory, "update.appinstaller")
	temporary := target + ".tmp"
	defer os.Remove(temporary)
	if err := os.WriteFile(temporary, body, 0o666); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, target); err != nil {
		return "", err
	}
	return target, nil
}

// HTTP client refusing redirects, routed through the system proxy as the webview routes it
func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		Transport:     &http.Transport{Proxy: systemProxy},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect mode is set to error") },
	}
}

// Proxy the system resolver picks for a request, direct for DIRECT and on resolver failure
func systemProxy(req *http.Request) (*url.URL, error) {
	answer, err := resolveSystem(req.URL.String())
	if err != nil {
		return nil, nil
	}
	kind, hostPort := pacKind(answer)
	switch kind {
	case "PROXY":
		return url.Parse("http://" + hostPort)
	case "HTTPS":
		return url.Parse("https://" + hostPort)
	case "SOCKS", "SOCKS5":
		return url.Parse("socks5://" + hostPort)
	}
	return nil, nil
}
