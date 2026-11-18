package tui

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/ui/wm"
)

// Placeholder pets of petdex the gallery hides, as the reference picker does
var clawdPlaceholderRe = regexp.MustCompile(`(?i)^clawd(-|$)`)

// One pet of a pet.gallery answer
type petEntry struct {
	slug, name         string
	installed, curated bool
}

// Gallery of pet.gallery, whether the display is on, the configured slug and the pets in gateway order
type petGallery struct {
	enabled bool
	active  string
	pets    []petEntry
}

// Gallery of a pet.gallery answer
func parsePetGallery(res map[string]any) petGallery {
	g := petGallery{enabled: truthyJS(res["enabled"]), active: stringOr(res["active"], "")}
	list, _ := res["pets"].([]any)
	for _, raw := range list {
		entry, _ := raw.(map[string]any)
		slug := stringOr(entry["slug"], "")
		if slug == "" {
			continue
		}
		g.pets = append(g.pets, petEntry{slug: slug, name: stringOr(entry["displayName"], slug), installed: truthyJS(entry["installed"]), curated: truthyJS(entry["curated"])})
	}
	return g
}

// Installed pet of a slug, nil when the slug is not installed
func (g petGallery) installed(slug string) *petEntry {
	for i := range g.pets {
		if g.pets[i].installed && g.pets[i].slug == slug {
			return &g.pets[i]
		}
	}
	return nil
}

// Pet the display resolves, the configured one when installed, else the first installed
func (g petGallery) current() *petEntry {
	if e := g.installed(g.active); e != nil {
		return e
	}
	for i := range g.pets {
		if g.pets[i].installed {
			return &g.pets[i]
		}
	}
	return nil
}

// Pet verb of a command, its failure said in the pet's words, its answer dropped once the run is stale
func (r *slashRun) petCall(method string, params map[string]any, then func(map[string]any) tea.Cmd) tea.Cmd {
	return gatewayCall(r.a.gw, method, params, func(v any, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		res := asRPCResult(v)
		switch {
		case err != nil:
			r.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
			return nil
		case res == nil:
			r.sys(tr("slashCmd.session.pet.error", tr("pickers.common.invalidResponse", method)))
			return nil
		}
		return then(res)
	})
}

// Slash command of the animated pet, every subcommand on the pet verbs
func (a *app) petCommands() []slashCommand {
	subs := []string{"toggle", "list", "select", "off", "scale", "rename", "remove", "export", "info"}
	return []slashCommand{{name: "pet", help: tr("slashCmd.session.pet.help"),
		usage:   "[toggle|list|select <slug>|off|scale <n>|rename [slug] <name>|remove [slug]|export [slug] [path]|info|<slug>]",
		suggest: func(string) []string { return subs },
		run:     func(r *slashRun) tea.Cmd { return tea.Batch(r.ran(), a.runPet(r)) }}}
}

// Runs /pet, bare or toggle flipping the display, the gallery for list, a subcommand, or a slug to adopt
func (a *app) runPet(r *slashRun) tea.Cmd {
	arg := trimJS(r.arg)
	low := strings.ToLower(arg)
	first, rest := "", ""
	if words := strings.Fields(arg); len(words) > 0 {
		first, rest = strings.ToLower(words[0]), trimJS(strings.TrimPrefix(arg, words[0]))
	}
	switch {
	case arg == "" || low == "toggle":
		return a.petToggle(r)
	case low == "list" || low == "gallery" || low == "browse" || low == "all":
		return a.openPetGallery(r)
	case low == "off":
		return r.petCall("pet.disable", map[string]any{}, func(map[string]any) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.putAway"))
			return a.pet.refresh()
		})
	case low == "info":
		return a.petInfo(r)
	case first == "scale":
		return a.petScale(r, rest)
	case first == "select":
		if rest == "" {
			r.sys(tr("slashCmd.session.pet.usageSelect"))
			return nil
		}
		return a.petSelect(r, rest)
	case first == "rename":
		return a.petRename(r, rest)
	case first == "remove":
		return a.petRemove(r, rest)
	case first == "export":
		return a.petExport(r, rest)
	}
	return a.petSelect(r, arg)
}

