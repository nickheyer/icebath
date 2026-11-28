package app

import (
	tea "charm.land/bubbletea/v2"

	configapp "github.com/nickheyer/icebath/internal/config/app"
	vaultconfig "github.com/nickheyer/icebath/internal/vault/config"
	configv1 "github.com/nickheyer/icebath/pkg/gen/icebath/config/v1"
	vaultv1 "github.com/nickheyer/icebath/pkg/gen/icebath/vault/v1"
	"github.com/nickheyer/icebath/pkg/tui"
	"github.com/nickheyer/icebath/pkg/ui/cards"
	"github.com/nickheyer/icebath/pkg/ui/render"
	"github.com/nickheyer/icebath/pkg/ui/theme"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Whether the profile uses a password manager, in the words every page says it in, in the color of how it stands
func managerWords(source *vaultv1.Source, t theme.Theme) string {
	switch source.GetState() {
	case "on":
		return render.Good("In use", t)
	case "off":
		return "Off"
	}
	return render.Warm("Not installed", t)
}

// What a row of the page of a password manager does
type managerAct int

const (
	optionRow managerAct = iota
	unlockRow
	lockRow
	turnRow
)

// Row of the page of a password manager: unlocking or locking it, turning it on or off, an option, or the row of its advanced options
type managerRow struct {
	act      managerAct
	setting  *configv1.Setting
	advanced bool
}

// Section of the settings of a password manager
func managerSection(name string) string {
	return vaultconfig.Key + "." + name
}

// Opens the page of a password manager over the first view: how it stands as a sentence, unlocking or locking it, turning it on or off,
// then its options
func (p *page) managerPage(name string) tea.Cmd {
	source := p.source(name)
	if source == nil {
		return nil
	}
	t := p.kit.Env.Theme
	section := managerSection(name)
	enabled := vaultconfig.Of(name, vaultconfig.Enabled)
	var shown []managerRow
	var view *cards.List
	opened := false
	view = cards.NewList(p.kit.Env, cards.ListSpec{
		OnFocus: func(*cards.List) tea.Cmd {
			if !opened || p.hub.Top() != view {
				return nil
			}
			opened = false
			return p.load(nil)
		},
		Title:   source.GetDisplayName(),
		Glyph:   t.Glyphs.Keys,
		Key:     Screen + ":" + name,
		Context: cards.HubContext,
		Primary: "hub.use",
		Intro:   func() []string { return p.managerState(name) },
		Columns: configapp.Columns,
		Rows: func() [][]string {
			shown = nil
			now := p.source(name)
			if now == nil {
				return nil
			}
			switch {
			case now.GetState() != "on" || now.GetReady():
			case now.GetLocked():
				shown = append(shown, managerRow{act: unlockRow})
			default:
				shown = append(shown, managerRow{act: lockRow})
			}
			if now.GetState() != "missing" {
				shown = append(shown, managerRow{act: turnRow})
			}
			options, advanced, _ := p.sectionOptions(section, enabled)
			for _, setting := range options {
				shown = append(shown, managerRow{setting: setting})
			}
			if advanced != nil {
				shown = append(shown, managerRow{advanced: true})
			}
			cells := make([][]string, len(shown))
			for i, r := range shown {
				cells[i] = p.managerCells(now, section, enabled, r)
			}
			return cells
		},
		Group: func(i int) string {
			if shown[i].setting != nil || shown[i].advanced {
				return "Options"
			}
			return ""
		},
		Detail: func(i int) []string {
			now := p.source(name)
			switch r := shown[i]; {
			case r.act == unlockRow:
				return []string{"Type the master password of " + now.GetDisplayName() + ", kept for this run of icebath only."}
			case r.act == lockRow:
				return []string{"Forget the unlock, so the agent asks you again when it needs a login."}
			case r.act == turnRow && now.GetState() == "on":
				return []string{"Stop the agent from filling in the logins of " + now.GetDisplayName() + "."}
			case r.act == turnRow:
				return []string{"Let the agent fill in the logins of " + now.GetDisplayName() + "."}
			case r.setting != nil:
				return p.options.Detail(r.setting, view.Cut(i, 2))
			}
			return []string{"Settings of " + now.GetDisplayName() + " that few people change."}
		},
		Empty: func() string { return tui.Waiting(p.loaded, p.err, source.GetDisplayName()+" is no longer there.") },
		Hints: func(_ *cards.List, i int) []wm.Hint {
			if i < 0 || i >= len(shown) {
				return nil
			}
			switch r := shown[i]; {
			case r.act == unlockRow:
				return []wm.Hint{{Action: "hub.use", Label: "unlock"}}
			case r.act == lockRow:
				return []wm.Hint{{Action: "hub.use", Label: "lock"}}
			case r.act == turnRow && p.source(name).GetState() == "on":
				return []wm.Hint{{Action: "hub.use", Label: "turn off"}}
			case r.act == turnRow:
				return []wm.Hint{{Action: "hub.use", Label: "turn on"}}
			case r.setting != nil:
				return p.options.Hints(r.setting)
			}
			return []wm.Hint{{Action: "hub.use", Label: "open"}}
		},
		Actions: map[string]func(*cards.List, int) tea.Cmd{
			"hub.use": func(_ *cards.List, i int) tea.Cmd {
				now := p.source(name)
				if now == nil {
					return nil
				}
				switch r := shown[i]; {
				case r.act == unlockRow:
					return p.unlock(now)
				case r.act == lockRow:
					return p.change("Locking "+now.GetDisplayName()+"…", func() (string, error) {
						_, err := p.service.Lock(p.kit.Env.Ctx, &vaultv1.LockRequest{Profile: p.profile, Name: name})
						return now.GetDisplayName() + " is locked. The agent asks you to unlock it when it needs a login.", err
					})
				case r.act == turnRow:
					on := now.GetState() != "on"
					return p.change("Saving…", func() (string, error) {
						set, err := p.service.SetSource(p.kit.Env.Ctx, &vaultv1.SetSourceRequest{Profile: p.profile, Name: name, On: on})
						if on {
							return set.GetDisplayName() + " is now In use.", err
						}
						return set.GetDisplayName() + " is now Off.", err
					})
				case r.setting != nil:
					opened = true
					return p.options.Change(r.setting)
				}
				if _, _, open := p.sectionOptions(section, enabled); open != nil {
					opened = true
					return open()
				}
				return nil
			},
			"hub.remove": func(l *cards.List, i int) tea.Cmd {
				if setting := shown[i].setting; setting != nil {
					return p.options.Reset(l, setting)
				}
				return nil
			},
		},
	})
	view.ToggleDetail()
	p.watch(view)
	return p.hub.Push(view)
}

func (p *page) managerCells(source *vaultv1.Source, section, enabled string, r managerRow) []string {
	switch {
	case r.act == unlockRow:
		return []string{"Unlock", "", ""}
	case r.act == lockRow:
		return []string{"Lock", "", ""}
	case r.act == turnRow && source.GetState() == "on":
		return []string{"Turn off", "", ""}
	case r.act == turnRow:
		return []string{"Turn on", "", ""}
	case r.setting != nil:
		return p.options.Cells(r.setting)
	}
	_, advanced, _ := p.sectionOptions(section, enabled)
	return advanced
}

// First line of the page of a password manager: how it stands, as a sentence
func (p *page) managerState(name string) []string {
	source := p.source(name)
	if source == nil {
		return nil
	}
	t := p.kit.Env.Theme
	switch {
	case source.GetState() == "missing":
		return []string{render.Warm("Not installed. icebath looks for its program on your PATH, or where its options below say.", t)}
	case source.GetState() == "off":
		return []string{"Off. The agent does not fill in its logins."}
	case source.GetReady():
		return []string{render.Good("In use.", t) + " It needs no unlock."}
	case source.GetLocked():
		return []string{render.Good("In use.", t) + " Locked: the agent asks you to unlock it when it needs a login."}
	}
	return []string{render.Good("In use.", t) + " Unlocked for this run of icebath, until it sits idle."}
}

// Asks for the master password of a password manager and unlocks it with it
func (p *page) unlock(source *vaultv1.Source) tea.Cmd {
	name, title := source.GetName(), source.GetDisplayName()
	field := cards.Field{Label: "Master password", Kind: cards.FieldSecret,
		Help: "The master password of " + title + ". icebath keeps it for this run only, until it sits idle."}
	return p.kit.Ask(p.hub, "Unlock "+title, field, "Unlock", func(master string) tea.Cmd {
		return p.change("Unlocking "+title+"…", func() (string, error) {
			_, err := p.service.Unlock(p.kit.Env.Ctx, &vaultv1.UnlockRequest{Profile: p.profile, Name: name, MasterPassword: master})
			return title + " is unlocked until icebath sits idle.", err
		})
	})
}
