//go:build !icebath_core && !icebath_noextras

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	templateutil "github.com/nickheyer/icebath/pkg/utils/template"
	termutil "github.com/nickheyer/icebath/pkg/utils/term"
	textutil "github.com/nickheyer/icebath/pkg/utils/text"
)

// One icebath pets invocation with its home, namespace, output and tool environment
type petsRun struct {
	ctx context.Context
	h   string
	ns  map[string]any
	w   io.Writer
	env *tools.Env
}

func (r *petsRun) printf(format string, a ...any) { fmt.Fprintf(r.w, format+"\n", a...) }
func (r *petsRun) errf(format string, a ...any)   { fmt.Fprintf(stderrWriter(), format+"\n", a...) }

// Shared pet operations bound to this invocation
func (r *petsRun) core() *pet.Run { return &pet.Run{Ctx: r.ctx, Home: r.h, W: r.w, Env: r.env} }

func (r *petsRun) petConfig() (map[string]any, error) { return r.core().PetConfig() }
func (r *petsRun) setActive(slug string) error        { return r.core().SetActive(slug) }
func (r *petsRun) hasActivePet() (bool, error)        { return r.core().HasActivePet() }
func (r *petsRun) updatePetConfig(whenSlug *string, values ...any) (bool, error) {
	return r.core().UpdatePetConfig(whenSlug, values...)
}
func (r *petsRun) setPetScale(value any) (float64, string, error) { return r.core().SetPetScale(value) }
func (r *petsRun) cmdList() (int, error) {
	if nsTruthy(r.ns, "installed") {
		pets, err := pet.InstalledPets(r.h)
		if err != nil {
			return 1, err
		}
		if len(pets) == 0 {
			r.printf("No pets installed. Try: icebath pets install boba")
			return 0, nil
		}
		r.printf("Installed pets (%d):", len(pets))
		for _, p := range pets {
			r.printf("  %s %s", padRight(p.Slug, 24), p.DisplayName)
		}
		return 0, nil
	}
	entries, err := pet.FetchManifest(r.ctx, r.env, false)
	if err != nil {
		var me *pet.ManifestError
		if errors.As(err, &me) {
			r.errf("✗ %s", me.Msg)
			return 1, nil
		}
		return 1, err
	}
	query := strings.ToLower(strings.TrimSpace(nsText(r.ns, "query")))
	if query != "" {
		var filtered []pet.ManifestEntry
		for _, e := range entries {
			if strings.Contains(strings.ToLower(e.Slug), query) || strings.Contains(strings.ToLower(e.DisplayName), query) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	limit, _ := nsInt(r.ns, "limit")
	shown := entries
	if limit > 0 && len(entries) > limit {
		shown = entries[:limit]
	}
	installedPets, err := pet.InstalledPets(r.h)
	if err != nil {
		return 1, err
	}
	installed := map[string]bool{}
	for _, p := range installedPets {
		installed[p.Slug] = true
	}
	matching := ""
	if query != "" {
		matching = " matching " + errchain.Show(query)
	}
	r.printf("petdex gallery — %d pet(s)%s:", len(entries), matching)
	for _, e := range shown {
		mark := " "
		if installed[e.Slug] {
			mark = "✓"
		}
		r.printf("  %s %s %s  (%s)", mark, padRight(e.Slug, 28), e.DisplayName, e.Kind)
	}
	if limit != 0 && len(entries) > limit {
		r.printf("  … %d more (use --limit 0 or --query to filter)", len(entries)-limit)
	}
	r.printf("\nInstall one with: icebath pets install <slug>")
	return 0, nil
}

func (r *petsRun) cmdInstall() (int, error) {
	slug := strings.TrimSpace(nsText(r.ns, "slug"))
	p, err := pet.InstallPet(r.ctx, r.env, r.h, slug, nsTruthy(r.ns, "force"))
	if err != nil {
		var se *pet.StoreError
		var me *pet.ManifestError
		if errors.As(err, &se) || errors.As(err, &me) {
			r.errf("✗ install failed: %s", err.Error())
			return 1, nil
		}
		return 1, err
	}
	r.printf("✓ installed %s → %s", p.DisplayName, p.Directory)
	active, err := r.hasActivePet()
	if err != nil {
		return 1, err
	}
	if nsTruthy(r.ns, "select") || !active {
		if err := r.setActive(slug); err != nil {
			return 1, err
		}
		r.printf("✓ %s is now the active pet (display.pet.slug=%s, enabled)", p.DisplayName, slug)
	} else {
		r.printf("  Make it active with: icebath pets select %s", slug)
	}
	return 0, nil
}

func (r *petsRun) cmdRemove() (int, error) {
	slug := strings.TrimSpace(nsText(r.ns, "slug"))
	removed, err := pet.RemovePet(r.h, slug)
	if err != nil {
		return 1, err
	}
	if removed {
		r.printf("✓ removed %s", slug)
		return 0, nil
	}
	r.errf("✗ '%s' is not installed", slug)
	return 1, nil
}

// Numbered picker over installed pets, empty on cancel or an invalid choice
func (r *petsRun) interactivePick(pets []*pet.InstalledPet) string {
	r.printf("Installed pets:")
	for i, p := range pets {
		r.printf("  %d. %s %s", i+1, padRight(p.Slug, 24), p.DisplayName)
	}
	con := console()
	prompter := &cliui.Console{In: con.In, Out: r.w, Getenv: con.Getenv, StdinTTY: con.StdinIsTTY, StdoutTTY: con.StdoutIsTTY}
	answer, err := prompter.Input("Select a pet [1]: ")
	raw := strings.TrimSpace(answer)
	if raw == "" {
		raw = "1"
	}
	n, err := textutil.ParseInt(raw)
	if err != nil || err != nil {
		r.errf("✗ cancelled")
		return ""
	}
	if n-1 >= 0 && n-1 < int64(len(pets)) {
		return pets[n-1].Slug
	}
	r.errf("✗ invalid selection")
	return ""
}

func (r *petsRun) cmdSelect() (int, error) {
	slug := strings.TrimSpace(nsText(r.ns, "slug"))
	if slug == "" {
		pets, err := pet.InstalledPets(r.h)
		if err != nil {
			return 1, err
		}
		if len(pets) == 0 {
			r.errf("✗ no pets installed — run: icebath pets install boba")
			return 1, nil
		}
		if slug = r.interactivePick(pets); slug == "" {
			return 1, nil
		}
	}
	p, err := pet.LoadPet(r.h, slug)
	if err != nil {
		return 1, err
	}
	if p == nil || !p.Exists() {
		r.errf("✗ '%s' is not installed — run: icebath pets install %s", slug, slug)
		return 1, nil
	}
	if err := r.setActive(slug); err != nil {
		return 1, err
	}
	r.printf("✓ active pet set to %s (display.pet.slug=%s, enabled)", p.DisplayName, slug)
	return 0, nil
}

func (r *petsRun) cmdOff() (int, error) {
	if _, err := r.updatePetConfig(nil, "enabled", false); err != nil {
		return 1, err
	}
	r.printf("✓ pet disabled (display.pet.enabled=false)")
	return 0, nil
}

func (r *petsRun) cmdScale() (int, error) {
	scale, msg, err := r.setPetScale(r.ns["factor"])
	if err != nil {
		return 1, err
	}
	if msg != "" {
		r.errf("✗ %s", msg)
		return 1, nil
	}
	shown, err := templateutil.Format("{v:g}", map[string]any{"v": scale})
	if err != nil {
		return 1, err
	}
	r.printf("✓ pet scale set to %s (display.pet.scale)", shown)
	return 0, nil
}

// Animates the active or named pet in the terminal until Ctrl+C, or once through the states
func (r *petsRun) cmdShow() (int, error) {
	cfg, err := r.petConfig()
	if err != nil {
		return 1, err
	}
	slug := strings.TrimSpace(nsText(r.ns, "slug"))
	if slug == "" {
		slug = pet.CfgText(cfg, "slug")
	}
	p, err := pet.ResolveActivePet(r.h, slug)
	if err != nil {
		return 1, err
	}
	if p == nil {
		r.errf("✗ no pet to show — run: icebath pets install boba")
		return 1, nil
	}
	modeCfg := nsText(r.ns, "mode")
	if modeCfg == "" {
		modeCfg = pet.CfgText(cfg, "render_mode")
		if modeCfg == "" {
			modeCfg = "auto"
		}
	}
	var scaleVal any = r.ns["scale"]
	if !anyutil.IsTrue(scaleVal) {
		scaleVal = cfg["scale"]
		if _, present := cfg["scale"]; !present {
			scaleVal = pet.DefaultScale
		}
		if !anyutil.IsTrue(scaleVal) {
			scaleVal = pet.DefaultScale
		}
	}
	scale, err := anyutil.ToFloat(scaleVal)
	if err != nil {
		return 1, err
	}
	unicodeCols := 0
	if v, ok := cfg["unicode_cols"]; ok && v != nil {
		n, err := anyutil.ToInt(v)
		if err != nil {
			return 1, err
		}
		unicodeCols = int(n)
	}
	cols := pet.ResolveCols(scale, unicodeCols)
	isTTY := console().StdoutIsTTY()
	renderer := pet.BuildRenderer(p.Spritesheet, modeCfg, scale, cols, isTTY)
	if !renderer.Available() {
		r.errf("✗ cannot render here (no TTY / graphics disabled). Effective mode: %s.", renderer.Mode)
		return 1, nil
	}
	requested := strings.ToLower(strings.TrimSpace(nsText(r.ns, "state")))
	var states []string
	switch {
	case requested != "":
		states = []string{requested}
	case nsTruthy(r.ns, "cycle"):
		driveable := map[string]bool{}
		for _, s := range pet.States {
			driveable[string(s)] = true
		}
		for _, s := range pet.StateRows {
			if driveable[s] {
				states = append(states, s)
			}
		}
	default:
		states = []string{string(pet.StateIdle)}
	}
	isUnicode := renderer.Mode == "unicode"
	frames := max(1, renderer.FrameCount(states[0]))
	frameDelay := max(0.05, (float64(pet.LoopMS)/1000.0)/float64(frames))
	termCols, _ := termutil.Size(80, 24)
	spriteCols := cols
	if !isUnicode {
		spriteCols = max(1, int(float64(renderer.FrameW)*renderer.Scale)/8)
	}
	indent := strings.Repeat(" ", max(0, termCols-spriteCols-1))
	out := r.w
	io.WriteString(out, "\x1b[?25l")
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)
	defer signal.Stop(interrupt)
	defer io.WriteString(out, "\x1b[?25h\x1b[0m\n")
	fmt.Fprintf(out, "%s — mode=%s  (Ctrl+C to stop)\n", p.DisplayName, renderer.Mode)
	prevLines := 0
	for loops := 0; ; {
		for _, state := range states {
			for i := 0; i < max(1, renderer.FrameCount(state)); i++ {
				encoded := renderer.Frame(state, i)
				if isUnicode {
					if indent != "" {
						lines := strings.Split(encoded, "\n")
						for j := range lines {
							lines[j] = indent + lines[j]
						}
						encoded = strings.Join(lines, "\n")
					}
					if prevLines > 0 {
						fmt.Fprintf(out, "\x1b[%dF", prevLines)
					}
					io.WriteString(out, encoded+"\x1b[0m\n")
					prevLines = strings.Count(encoded, "\n") + 1
				} else {
					io.WriteString(out, "\x1b[2J\x1b[3J\x1b[H")
					fmt.Fprintf(out, "%s [%s]\n", p.DisplayName, state)
					io.WriteString(out, indent+encoded+"\n")
				}
				select {
				case <-interrupt:
					return 0, nil
				case <-r.ctx.Done():
					return 0, nil
				case <-time.After(time.Duration(frameDelay * float64(time.Second))):
				}
			}
		}
		loops++
		if nsTruthy(r.ns, "once") && loops >= len(states) {
			return 0, nil
		}
	}
}

// Reports install state, active pet, config and terminal capability
func (r *petsRun) cmdDoctor() (int, error) {
	cfg, err := r.petConfig()
	if err != nil {
		return 1, err
	}
	enabled := pet.Enabled(cfg)
	configuredSlug := pet.CfgText(cfg, "slug")
	modeCfg := pet.CfgText(cfg, "render_mode")
	if modeCfg == "" {
		modeCfg = "auto"
	}
	pets, err := pet.InstalledPets(r.h)
	if err != nil {
		return 1, err
	}
	active, err := pet.ResolveActivePet(r.h, configuredSlug)
	if err != nil {
		return 1, err
	}
	dir, err := pet.PetsDir(r.h)
	if err != nil {
		return 1, err
	}
	slugs := make([]string, 0, len(pets))
	for _, p := range pets {
		slugs = append(slugs, p.Slug)
	}
	installedText := strings.Join(slugs, ", ")
	if installedText == "" {
		installedText = "none"
	}
	slugText := configuredSlug
	if slugText == "" {
		slugText = "(unset)"
	}
	activeText := "(none)"
	if active != nil {
		activeText = active.Slug
	}
	r.printf("petdex doctor")
	r.printf("  pets dir:        %s", dir)
	r.printf("  installed:       %d (%s)", len(pets), installedText)
	r.printf("  display.pet.enabled:     %s", anyutil.String(enabled))
	r.printf("  display.pet.slug:        %s", slugText)
	r.printf("  active (resolved):       %s", activeText)
	r.printf("  display.pet.render_mode: %s", modeCfg)
	r.printf("  detected graphics:       %s", pet.DetectTerminalGraphics())
	r.printf("  effective mode (TTY):    %s", pet.ResolveMode(modeCfg, console().StdoutIsTTY()))
	ok := true
	switch {
	case len(pets) == 0:
		r.printf("  → no pets installed. Run: icebath pets install boba")
		ok = false
	case active == nil:
		r.printf("  → active pet unresolved. Run: icebath pets select <slug>")
		ok = false
	case !enabled:
		r.printf("  → pet display is disabled. Run: icebath pets select " + active.Slug)
	}
	if ok && enabled {
		r.printf("  ✓ ready")
	} else {
		r.printf("  (run the suggestions above to finish setup)")
	}
	return 0, nil
}

// Runs one icebath pets subcommand, help when none was given
func PetsCommand(ctx context.Context, h string, ns map[string]any, w io.Writer) (int, error) {
	cfg, _ := config.LoadReadonly(ctx, h, nil)
	cwd, _ := os.Getwd()
	r := &petsRun{ctx: ctx, h: h, ns: ns, w: w, env: &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: pkgLog()}}
	switch nsText(ns, "pets_command") {
	case "list":
		return r.cmdList()
	case "install":
		return r.cmdInstall()
	case "select":
		return r.cmdSelect()
	case "show":
		return r.cmdShow()
	case "off":
		return r.cmdOff()
	case "scale":
		return r.cmdScale()
	case "remove":
		return r.cmdRemove()
	case "doctor":
		return r.cmdDoctor()
	}
	return 1, fmt.Errorf("unknown pets command %s", errchain.Show(ns["pets_command"]))
}
