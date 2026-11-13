package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

const pendingRelaunchFile = "pending-update-relaunch.json"

// Connections is the connections group surface a local update apply reads
type Connections interface {
	ManagedUpdates
	// Whether one remote gateway serves this desktop, an env remote URL or a remote, cloud or SSH mode
	GlobalRemoteActive() bool
}

// Joins the connections service an apply waits on and the first run gate the bootstrap calls reset
func (s *Service) AttachConnections(c Connections, gate FirstRunGate) {
	s.conns = c
	s.firstRun = gate
}

// Errors gathered by one failing step, reported under one message as AggregateError does
type aggregateError struct {
	message string
	errs    []error
}

func (e *aggregateError) Error() string   { return e.message }
func (e *aggregateError) Unwrap() []error { return e.errs }

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Progress payload of updates:progress in the key order the renderer reads
type updateProgress struct {
	Stage   string   `json:"stage"`
	Message string   `json:"message"`
	Percent *float64 `json:"percent"`
	Error   *string  `json:"error"`
	At      int64    `json:"at"`
}

// Logs and broadcasts one update progress stage, noting it for the update run metrics
func (s *Service) emitUpdateProgress(stage, message string, percent *float64) {
	s.log(fmt.Sprintf("[updates] %s: %s", stage, message))
	s.metrics.noteProgress(stage)
	s.host.Broadcast("updates:progress", updateProgress{Stage: stage, Message: message, Percent: percent, At: time.Now().UnixMilli()})
}

func percentOf(value float64) *float64 { return &value }

// Applies the update through the packaged owner or the checkout hand-off, refusing while a managed SSH update runs
func (s *Service) ApplyUpdates() (ApplyResult, error) {
	return s.operation.apply(func() (ApplyResult, error) {
		s.updateInFlight.Store(true)
		handedOff := false
		defer func() {
			if !handedOff {
				s.updateInFlight.Store(false)
			}
		}()
		if joined, code, message := s.conns.JoinManagedUpdatesForApply(s.log); !joined {
			return ApplyResult{Error: code, Message: message}, nil
		}
		packaged, err := s.operation.resolve()
		if err != nil {
			return ApplyResult{}, err
		}
		strategy := packaged
		mechanism := ""
		if packaged != nil {
			mechanism = packaged.Mechanism()
		} else {
			strategy = s.checkoutStrategy()
		}
		var commitDate *float64
		if s.stamp != nil {
			commitDate = s.stamp.CommitDate
		}
		result, err := s.metrics.track(mechanism, commitDate, strategy.Apply)
		handedOff = err == nil && result.HandedOff
		return result, err
	})
}

// Status of the update owner, the packaged strategy first and the checkout strategy otherwise
func (s *Service) checkUpdates(force bool) (UpdaterStatus, error) {
	strategy, err := s.operation.resolve()
	if err == nil && strategy != nil {
		status, checkErr := strategy.Check(force)
		if checkErr == nil {
			return status, nil
		}
		err = checkErr
	}
	if err != nil {
		status := UpdaterStatus{Supported: true, Error: "check-failed", Message: err.Error(), FetchedAt: time.Now().UnixMilli()}
		if strategy != nil {
			status.Mechanism = strategy.Mechanism()
		}
		return status, nil
	}
	return s.checkoutStrategy().check(force, "")
}

// UpdatesCheckOptions is the payload of updates.check
type UpdatesCheckOptions struct {
	Force bool `json:"force"`
}

// Update status for the About panel, a check failure answered as a status
func (s *Service) UpdatesCheck(opts *UpdatesCheckOptions) UpdaterStatus {
	force := opts != nil && opts.Force
	status, err := s.checkUpdates(force)
	if err != nil {
		branch, _ := s.readDesktopUpdateConfig()
		return UpdaterStatus{Supported: true, Branch: branch, Error: "check-failed", Message: err.Error(), FetchedAt: time.Now().UnixMilli()}
	}
	return status
}

// Applies the update, a thrown failure answered as apply-failed
func (s *Service) UpdatesApply() ApplyResult {
	result, err := s.ApplyUpdates()
	if err != nil {
		return ApplyResult{Error: "apply-failed", Message: err.Error()}
	}
	return result
}

// Moves a source checkout to the stable or main channel and answers its fresh status
func (s *Service) UpdatesSetChannel(name any) (UpdaterStatus, error) {
	if s.stamp != nil && stringOrEmpty(s.stamp.Source) == "commit-build" {
		return UpdaterStatus{}, errors.New(commitBuildUpdateMessage)
	}
	if s.stamp != nil && s.stamp.Payload != "bootstrap" {
		return UpdaterStatus{}, errors.New("This package has a fixed update channel. Install the other package to change channels.")
	}
	channel, ok := name.(string)
	if !ok || (channel != "stable" && channel != "main") {
		return UpdaterStatus{}, errors.New("The update channel must be stable or main.")
	}
	return s.checkoutStrategy().check(true, channel)
}

