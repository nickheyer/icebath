//go:build !windows

package msalruntime

// Console window handle, zero outside Windows where window handles are int64 values the caller supplies
func ConsoleWindow() WindowHandle { return 0 }

// Desktop window handle, zero outside Windows
func DesktopWindow() WindowHandle { return 0 }
