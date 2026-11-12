package main

import (
	"embed"
	"io/fs"
	"log/slog"
	"os"
	"runtime"
	"time"

	"github.com/nickheyer/icebath/pkg/install"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:dist
var assets embed.FS

func isMacOS() bool { return runtime.GOOS == "darwin" }

func main() {
	if closer, err := install.InitLogging(); err == nil {
		defer closer.Close()
	} else {
		os.Stderr.WriteString("[hermes-setup] could not create log dir " + install.LogDir() + ": " + err.Error() + "\n")
	}
	args := os.Args[1:]
	mode := install.ModeFromArgs(args)
	force := install.ForceSetupFromArgs(args)
	slog.Info("Icebath installer starting", "mode", mode, "force_setup", force)
	if install.HandoffBeforeAppKit(isMacOS(), mode, force) {
		root := install.InstallRootOf(install.Home())
		if install.IsInstalled(root) {
			if err := install.SpawnInstalledDesktop(root); err == nil {
				time.Sleep(200 * time.Millisecond)
				slog.Info("icebath already installed — relaunched desktop; exiting installer")
				return
			} else {
				slog.Warn("relaunch of installed desktop failed; showing installer UI", "error", err)
			}
		}
	}
	dist, err := fs.Sub(assets, "dist")
	if err != nil {
		slog.Error("embedded frontend missing", "error", err)
		os.Exit(1)
	}
	svc := &SetupService{mode: mode, args: args}
	app := application.New(application.Options{
		Name:        "Icebath",
		Description: "Installs Icebath Agent on your machine.",
		Services:    []application.Service{application.NewService(svc)},
		Assets:      application.AssetOptions{Handler: application.AssetFileServerFS(dist)},
		Mac:         application.MacOptions{ActivationPolicy: application.ActivationPolicyRegular, ApplicationShouldTerminateAfterLastWindowClosed: true},
	})
	svc.app = app
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:            "main",
		Title:           "Icebath",
		Width:           880,
		Height:          620,
		MinWidth:        720,
		MinHeight:       520,
		InitialPosition: application.WindowCentered,
		Hidden:          true,
		URL:             "/",
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		win.Show()
	})
	if err := app.Run(); err != nil {
		slog.Error("error while running Icebath Setup", "error", err)
		os.Exit(1)
	}
}
