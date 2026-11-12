package windows

// MenuItem is one entry of a native menu, the shell maps roles and actions onto the platform menu.
type MenuItem struct {
	Label               string     `json:"label,omitempty"`
	Role                string     `json:"role,omitempty"`
	Accelerator         string     `json:"accelerator,omitempty"`
	Action              MenuAction `json:"action,omitempty"`
	Separator           bool       `json:"separator,omitempty"`
	NoAccelerator       bool       `json:"noAccelerator,omitempty"`
	Submenu             []MenuItem `json:"submenu,omitempty"`
	FocusedWindowTarget bool       `json:"focusedWindowTarget,omitempty"`
}

// MenuAction names what a clicked menu item does.
type MenuAction string

const (
	ActionAbout          MenuAction = "about"
	ActionCheckUpdates   MenuAction = "check-for-updates"
	ActionNewWindow      MenuAction = "new-window"
	ActionOpenFolder     MenuAction = "open-folder"
	ActionClosePreview   MenuAction = "close-preview"
	ActionPreviewReload  MenuAction = "preview-reload"
	ActionToggleDevTools MenuAction = "toggle-devtools"
	ActionZoomReset      MenuAction = "zoom-reset"
	ActionZoomIn         MenuAction = "zoom-in"
	ActionZoomOut        MenuAction = "zoom-out"
	ActionCloseWindow    MenuAction = "close-window"
)

func separator() MenuItem { return MenuItem{Separator: true} }

// WindowMenuTemplate is the native Window menu on macOS and a click only Close elsewhere
func WindowMenuTemplate(isMac bool) MenuItem {
	if isMac {
		return MenuItem{Label: "Window", Role: "windowMenu", Submenu: []MenuItem{{Role: "minimize"}, {Role: "zoom"}, {Role: "front"}}}
	}
	return MenuItem{Label: "Window", Submenu: []MenuItem{
		{Role: "minimize"},
		{Label: "Close", Action: ActionCloseWindow, NoAccelerator: true, FocusedWindowTarget: true},
	}}
}

// ApplicationMenuTemplate builds the app, File, Edit, View, Window and Help menus of the desktop app
func ApplicationMenuTemplate(isMac bool, appName string) []MenuItem {
	checkForUpdates := MenuItem{Label: "Check for Updates…", Action: ActionCheckUpdates}
	var t []MenuItem
	if isMac {
		t = append(t, MenuItem{Label: appName, Submenu: []MenuItem{
			{Label: "About " + appName, Action: ActionAbout},
			checkForUpdates,
			separator(),
			{Role: "services"},
			separator(),
			{Role: "hide"},
			{Role: "hideOthers"},
			{Role: "unhide"},
			separator(),
			{Role: "quit"},
		}})
	}
	closeItem := MenuItem{Role: "quit"}
	if isMac {
		closeItem = MenuItem{Label: "Close", Action: ActionClosePreview}
	}
	t = append(t, MenuItem{Label: "File", Submenu: []MenuItem{
		{Label: "New Window", Action: ActionNewWindow},
		{Label: "Open Folder…", Action: ActionOpenFolder},
		separator(),
		closeItem,
	}})
	edit := []MenuItem{
		{Role: "undo"}, {Role: "redo"}, separator(),
		{Role: "cut"}, {Role: "copy"}, {Role: "paste"}, {Role: "pasteAndMatchStyle"}, {Role: "delete"}, {Role: "selectAll"},
	}
	if isMac {
		edit = append(edit, separator(), MenuItem{Label: "Substitutions", Submenu: []MenuItem{
			{Role: "showSubstitutions"}, separator(), {Role: "toggleTextReplacement"},
		}})
	}
	t = append(t, MenuItem{Label: "Edit", Submenu: edit})
	devtools := "Ctrl+Shift+I"
	if isMac {
		devtools = "Alt+Cmd+I"
	}
	t = append(t, MenuItem{Label: "View", Submenu: []MenuItem{
		{Label: "Reload", Action: ActionPreviewReload},
		{Role: "forceReload"},
		{Label: "Toggle Developer Tools", Accelerator: devtools, Action: ActionToggleDevTools, FocusedWindowTarget: true},
		separator(),
		{Label: "Actual Size", Accelerator: "CommandOrControl+0", Action: ActionZoomReset},
		{Label: "Zoom In", Accelerator: "CommandOrControl+Plus", Action: ActionZoomIn},
		{Label: "Zoom Out", Accelerator: "CommandOrControl+-", Action: ActionZoomOut},
		separator(),
		{Role: "togglefullscreen"},
	}})
	t = append(t, WindowMenuTemplate(isMac))
	t = append(t, MenuItem{Label: "Help", Role: "help", Submenu: []MenuItem{checkForUpdates}})
	return t
}

// MenuStartup is what installing the application menu needs from the shell.
type MenuStartup[M any] struct {
	IsMac              bool
	BuildMenu          func() M
	SetApplicationMenu func(M)
	CreateWindow       func()
}

// InstallApplicationMenuAfterFirstWindow creates the first window, then on macOS builds and installs the menu
func InstallApplicationMenuAfterFirstWindow[M any](d MenuStartup[M]) {
	d.CreateWindow()
	if d.IsMac {
		d.SetApplicationMenu(d.BuildMenu())
	}
}
