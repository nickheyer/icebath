package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/wailsapp/wails/v3/pkg/application"
)

// Bound service answering the nine installer commands and emitting the bootstrap event
type SetupService struct {
	app           *application.App
	mode          install.AppMode
	args          []string
	mu            sync.Mutex
	cancel        context.CancelFunc
	status        *install.BootstrapStatus
	updateRunning atomic.Bool
}

// Sends one payload on the bootstrap channel
func (s *SetupService) emit(e install.Event) {
	if s.app == nil {
		return
	}
	s.app.Event.Emit(install.EventChannel, e)
}

// Install or update, chosen once from the process arguments
func (s *SetupService) GetMode() install.AppMode { return s.mode }

// Starts the install in the background, refusing while one is running
func (s *SetupService) StartBootstrap(args install.BootstrapArgs) error {
	s.mu.Lock()
	if s.status != nil && s.status.Running {
		s.mu.Unlock()
		return errors.New("Bootstrap is already running")
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.status = &install.BootstrapStatus{Running: true}
	s.mu.Unlock()
	go func() {
		root, err := install.Bootstrap(ctx, args, s.emit)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Running = false
		if err != nil {
			msg := err.Error()
			s.status.Completed, s.status.LastError = false, &msg
			return
		}
		s.status.Completed, s.status.InstallRoot, s.status.LastError = true, &root, nil
	}()
	return nil
}

// Cancels a running install
func (s *SetupService) CancelBootstrap() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Running, completed, install root and last error of the most recent install
func (s *SetupService) GetBootstrapStatus() install.BootstrapStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		return install.BootstrapStatus{}
	}
	return *s.status
}

// Starts the update hand off once, a duplicate call re-emitting the manifest so a reset UI recovers its stages
func (s *SetupService) StartUpdate() error {
	if !s.updateRunning.CompareAndSwap(false, true) {
		root := install.InstallRootOf(install.Home())
		s.emit(install.ManifestEvent(install.UpdateStages(install.DesktopProductPresent(root), install.TargetAppFromArgs(s.args) != "" && isMacOS()), nil))
		return nil
	}
	go func() {
		defer s.updateRunning.Store(false)
		err := install.RunUpdate(context.Background(), install.UpdateHost{Args: s.args, Emit: s.emit, Exit: s.exitAfterSuccess})
		if err != nil {
			slog.Error("update failed", "error", err)
		}
	}()
	return nil
}

// Quits the app with a forced process exit after three seconds for an event loop that ignores the request
func (s *SetupService) exitAfterSuccess() {
	go func() {
		time.Sleep(3 * time.Second)
		slog.Warn("graceful updater exit timed out; forcing process exit")
		os.Exit(0)
	}()
	if s.app != nil {
		s.app.Quit()
	}
}

// Launches the installed desktop and closes the installer
func (s *SetupService) LaunchHermesDesktop(installRoot string) error {
	slog.Info("launching Icebath desktop", "install_root", installRoot)
	if err := install.LaunchDesktop(installRoot); err != nil {
		return err
	}
	if s.app != nil {
		s.app.Quit()
	}
	return nil
}

// Path of the installer log
func (s *SetupService) GetLogPath() string { return install.LogPath() }

// Icebath home the installer works in
func (s *SetupService) GetHermesHome() string { return install.Home() }

// Opens the log directory in the platform file manager
func (s *SetupService) OpenLogDir() error {
	if s.app == nil {
		return errors.New("installer app is not running")
	}
	return s.app.Browser.OpenFile(install.LogDir())
}
