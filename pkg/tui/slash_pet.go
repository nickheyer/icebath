package tui

import (
	"cmp"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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
func parsePetGallery(res *tuigateway.PetGalleryResult) petGallery {
	g := petGallery{enabled: res.Enabled, active: res.Active}
	for _, entry := range res.Pets {
		if entry.Slug == "" {
			continue
		}
		g.pets = append(g.pets, petEntry{slug: entry.Slug, name: cmp.Or(entry.DisplayName, entry.Slug), installed: entry.Installed, curated: entry.Curated})
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

// Typed pet verb of a command, its failure or misfit said in the pet's words, its answer dropped once the run is stale
func petCall[P, R any](r *slashRun, m tuigateway.Method[P, R], params P, then func(*R) tea.Cmd) tea.Cmd {
	return gatewayCall(r.a.gw, m.Name(), wireParams(params), func(v any, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		if err != nil {
			r.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
			return nil
		}
		res, misfit := m.Result(v)
		if misfit != nil {
			r.sys(tr("slashCmd.session.pet.error", misfitText(tr("pickers.common.invalidResponse", m.Name()))+" "+misfit.Error()))
			return nil
		}
		return then(&res)
	})
}

// Gallery of the installed pets only, the remote manifest skipped
var localPets = tuigateway.PetGalleryParams{LocalOnly: true}

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
		return petCall(r, tuigateway.PetDisable, tuigateway.ProfileParams{}, func(*tuigateway.PetOkResult) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.putAway"))
			return nil
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

// Flips the display, putting the shown pet away or turning it on with the configured slug kept
func (a *app) petToggle(r *slashRun) tea.Cmd {
	return petCall(r, tuigateway.PetGallery, localPets, func(res *tuigateway.PetGalleryResult) tea.Cmd {
		g := parsePetGallery(res)
		cur := g.current()
		if g.enabled {
			return petCall(r, tuigateway.PetDisable, tuigateway.ProfileParams{}, func(*tuigateway.PetOkResult) tea.Cmd {
				if cur != nil {
					r.sys(tr("slashCmd.session.pet.putAwayNamed", cur.name))
				} else {
					r.sys(tr("slashCmd.session.pet.putAway"))
				}
				return nil
			})
		}
		if cur == nil {
			r.sys(tr("slashCmd.session.pet.error", tr("slashCmd.session.pet.noneInstalled")))
			return nil
		}
		return petCall(r, tuigateway.PetEnable, tuigateway.ProfileParams{}, func(*tuigateway.PetOkResult) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.isOut", cur.name))
			return nil
		})
	})
}

// Adopts a petdex pet by slug, installing it first when needed, a store refusal said as the CLI says it
func (a *app) petSelect(r *slashRun, slug string) tea.Cmd {
	r.sys(tr("slashCmd.session.pet.fetching", slug))
	return call(r.a.gw, tuigateway.PetSelect, tuigateway.PetSlugParams{Slug: slug}, func(res *tuigateway.PetSlugResult, err error) tea.Cmd {
		if r.stale() {
			return nil
		}
		var refusal *rpcError
		prefix := "could not adopt '" + slug + "': "
		if errors.As(err, &refusal) && refusal.Code == 5031 && strings.HasPrefix(refusal.Message, prefix) {
			r.sys(tr("slashCmd.session.pet.adoptFailed", slug, strings.TrimPrefix(refusal.Message, prefix)))
			return nil
		}
		if err != nil {
			r.sys(tr("slashCmd.session.pet.error", rpcErrorMessage(err)))
			return nil
		}
		r.sys(tr("slashCmd.session.pet.isOut", cmp.Or(res.DisplayName, slug)))
		return nil
	})
}

// Persists the pet scale the gateway clamps to the engine bounds
func (a *app) petScale(r *slashRun, value string) tea.Cmd {
	if value == "" {
		r.sys(tr("slashCmd.session.pet.usageScale"))
		return nil
	}
	return petCall(r, tuigateway.PetScale, tuigateway.PetScaleParams{Scale: value}, func(res *tuigateway.PetScaleResult) tea.Cmd {
		shown := strconv.FormatFloat(res.Scale, 'g', 6, 64)
		r.sys(tr("slashCmd.session.pet.scaleSet", shown))
		return nil
	})
}

// Renames an installed pet, the named slug when the first word is one, else the pet the display resolves
func (a *app) petRename(r *slashRun, rest string) tea.Cmd {
	return petCall(r, tuigateway.PetGallery, localPets, func(res *tuigateway.PetGalleryResult) tea.Cmd {
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
		return petCall(r, tuigateway.PetRename, tuigateway.PetRenameParams{Slug: target.slug, Name: name}, func(out *tuigateway.PetSlugResult) tea.Cmd {
			r.sys(tr("slashCmd.session.pet.renamed", target.name, name, cmp.Or(out.Slug, target.slug)))
			return nil
		})
	})
}

// Uninstalls a pet after a confirmation, the named one or the pet the display resolves
func (a *app) petRemove(r *slashRun, slug string) tea.Cmd {
	return petCall(r, tuigateway.PetGallery, localPets, func(res *tuigateway.PetGalleryResult) tea.Cmd {
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
				return petCall(r, tuigateway.PetRemove, tuigateway.PetSlugParams{Slug: target.slug}, func(out *tuigateway.PetSlugResult) tea.Cmd {
					if out.Ok {
						r.sys(tr("slashCmd.session.pet.removed", target.name))
					} else {
						r.sys(tr("slashCmd.session.pet.notInstalled", target.slug))
					}
					return nil
				})
			})
	})
}

// Writes the zip of a pet to a path, the pet named first or else the pet the display resolves
func (a *app) petExport(r *slashRun, rest string) tea.Cmd {
	return petCall(r, tuigateway.PetGallery, localPets, func(res *tuigateway.PetGalleryResult) tea.Cmd {
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
		return petCall(r, tuigateway.PetExport, tuigateway.PetSlugParams{Slug: target.slug}, func(out *tuigateway.PetExportResult) tea.Cmd {
			data, err := base64.StdEncoding.DecodeString(out.ZipBase64)
			if err != nil {
				r.sys(tr("slashCmd.session.pet.error", err.Error()))
				return nil
			}
			dest, err := a.exportPath(path, cmp.Or(out.Filename, target.slug+".zip"))
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
	return petCall(r, tuigateway.PetInfoMeta, tuigateway.ProfileParams{}, func(meta *tuigateway.PetInfoMetaResult) tea.Cmd {
		if !meta.Enabled {
			r.sys(tr("slashCmd.session.pet.infoOff"))
			return nil
		}
		known := tuigateway.PetInfoParams{KnownRevision: meta.SpritesheetRevision}
		return petCall(r, tuigateway.PetInfo, known, func(info *tuigateway.PetInfoResult) tea.Cmd {
			if !info.Enabled {
				r.sys(tr("slashCmd.session.pet.infoOff"))
				return nil
			}
			shown := strconv.FormatFloat(info.Scale, 'g', 6, 64)
			var frames []string
			for _, state := range pet.States {
				if n, has := info.FramesByState[string(state)]; has {
					frames = append(frames, fmt.Sprintf("%s %d", state, n))
				}
			}
			rows := [][2]string{
				{tr("slashCmd.session.pet.infoSlug"), info.Slug},
				{tr("slashCmd.session.pet.infoScale"), shown},
				{tr("slashCmd.session.pet.infoSheet"), fmt.Sprintf("%s · %d×%d", info.Mime, info.FrameW, info.FrameH)},
				{tr("slashCmd.session.pet.infoFrames"), strings.Join(frames, " · ")},
				{tr("slashCmd.session.pet.infoRows"), strings.Join(info.StateRows, " · ")},
				{tr("slashCmd.session.pet.infoRevision"), info.SpritesheetRevision},
				{tr("slashCmd.session.pet.infoDrawing"), a.pet.drawing()},
			}
			a.panel(cmp.Or(info.DisplayName, info.Slug), []panelSection{{Rows: rows}})
			return nil
		})
	})
}

// Gallery picker of pet.gallery, typed text narrowing it and enter adopting the pet under the cursor
func (a *app) openPetGallery(r *slashRun) tea.Cmd {
	return tea.Batch(wm.Notify(tr("pickers.pet.loading"), wm.Info), petCall(r, tuigateway.PetGallery, tuigateway.PetGalleryParams{}, func(res *tuigateway.PetGalleryResult) tea.Cmd {
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
		byID := map[string]petEntry{}
		for _, e := range shown {
			byID["pet:"+e.slug] = e
		}
		picker := wm.NewPalette(a.toolkit, items)
		picker.Placeholder = tr("pickers.pet.title") + " · " + count
		picker.Filter = func(query string, it wm.Item) bool {
			needle, e := strings.ToLower(trimJS(query)), byID[it.ID]
			return needle == "" || strings.Contains(strings.ToLower(e.slug), needle) || strings.Contains(strings.ToLower(e.name), needle)
		}
		return a.wm.ShowOverlay(picker)
	}))
}

// Adopts the pet picked in the gallery, it lighting up once the pet view takes the new selection
func (a *app) adoptPet(slug string) tea.Cmd {
	return tea.Batch(wm.Notify(tr("pickers.pet.adopting"), wm.Info), call(a.gw, tuigateway.PetSelect, tuigateway.PetSlugParams{Slug: slug}, func(_ *tuigateway.PetSlugResult, err error) tea.Cmd {
		if err != nil {
			a.sys(tr("pickers.common.error", rpcErrorMessage(err)))
		}
		return nil
	}))
}
