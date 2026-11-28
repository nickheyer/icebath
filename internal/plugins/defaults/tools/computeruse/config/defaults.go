package config

import (
	"github.com/nickheyer/icebath/internal/config"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
)

func Register(cfg *config.Config) {
	cfg.Register(config.Sections{{
		Key:    Key,
		Parent: toolsconfig.Key,
		Title:  "Computer use",
		About:  "Screenshots, mouse and keyboard control for the agent",
		Order:  110,
		Settings: []config.Setting{
			{Key: PermissionMode, Title: "Access", About: "How much of the computer the agent may control.",
				Help: "Running with approvals bypassed lifts this limit.", Default: Standard, Options: config.Options(
					config.Option{Value: Standard, Title: "Standard", About: "The driver asks you as it normally does"},
					config.Option{Value: Bounded, Title: "Bounded", About: "Limited to what the capability file allows"},
				)},
			{Key: CaptureAfterMode, Title: "Capture after actions", About: "What the agent gets back after an action that asks for a capture.",
				Default: "som", Options: config.Options(
					config.Option{Value: "som", Title: "Labeled screenshot", About: "A screenshot with numbered elements"},
					config.Option{Value: "vision", Title: "Screenshot", About: "A plain screenshot"},
					config.Option{Value: "ax", Title: "Element list", About: "A text list of what is on screen"},
				)},
			{Key: NoOverlay, Title: "Cursor overlay", About: "Whether the agent's cursor is drawn on the screen.", Default: OverlayAuto, Options: config.Options(
				config.Option{Value: OverlayAuto, Title: "Automatic", About: "Hidden on macOS, and on Linux without Wayland"},
				config.Option{Value: OverlayOff, Title: "Hidden", About: "The overlay is off"},
				config.Option{Value: OverlayOn, Title: "Shown", About: "The overlay is on"},
			)},
			{Key: Autostart, Title: "Start at sign-in", About: "Start the driver each time you sign in to Windows.", Default: false},
			{Key: Telemetry, Title: "Driver usage data", About: "Let the driver send usage data to its makers.", Default: false},
			{Key: Backend, Title: "Driver", About: "Which program controls the screen.", Default: Cua, Options: config.Options(
				config.Option{Value: Cua, Title: "cua-driver", About: "Built in, works in the background without taking the cursor"},
			), Advanced: true},
			{Key: CapabilityManifest, Title: "Capability file", About: "The file that limits what the driver may do.",
				Help: "Needed for Bounded access. A version 3 file also limits runs with approvals bypassed.", Default: "", Advanced: true,
				Path: &config.Path{Kind: config.PathFile, Exists: true}},
			{Key: DriverCommand, Title: "Driver program", About: "The driver program that runs instead of the one icebath installs.", Default: "",
				Zero: "Installed driver", Env: []string{DriverCommandEnv}, Advanced: true, Path: &config.Path{Kind: config.PathProgram, Exists: true}},
			{Key: AllowUnsignedDriver, Title: "Allow unsigned driver", About: "Accept a driver that lacks the publisher's signature.",
				Help: "Only for a driver you built yourself on macOS.", Default: false, Advanced: true},
			{Key: AXMaxElements, Title: "Elements per list", About: "The most elements that one list of what is on screen holds.", Default: 200,
				Zero: "No limit", Counts: "elements", Least: config.Num(0), Advanced: true},
			{Key: MaxImageDimension, Title: "Screenshot size", About: "The longest side of a screenshot that the model sees.", Default: 1456,
				Zero: "The driver's default", Counts: "pixels", Least: config.Num(0), Advanced: true},
			{Key: NativeWayland, Title: "Native Wayland", About: "Use Wayland directly instead of the X11 layer.", Default: false, Advanced: true},
		},
	}})
}
