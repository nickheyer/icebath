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
		About:  "Desktop control through cua-driver: screenshots, mouse and keyboard, in the background",
		Settings: []config.Setting{
			{Key: Backend, Title: "Computer use backend", About: "Which computer-use provider controls the screen; an unknown name stops the tool with a list of the available ones", Default: Cua, Options: config.Options(
				config.Option{Value: Cua, Title: "cua-driver", About: "Built-in background computer use on macOS, Windows and Linux; does not steal the cursor or focus"},
			)},
			{Key: PermissionMode, Title: "Computer use access", About: "How much of the screen the agent may control; any value other than bounded is treated as standard", Default: Standard, Options: config.Options(
				config.Option{Value: Standard, Title: "Standard", About: "Normal driver access."},
				config.Option{Value: Bounded, Title: "Bounded", About: "Limited by the capability file."},
			)},
			{Key: CapabilityManifest, Title: "Capability file", About: "Path to a file limiting what the driver may do; used only outside the standard permission mode", Default: ""},
			{Key: CaptureAfterMode, Title: "Follow-up capture", About: "What each action sends back after it runs when it asks for a capture", Default: "som", Options: config.Options(
				config.Option{Value: "som", Title: "Labeled shot", About: "Screenshot with numbered elements."},
				config.Option{Value: "vision", Title: "Screenshot", About: "Plain screenshot only."},
				config.Option{Value: "ax", Title: "Element list", About: "Text list of on-screen elements."},
			)},
			{Key: DriverCommand, Title: "Driver program", About: "Run this computer-use driver program instead of the managed one; empty uses the cua-driver icebath installs", Default: "", Env: []string{DriverCommandEnv}},
			{Key: AllowUnsignedDriver, Title: "Allow unsigned driver", About: "Accept a driver app that lacks the expected publisher signature; only for locally built drivers on macOS", Default: false},
			{Key: Autostart, Title: "Auto start driver", About: "Start the computer-use driver each time you log in, on Windows", Default: false},
			{Key: AXMaxElements, Title: "Max screen elements", About: "Most on-screen items listed per accessibility snapshot; zero means no limit", Default: 200},
			{Key: MaxImageDimension, Title: "Max screenshot edge", About: "Longest side of a screenshot sent to the model, in pixels; zero or less turns the cap off", Default: 1456},
			{Key: Telemetry, Title: "Driver telemetry", About: "Let the computer-use driver send usage data", Default: false},
			{Key: NativeWayland, Title: "Native Wayland", About: "Use Wayland directly instead of the X11 compatibility layer", Default: false},
			{Key: NoOverlay, Title: "Hide overlay", About: "Turn the on-screen cursor overlay off; auto decides by the platform and display", Default: OverlayAuto, Options: config.Options(
				config.Option{Value: OverlayAuto, Title: "Decided automatically", About: "Hidden on macOS and without a Wayland session on Linux."},
				config.Option{Value: OverlayOff, Title: "Hidden", About: "The overlay is turned off."},
				config.Option{Value: OverlayOn, Title: "Shown", About: "The overlay stays on."},
			)},
		},
	}})
}