// Flips the display, putting the shown pet away or bringing out the configured or first installed one
func (a *app) petToggle(r *slashRun) tea.Cmd {
	return r.petCall("pet.gallery", map[string]any{"localOnly": true}, func(res map[string]any) tea.Cmd {
		g := parsePetGallery(res)
		cur := g.current()
		if g.enabled {
			return r.petCall("pet.disable", map[string]any{}, func(map[string]any) tea.Cmd {
				if cur != nil {
					r.sys(tr("slashCmd.session.pet.putAwayNamed", cur.name))
				} else {
					r.sys(tr("slashCmd.session.pet.putAway"))
				}
				return a.pet.refresh()
			})
		}
		if cur == nil {
			r.sys(tr("slashCmd.session.pet.error", tr("slashCmd.session.pet.noneInstalled")))
			return nil
		}
		return r.petCall("pet.select", map[string]any{"slug": cur.slug}, func(sel map[string]any) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.isOut", stringOr(sel["displayName"], cur.name)))
			return a.pet.refresh()
		})
	})
}

// Adopts a petdex pet by slug, installing it first when needed
func (a *app) petSelect(r *slashRun, slug string) tea.Cmd {
	r.sys(tr("slashCmd.session.pet.fetching", slug))
	return r.petCall("pet.select", map[string]any{"slug": slug}, func(res map[string]any) tea.Cmd {
		r.sys(tr("slashCmd.session.pet.isOut", stringOr(res["displayName"], slug)))
		return a.pet.refresh()
	})
}

// Persists the pet scale the gateway clamps to the engine bounds
func (a *app) petScale(r *slashRun, value string) tea.Cmd {
	if value == "" {
		r.sys(tr("slashCmd.session.pet.usageScale"))
		return nil
	}
	return r.petCall("pet.scale", map[string]any{"scale": value}, func(res map[string]any) tea.Cmd {
		scale, _ := res["scale"].(float64)
		shown, err := pyrepr.Format(scale, "g")
		if err != nil {
			r.sys(tr("slashCmd.session.pet.error", err.Error()))
			return nil
		}
		r.sys(tr("slashCmd.session.pet.scaleSet", shown))
		return a.pet.refresh()
	})
}

// Renames an installed pet, the named slug when the first word is one, else the pet the display resolves
func (a *app) petRename(r *slashRun, rest string) tea.Cmd {
	return r.petCall("pet.gallery", map[string]any{"localOnly": true}, func(res map[string]any) tea.Cmd {
		g := parsePetGallery(res)
		target, name := (*petEntry)(nil), rest
		if words := strings.Fields(rest); len(words) >= 2 {
			if e := g.installed(words[0]); e != nil {
				target, name = e, trimJS(strings.TrimPrefix(rest, words[0]))
			}
		}
		if target == nil {
			target = g.current()
		}
		if target == nil || name == "" {
			r.sys(tr("slashCmd.session.pet.usageRename"))
			return nil
		}
		return r.petCall("pet.rename", map[string]any{"slug": target.slug, "name": name}, func(out map[string]any) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.renamed", target.name, name, stringOr(out["slug"], target.slug)))
			return a.pet.refresh()
		})
	})
}

// Uninstalls a pet after a confirmation, the named one or the pet the display resolves
func (a *app) petRemove(r *slashRun, slug string) tea.Cmd {
	return r.petCall("pet.gallery", map[string]any{"localOnly": true}, func(res map[string]any) tea.Cmd {
		g := parsePetGallery(res)
		target := g.current()
		if slug != "" {
			if target = g.installed(slug); target == nil {
				r.sys(tr("slashCmd.session.pet.notInstalled", slug))
				return nil
			}
		}
		if target == nil {
			r.sys(tr("slashCmd.session.pet.usageRemove"))
			return nil
		}
		return a.confirmDestructive(tr("slashCmd.session.pet.removeTitle", target.name), tr("slashCmd.session.pet.removeDetail"),
			tr("slashCmd.session.pet.removeConfirm"), tr("slashCmd.session.pet.cancel"), func() tea.Cmd {
				return r.petCall("pet.remove", map[string]any{"slug": target.slug}, func(out map[string]any) tea.Cmd {
					if truthyJS(out["ok"]) {
						r.sys(tr("slashCmd.session.pet.removed", target.name))
					} else {
						r.sys(tr("slashCmd.session.pet.notInstalled", target.slug))
					}
					return a.pet.refresh()
				})
			})
	})
}