// Release channel baked into this artifact, the channel build's name, else canary or stable by tag
func (s *Service) packagedReleaseChannel() string {
	if s.stamp == nil {
		return ""
	}
	if s.stamp.ChannelBuild != nil {
		return s.stamp.ChannelBuild.Channel
	}
	if s.stamp.Tag == nil || *s.stamp.Tag == "" || stringOrEmpty(s.stamp.Source) == "commit-build" {
		return ""
	}
	if canaryTagRE.MatchString(*s.stamp.Tag) {
		return "canary"
	}
	return "stable"
}

func (s *Service) updaterChannelFromStamp() string {
	if channel := s.packagedReleaseChannel(); channel != "" {
		return channel
	}
	return "stable"
}

func (s *Service) isLightVariant() bool {
	return s.stamp != nil && s.stamp.Payload == "light"
}

// Packaged update owner of this artifact, nil for the self updating hand-off installs
func (s *Service) createPackagedUpdateStrategy() (UpdaterStrategy, error) {
	mechanism := resolveUpdaterMechanism(s.isWindows(), s.stamp)
	if mechanism == "windows-handoff" || mechanism == "posix-handoff" {
		return nil, nil
	}
	if s.stamp != nil && s.stamp.ChannelBuild != nil && (mechanism == "electron-updater" || mechanism == "app-installer") {
		build := s.stamp.ChannelBuild
		installed, err := s.inspectRunningChannelApp(build)
		if err != nil {
			return nil, err
		}
		platform, arch := channelPlatform(), channelArch()
		if (platform != "darwin" && platform != "win32") || (arch != "arm64" && arch != "x64") {
			return nil, errors.New("Channel updates require a supported packaged application")
		}
		resolver, err := newChannelResolver(build, platform, arch, installed.Signer)
		if err != nil {
			return nil, err
		}
		return &channelStrategy{build: build, mechanism: mechanism, resolver: resolver, nativeFactory: func(target *ChannelTarget) (UpdaterStrategy, error) {
			return s.createNativePackagedStrategy(mechanism, target)
		}}, nil
	}
	return s.createNativePackagedStrategy(mechanism, nil)
}

// Native strategy of one mechanism, pinned to a channel target when one is given
func (s *Service) createNativePackagedStrategy(mechanism string, target *ChannelTarget) (UpdaterStrategy, error) {
	switch mechanism {
	case "electron-updater":
		feedBase, err := s.desktopFeedBaseURL()
		if err != nil {
			return nil, err
		}
		strategy := &releaseStrategy{service: s, channel: s.updaterChannelFromStamp(), light: s.isLightVariant(), feedBaseURL: feedBase, appVersion: s.host.AppVersion()}
		if target != nil {
			return strategy.pinTo(target)
		}
		return strategy, nil
	case "app-installer":
		payload, err := s.requireBundledPayload(mechanism)
		if err != nil {
			return nil, err
		}
		feedBase, err := s.desktopFeedBaseURL()
		if err != nil {
			return nil, err
		}
		appVersion := s.host.AppVersion()
		if s.stamp != nil && s.stamp.ChannelBuild != nil {
			appVersion = s.stamp.ChannelBuild.WindowsVersion
		}
		strategy := &appInstallerStrategy{service: s, binary: payload.Shim, channel: s.updaterChannelFromStamp(), light: s.isLightVariant(), feedBaseURL: feedBase, appVersion: appVersion}
		if target != nil {
			return strategy.pinTo(target)
		}
		return strategy, nil
	case "microsoft-store":
		payload, err := s.requireBundledPayload(mechanism)
		if err != nil {
			return nil, err
		}
		return &storeStrategy{service: s, binary: payload.Shim, appVersion: s.host.AppVersion()}, nil
	}
	return newExternalStrategy(s.stamp), nil
}

// Bundled payload an update mechanism runs inside, an error naming the broken install when it has none
func (s *Service) requireBundledPayload(mechanism string) (*backend.PayloadInfo, error) {
	payload, runtime := "", (*backend.PayloadRuntime)(nil)
	if s.stamp != nil {
		payload, runtime = s.stamp.Payload, s.stamp.Runtime
	}
	return backend.RequireBundledPayload(mechanism, s.opts.ResourcesPath, payload, runtime)
}

// Invalidates connections and waits for every owned backend before an application swap
func (s *Service) teardownBundledBackend() error {
	s.quittingForHandoff.Store(true)
	var primaryErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		primaryErr = s.backend.TeardownPrimary(false)
	}()
	s.backend.StopAllPoolBackends()
	wg.Wait()
	if primaryErr != nil {
		failure := &aggregateError{message: "Backend shutdown failed", errs: []error{primaryErr}}
		s.backend.SetStartFailure(failure)
		return failure
	}
	return nil
}

