module github.com/nickheyer/icebath/apps/desktop

go 1.27.2

require (
	github.com/nickheyer/icebath v0.0.0
	github.com/wailsapp/wails/v3 v3.1.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/ebitengine/purego v0.11.1 // indirect
	github.com/go-ole/go-ole v1.3.0 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	golang.org/x/term v0.47.0 // indirect
)

replace github.com/nickheyer/icebath => ../..