// Writes the zip of a pet to a path, the pet named first or else the pet the display resolves
func (a *app) petExport(r *slashRun, rest string) tea.Cmd {
	return r.petCall("pet.gallery", map[string]any{"localOnly": true}, func(res map[string]any) tea.Cmd {
		g := parsePetGallery(res)
		target, path := (*petEntry)(nil), rest
		if words := strings.Fields(rest); len(words) > 0 {
			if e := g.installed(words[0]); e != nil {
				target, path = e, trimJS(strings.TrimPrefix(rest, words[0]))
			}
		}
		if target == nil {
			target = g.current()
		}
		if target == nil {
			r.sys(tr("slashCmd.session.pet.usageExport"))
			return nil
		}
		return r.petCall("pet.export", map[string]any{"slug": target.slug}, func(out map[string]any) tea.Cmd {
			data, err := base64.StdEncoding.DecodeString(stringOr(out["zipBase64"], ""))
			if err != nil {
				r.sys(tr("slashCmd.session.pet.error", err.Error()))
				return nil
			}
			dest, err := a.exportPath(path, stringOr(out["filename"], target.slug+".zip"))
			if err != nil {
				r.sys(tr("slashCmd.session.pet.error", err.Error()))
				return nil
			}
			write := func() tea.Cmd { return a.writePetExport(target.name, dest, data) }
			if _, err := os.Stat(dest); err == nil {
				return a.confirmDestructive(tr("slashCmd.session.pet.replaceTitle", dest), tr("slashCmd.session.pet.replaceDetail"),
					tr("slashCmd.session.pet.replaceConfirm"), tr("slashCmd.session.pet.cancel"), write)
			}
			return write()
		})
	})
}

// File an export goes to, a folder or a trailing separator taking the file name the gateway gave
func (a *app) exportPath(raw, filename string) (string, error) {
	base := a.store.typedPathBase()
	if base == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = cwd
	}
	if raw == "" {
		return filepath.Join(base, filename), nil
	}
	path := raw
	if path == "~" || strings.HasPrefix(path, "~/") {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(userHome, strings.TrimPrefix(path, "~"))
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	if info, err := os.Stat(path); (err == nil && info.IsDir()) || strings.HasSuffix(raw, "/") || strings.HasSuffix(raw, string(os.PathSeparator)) {
		return filepath.Join(path, filename), nil
	}
	return path, nil
}

// Writes an export zip beside the UI and says where it went
func (a *app) writePetExport(name, dest string, data []byte) tea.Cmd {
	return func() tea.Msg {
		err := os.WriteFile(dest, data, 0o644)
		return asyncDoneMsg{run: func() tea.Cmd {
			if err != nil {
				a.sys(tr("slashCmd.session.pet.error", err.Error()))
				return nil
			}
			a.sys(tr("slashCmd.session.pet.exported", name, dest))
			return nil
		}}
	}
}