// Restarts the local backend after a failed application swap
func (s *Service) restoreBundledBackend() error {
	err := s.teardownBundledBackend()
	s.quittingForHandoff.Store(false)
	s.updateInFlight.Store(false)
	if err != nil {
		return err
	}
	s.backend.SetStartFailure(nil)
	_, err = s.backend.EnsureBackend("", backend.EnsureOptions{})
	return err
}

// App Installer and release feed base of config.yaml updates.desktop_feed_base_url, empty when unset
func (s *Service) desktopFeedBaseURL() (string, error) {
	value := ""
	if doc, err := config.ReadConfigDocReadonly(context.Background(), filepath.Join(s.opts.IcebathHome, "config.yaml")); err == nil {
		if cfg, ok := doc.(map[string]any); ok {
			if updates, ok := cfg["updates"].(map[string]any); ok {
				if configured, ok := updates["desktop_feed_base_url"].(string); ok {
					value = strings.TrimSpace(configured)
				}
			}
		}
	}
	if value == "" {
		return "", nil
	}
	if _, err := channelPublicBase(value); err != nil {
		return "", err
	}
	return value, nil
}

// Relaunch arranged for an update hand-off and how to withdraw it
type relaunchRegistration struct {
	automatic bool
	cancel    func() error
}

// Relaunch step of a packaged hand-off, onManual deciding what an unregistrable relaunch means
type packagedRelaunch struct {
	register func() (relaunchRegistration, error)
	onManual func() error
}

// Runs a packaged update, withdrawing the relaunch and restoring the backend when it fails
func (s *Service) applyPackagedHandoff(teardown, restore func() error, relaunch *packagedRelaunch, apply func(stop func() error) (ApplyResult, error)) (ApplyResult, error) {
	var registration *relaunchRegistration
	teardownStarted := false
	stop := func() error {
		if relaunch != nil {
			reg, err := relaunch.register()
			if err != nil {
				return err
			}
			registration = &reg
			if !reg.automatic {
				if err := relaunch.onManual(); err != nil {
					return err
				}
			}
		}
		teardownStarted = true
		return teardown()
	}
	result, err := apply(stop)
	if err == nil {
		return result, nil
	}
	errs := []error{err}
	if registration != nil {
		if cancelErr := registration.cancel(); cancelErr != nil {
			errs = append(errs, cancelErr)
		}
	}
	if teardownStarted {
		if restoreErr := restore(); restoreErr != nil {
			errs = append(errs, restoreErr)
		}
	}
	messages := make([]string, len(errs))
	for i, item := range errs {
		messages[i] = item.Error()
	}
	message := strings.Join(messages, "; ")
	s.emitUpdateProgress("error", message, nil)
	if len(errs) > 1 {
		return ApplyResult{}, &aggregateError{message: message, errs: errs}
	}
	return ApplyResult{}, err
}

// Writes the pending relaunch marker the next launch consumes, false when it cannot be written
func (s *Service) writePendingRelaunch(fromVersion string) bool {
	body, err := json.Marshal(struct {
		SchemaVersion int    `json:"schemaVersion"`
		FromVersion   string `json:"fromVersion"`
		StartedAt     int64  `json:"startedAt"`
	}{1, fromVersion, time.Now().UnixMilli()})
	return err == nil && os.WriteFile(filepath.Join(s.opts.UserData, pendingRelaunchFile), body, 0o666) == nil
}

// Registers the relaunch of a hand-off, the marker removed again when the waiter cannot start or is cancelled
func (s *Service) registerUpdateRelaunch(fromVersion string, relaunch func() (*relaunchWaiter, error)) (relaunchRegistration, error) {
	ownsMarker := s.writePendingRelaunch(fromVersion)
	removeMarker := func() error {
		if !ownsMarker {
			return nil
		}
		if err := os.Remove(filepath.Join(s.opts.UserData, pendingRelaunchFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	waiter, err := relaunch()
	if err != nil {
		if cleanupErr := removeMarker(); cleanupErr != nil {
			return relaunchRegistration{}, &aggregateError{message: "Relaunch registration cleanup failed", errs: []error{err, cleanupErr}}
		}
		return relaunchRegistration{}, err
	}
	var once sync.Once
	var cancelErr error
	return relaunchRegistration{automatic: waiter != nil, cancel: func() error {
		once.Do(func() {
			var errs []error
			if waiter != nil {
				if err := waiter.cancel(); err != nil {
					errs = append(errs, err)
				}
			}
			if err := removeMarker(); err != nil {
				errs = append(errs, err)
			}
			if len(errs) > 0 {
				cancelErr = &aggregateError{message: "Relaunch cancellation failed", errs: errs}
			}
		})
		return cancelErr
	}}, nil
}
