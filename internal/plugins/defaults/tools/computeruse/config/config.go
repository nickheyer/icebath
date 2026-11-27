package config

import toolsconfig "github.com/nickheyer/icebath/internal/tools/config"

const (
	Key = toolsconfig.Key + ".computer_use"
	// Provider that controls the screen
	Backend = Key + ".backend"
	// How much of the screen the agent may control, and the file bounding it
	PermissionMode     = Key + ".permission_mode"
	CapabilityManifest = Key + ".capability_manifest"
	// What an action with capture_after sends back
	CaptureAfterMode = Key + ".capture_after_mode"
	// Driver program in place of the managed one
	DriverCommand = Key + ".driver_command"
	// A locally built CuaDriver.app without the publisher's signature is accepted
	AllowUnsignedDriver = Key + ".allow_unsigned_driver"
	// The driver starts at each login on Windows
	Autostart = Key + ".autostart"
	// Most elements one accessibility walk lists, and the longest edge of a screenshot
	AXMaxElements     = Key + ".ax_max_elements"
	MaxImageDimension = Key + ".max_image_dimension"
	// The driver may send usage data
	Telemetry = Key + ".cua_telemetry"
	// The driver uses Wayland directly rather than X11 through XWayland
	NativeWayland = Key + ".native_wayland"
	// Whether the driver's cursor overlay is hidden, one of the overlay choices
	NoOverlay = Key + ".no_overlay"
)

// Provider built in
const Cua = "cua"

// Permission modes: the driver's own approvals, a ceiling the capability file sets, and none while approvals are bypassed
const (
	Standard     = "standard"
	Bounded      = "bounded"
	Unrestricted = "unrestricted"
)

// Overlay choices: decided by the platform and display, hidden, shown
const (
	OverlayAuto = "auto"
	OverlayOff  = "true"
	OverlayOn   = "false"
)

// Environment variable naming the driver program, which wins over the setting
const DriverCommandEnv = "ICEBATH_CUA_DRIVER_CMD"