// Panel of the pet out now, its sheet, frames, state rows and the way the window draws it
func (a *app) petInfo(r *slashRun) tea.Cmd {
	return r.petCall("pet.info.meta", map[string]any{}, func(meta map[string]any) tea.Cmd {
		if !truthyJS(meta["enabled"]) {
			r.sys(tr("slashCmd.session.pet.infoOff"))
			return nil
		}
		return r.petCall("pet.info", map[string]any{"knownRevision": stringOr(meta["spritesheetRevision"], "")}, func(info map[string]any) tea.Cmd {
			if !truthyJS(info["enabled"]) {
				r.sys(tr("slashCmd.session.pet.infoOff"))
				return nil
			}
			whole := func(key string) int {
				n, _ := info[key].(float64)
				return int(n)
			}
			scale, _ := info["scale"].(float64)
			shown, err := pyrepr.Format(scale, "g")
			if err != nil {
				r.sys(tr("slashCmd.session.pet.error", err.Error()))
				return nil
			}
			counts, _ := info["framesByState"].(map[string]any)
			var frames []string
			for _, state := range pet.States {
				if n, isNum := counts[string(state)].(float64); isNum {
					frames = append(frames, fmt.Sprintf("%s %d", state, int(n)))
				}
			}
			rows := [][2]string{
				{tr("slashCmd.session.pet.infoSlug"), stringOr(info["slug"], "")},
				{tr("slashCmd.session.pet.infoScale"), shown},
				{tr("slashCmd.session.pet.infoSheet"), fmt.Sprintf("%s · %d×%d", stringOr(info["mime"], ""), whole("frameW"), whole("frameH"))},
				{tr("slashCmd.session.pet.infoFrames"), strings.Join(frames, " · ")},
				{tr("slashCmd.session.pet.infoRows"), strings.Join(stringList(info["stateRows"]), " · ")},
				{tr("slashCmd.session.pet.infoRevision"), stringOr(info["spritesheetRevision"], "")},
				{tr("slashCmd.session.pet.infoDrawing"), a.pet.drawing()},
			}
			a.panel(stringOr(info["displayName"], stringOr(info["slug"], "")), []panelSection{{Rows: rows}})
			return nil
		})
	})
}

// Gallery picker of pet.gallery, typed text narrowing it and enter adopting the pet under the cursor
func (a *app) openPetGallery(r *slashRun) tea.Cmd {
	return tea.Batch(wm.Notify(tr("pickers.pet.loading"), wm.Info), r.petCall("pet.gallery", map[string]any{}, func(res map[string]any) tea.Cmd {
		g := parsePetGallery(res)
		var shown []petEntry
		for _, e := range g.pets {
			if !clawdPlaceholderRe.MatchString(e.slug) {
				shown = append(shown, e)
			}
		}
		if len(shown) == 0 {
			r.sys(tr("pickers.pet.noneAvailable"))
			return nil
		}
		rank := func(e petEntry) int {
			n := 0
			if g.enabled && e.slug == g.active {
				n += 4
			}
			if e.installed {
				n += 2
			}
			if e.curated {
				n++
			}
			return n
		}
		sort.SliceStable(shown, func(i, j int) bool { return rank(shown[i]) > rank(shown[j]) })
		items := make([]wm.Item, 0, len(shown))
		for _, e := range shown {
			mark := " "
			switch {
			case g.enabled && e.slug == g.active:
				mark = "●"
			case e.installed:
				mark = "✓"
			}
			detail := e.slug
			if !e.installed && e.curated {
				detail += tr("pickers.pet.officialTag")
			}
			slug := e.slug
			items = append(items, wm.Item{ID: "pet:" + slug, Title: mark + " " + e.name, Detail: detail, Run: func() tea.Cmd { return a.adoptPet(slug) }})
		}
		count := tr("pickers.pet.petCountOther", len(shown))
		if len(shown) == 1 {
			count = tr("pickers.pet.petCountOne", 1)
		}
		picker := wm.NewPalette(a.toolkit, items)
		picker.Placeholder = tr("pickers.pet.title") + " · " + count
		return a.wm.ShowOverlay(picker)
	}))
}

// Adopts the pet picked in the gallery, it lighting up once the pet view takes the new selection
func (a *app) adoptPet(slug string) tea.Cmd {
	return tea.Batch(wm.Notify(tr("pickers.pet.adopting"), wm.Info), gatewayCall(a.gw, "pet.select", map[string]any{"slug": slug}, func(_ any, err error) tea.Cmd {
		if err != nil {
			a.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
			return nil
		}
		return a.pet.refresh()
	}))
}
