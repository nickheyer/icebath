package main

import (
	"github.com/nickheyer/icebath/pkg/desktop/windows"
	"github.com/wailsapp/wails/v3/pkg/application"
)

var menuRoles = map[string]application.Role{
	"undo": application.Undo, "redo": application.Redo, "cut": application.Cut, "copy": application.Copy,
	"paste": application.Paste, "pasteAndMatchStyle": application.PasteAndMatchStyle, "delete": application.Delete,
	"selectAll": application.SelectAll, "minimize": application.Minimise, "zoom": application.Zoom,
	"front": application.Front, "hide": application.Hide, "hideOthers": application.HideOthers,
	"unhide": application.UnHide, "quit": application.Quit, "forceReload": application.ForceReload,
	"togglefullscreen": application.ToggleFullscreen, "services": application.ServicesMenu,
}

// buildApplicationMenu turns the windows group template into a native Wails menu
func (s *Shell) buildApplicationMenu() *application.Menu {
	m := application.NewMenu()
	for _, top := range windows.ApplicationMenuTemplate(s.platform.IsMac(), appName) {
		if top.Role == "windowMenu" {
			m.AddRole(application.WindowMenu)
			continue
		}
		s.fillMenu(m.AddSubmenu(top.Label), top.Submenu)
	}
	return m
}

func (s *Shell) fillMenu(m *application.Menu, items []windows.MenuItem) {
	for _, it := range items {
		switch {
		case it.Separator:
			m.AddSeparator()
		case len(it.Submenu) > 0:
			s.fillMenu(m.AddSubmenu(it.Label), it.Submenu)
		case it.Role == "showSubstitutions" || it.Role == "toggleTextReplacement":
			label := map[string]string{"showSubstitutions": "Show Substitutions", "toggleTextReplacement": "Text Replacement"}[it.Role]
			role := it.Role
			m.Add(label).OnClick(func(*application.Context) { nativeSendTextAction(role) })
		case it.Role != "":
			m.AddRole(menuRoles[it.Role])
		default:
			item := m.Add(it.Label)
			if it.Accelerator != "" && !it.NoAccelerator {
				item.SetAccelerator(it.Accelerator)
			}
			action := it.Action
			item.OnClick(func(*application.Context) { s.runMenuAction(action) })
		}
	}
}

func (s *Shell) focusedWindow() windows.Window {
	if cur := s.app.Window.Current(); cur != nil {
		if w := s.windowByName(cur.Name()); w != nil {
			return w
		}
	}
	return nil
}

func (s *Shell) runMenuAction(action windows.MenuAction) {
	s.Windows.RunMenuAction(action, s.focusedWindow(), windows.MenuHandlers{
		About:       s.showAbout,
		OpenUpdates: s.DeepLinks.OpenUpdates,
		NewWindow:   s.newInstanceWindow,
		ToggleDevTools: func(w windows.Window) {
			if sw, ok := w.(*shellWindow); ok && sw != nil {
				sw.w.OpenDevTools()
			}
		},
	})
}

// showAbout opens the native about panel with the app version and copyright
func (s *Shell) showAbout() {
	s.app.Menu.ShowAbout()
}
