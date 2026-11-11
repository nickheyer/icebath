package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	petManifestURL     = "https://petdex.dev/api/manifest"
	petManifestTimeout = 10 * time.Second
	petManifestTTL     = 300 * time.Second
	petDownloadTimeout = 60 * time.Second
	petUserAgent       = "icebath-agent-petdex"
)

// One pet row of the public petdex manifest
type petManifestEntry struct {
	slug, displayName, kind, submittedBy, spritesheetURL, petJSONURL, zipURL string
}

// Raised when the manifest cannot be fetched or parsed
type petManifestError struct{ msg string }

func (e *petManifestError) Error() string      { return e.msg }
func (e *petManifestError) PyTypeName() string { return "ManifestError" }

var petManifestCache struct {
	mu      sync.Mutex
	at      time.Time
	entries []petManifestEntry
	warm    bool
}

// Manifest row from its dict with the display name, kind and URLs defaulted
func petManifestEntryOf(d *pyjson.Object) petManifestEntry {
	text := func(key string) string { return pyrepr.Str(d.GetOr(key, "")) }
	orText := func(key string, fallback any) string {
		v := d.GetOr(key, "")
		if !pyrepr.Truthy(v) {
			v = fallback
		}
		return pyrepr.Str(v)
	}
	return petManifestEntry{slug: pystr.Strip(text("slug")), displayName: orText("displayName", d.GetOr("slug", "")),
		kind: orText("kind", "pet"), submittedBy: orText("submittedBy", ""), spritesheetURL: orText("spritesheetUrl", ""),
		petJSONURL: orText("petJsonUrl", ""), zipURL: orText("zipUrl", "")}
}

// SSRF vetted GET following redirects with the petdex user agent, raising on HTTP errors
func petHTTPGet(ctx context.Context, env *tools.Env, rawURL string, timeout time.Duration) (*pyhttpx.Response, error) {
	if !tools.IsSafeURL(env, rawURL) {
		return nil, &appValueError{"Pet store URL failed the SSRF safety check: " + rawURL}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", petUserAgent)
	resp, err := pyhttpx.Do(tools.NewSSRFSafeClient(env, timeout, true), req, timeout)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, errors.New(resp.StatusErrorText(rawURL))
	}
	return resp, nil
}

// Every approved pet of the public manifest, cached for five minutes unless forced
func petFetchManifest(ctx context.Context, env *tools.Env, force bool) ([]petManifestEntry, error) {
	petManifestCache.mu.Lock()
	if !force && petManifestCache.warm && time.Since(petManifestCache.at) < petManifestTTL {
		entries := petManifestCache.entries
		petManifestCache.mu.Unlock()
		return entries, nil
	}
	petManifestCache.mu.Unlock()
	payload, err := func() (any, error) {
		if !tools.IsSafeURL(env, petManifestURL) {
			return nil, &petManifestError{"Pet manifest URL failed the SSRF safety check: " + petManifestURL}
		}
		resp, err := petHTTPGet(ctx, env, petManifestURL, petManifestTimeout)
		if err != nil {
			return nil, err
		}
		return resp.JSON()
	}()
	if err != nil {
		return nil, &petManifestError{"could not fetch petdex manifest: " + petErrText(err)}
	}
	var pets []any
	if obj, ok := payload.(*pyjson.Object); ok {
		pets, _ = obj.GetOr("pets", nil).([]any)
	}
	if pets == nil {
		return nil, &petManifestError{"petdex manifest had no 'pets' array"}
	}
	entries := []petManifestEntry{}
	for _, raw := range pets {
		if d, ok := raw.(*pyjson.Object); ok {
			if e := petManifestEntryOf(d); e.slug != "" && e.spritesheetURL != "" {
				entries = append(entries, e)
			}
		}
	}
	petManifestCache.mu.Lock()
	petManifestCache.at, petManifestCache.entries, petManifestCache.warm = time.Now(), entries, true
	petManifestCache.mu.Unlock()
	return entries, nil
}

// str of a fetch failure, the httpx text for transport errors
func petErrText(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return pyhttpx.ErrorText(err)
	}
	return pyrepr.ExceptionMessage(err)
}

// Manifest entry of a slug compared case insensitively, nil when not listed
func petFindEntry(ctx context.Context, env *tools.Env, slug string) (*petManifestEntry, error) {
	slug = pystr.Lower(pystr.Strip(slug))
	entries, err := petFetchManifest(ctx, env, false)
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if pystr.Lower(entries[i].slug) == slug {
			return &entries[i], nil
		}
	}
	return nil, nil
}

// Whether a URL host is petdex.dev or one of its subdomains
func petIsPetdexHost(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := pystr.Lower(u.Hostname())
	return host == "petdex.dev" || strings.HasSuffix(host, ".petdex.dev")
}

// One bare path segment of a slug, empty when it has none
func petSafeSlug(slug string) string {
	segment := filepath.Base(pystr.Strip(slug))
	if segment == "." || segment == ".." || segment == string(filepath.Separator) || pystr.Strip(slug) == "" {
		return ""
	}
	return segment
}

// Installed pet of a slug only when its spritesheet exists, the failure text as a store error otherwise
func petUsable(h, slug, failure string) (*pet.InstalledPet, error) {
	p, err := pet.LoadPet(h, slug)
	if err != nil {
		return nil, err
	}
	if p != nil && p.Exists() {
		return p, nil
	}
	if failure != "" {
		return nil, &pet.StoreError{Msg: failure}
	}
	return nil, nil
}

// Streams a URL to a file through a part file so a failed download never leaves a truncated sheet
func petDownload(ctx context.Context, env *tools.Env, rawURL, dest string) error {
	err := func() error {
		resp, err := petHTTPGet(ctx, env, rawURL, petDownloadTimeout)
		if err != nil {
			return err
		}
		tmp := dest + ".part"
		if err := os.WriteFile(tmp, resp.Content, 0o666); err != nil {
			return err
		}
		return os.Rename(tmp, dest)
	}()
	if err != nil {
		return &pet.StoreError{Msg: "download failed for " + rawURL + ": " + petErrText(err)}
	}
	return nil
}

// Downloads a manifest pet into the pets dir, idempotent unless forced
func petInstall(ctx context.Context, env *tools.Env, h, slug string, force bool) (*pet.InstalledPet, error) {
	slug = petSafeSlug(slug)
	if slug == "" {
		return nil, &pet.StoreError{Msg: "invalid pet slug"}
	}
	if !force {
		if existing, err := petUsable(h, slug, ""); err != nil || existing != nil {
			return existing, err
		}
	}
	entry, err := petFindEntry(ctx, env, slug)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, &pet.StoreError{Msg: "pet '" + slug + "' is not in the petdex manifest"}
	}
	if !petIsPetdexHost(entry.spritesheetURL) {
		return nil, &pet.StoreError{Msg: "refusing non-petdex spritesheet host for '" + slug + "'"}
	}
	root, err := pet.PetsDir(h)
	if err != nil {
		return nil, err
	}
	directory := filepath.Join(root, slug)
	if err := os.MkdirAll(directory, 0o777); err != nil {
		return nil, err
	}
	ext := ".webp"
	if head, _, _ := strings.Cut(pystr.Lower(entry.spritesheetURL), "?"); strings.HasSuffix(head, ".png") {
		ext = ".png"
	}
	spritePath := filepath.Join(directory, "spritesheet"+ext)
	if err := petDownload(ctx, env, entry.spritesheetURL, spritePath); err != nil {
		return nil, err
	}
	meta := pyjson.NewObject()
	if entry.petJSONURL != "" && petIsPetdexHost(entry.petJSONURL) {
		if resp, err := petHTTPGet(ctx, env, entry.petJSONURL, petDownloadTimeout); err == nil {
			if data, err := resp.JSON(); err == nil {
				if d, ok := data.(*pyjson.Object); ok {
					meta = d
				}
			}
		}
	}
	if meta.Len() == 0 {
		meta = pyjson.ObjectOf("id", slug, "displayName", entry.displayName, "description", "")
	}
	meta.Set("spritesheetPath", filepath.Base(spritePath))
	if _, ok := meta.Get("id"); !ok {
		meta.Set("id", slug)
	}
	if _, ok := meta.Get("displayName"); !ok {
		meta.Set("displayName", entry.displayName)
	}
	text, err := pyjson.Dumps(meta, pyjson.Indent(2))
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(directory, "pet.json"), []byte(text), 0o666); err != nil {
		return nil, err
	}
	return petUsable(h, slug, "install of '"+slug+"' did not produce a spritesheet")
}

// Deletes an installed pet and its cached thumbnail, true when the directory is gone
func petRemove(h, slug string) (bool, error) {
	slug = petSafeSlug(slug)
	if slug == "" {
		return false, nil
	}
	root, err := pet.PetsDir(h)
	if err != nil {
		return false, err
	}
	if thumbs := filepath.Join(root, ".thumbs"); os.MkdirAll(thumbs, 0o777) == nil {
		os.Remove(filepath.Join(thumbs, slug+".png"))
	}
	directory := filepath.Join(root, slug)
	if st, err := os.Stat(directory); err != nil || !st.IsDir() {
		return false, nil
	}
	os.RemoveAll(directory)
	_, err = os.Stat(directory)
	return err != nil, nil
}

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

// display.pet config dict, empty when absent or not a dict
func (r *petsRun) petConfig() (map[string]any, error) {
	cfg, err := config.LoadContext(r.ctx, r.h, nil)
	if err != nil {
		return nil, err
	}
	display, _ := cfg["display"].(map[string]any)
	petCfg, _ := display["pet"].(map[string]any)
	if petCfg == nil {
		petCfg = map[string]any{}
	}
	return petCfg, nil
}

// Bool coercion over the shared truthy strings, the default for None
func petTruthy(v any, def bool) bool {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		switch pystr.Lower(pystr.Strip(x)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	return pyrepr.Truthy(v)
}

// Whether display.pet.enabled is truthy
func petEnabled(cfg map[string]any) bool { return petTruthy(cfg["enabled"], false) }

// str of a config value or empty when falsy
func petCfgText(cfg map[string]any, key string) string {
	if v := cfg[key]; pyrepr.Truthy(v) {
		return pyrepr.Str(v)
	}
	return ""
}

// Writes display.pet keys and saves config, only when the configured slug matches whenSlug if given
func (r *petsRun) updatePetConfig(whenSlug *string, values ...any) (bool, error) {
	cfg, err := config.LoadContext(r.ctx, r.h, nil)
	if err != nil {
		return false, err
	}
	display, ok := cfg["display"].(map[string]any)
	if _, present := cfg["display"]; !present {
		display, ok = map[string]any{}, true
		cfg["display"] = display
	}
	if !ok {
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(cfg["display"]) + "' object has no attribute 'setdefault'"}
	}
	petVal, present := display["pet"]
	if !present {
		petVal = map[string]any{}
		display["pet"] = petVal
	}
	petCfg, isDict := petVal.(map[string]any)
	if whenSlug != nil && (!isDict || petCfgText(petCfg, "slug") != *whenSlug) {
		return false, nil
	}
	if !isDict {
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(petVal) + "' object has no attribute 'update'"}
	}
	for i := 0; i+1 < len(values); i += 2 {
		petCfg[values[i].(string)] = values[i+1]
	}
	if err := config.SaveConfig(r.ctx, r.h, cfg, config.SaveOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

func (r *petsRun) setActive(slug string) error {
	_, err := r.updatePetConfig(nil, "slug", slug, "enabled", true)
	return err
}

// Whether a pet is enabled and a slug is configured
func (r *petsRun) hasActivePet() (bool, error) {
	cfg, err := r.petConfig()
	if err != nil {
		return false, err
	}
	return petEnabled(cfg) && pyrepr.Truthy(cfg["slug"]), nil
}

// Persists the clamped display.pet.scale, a message and nothing written when the value is not a number
func (r *petsRun) setPetScale(value any) (float64, string, error) {
	f, err := pyint.Float(value)
	if err != nil {
		return 0, "not a number: " + pyrepr.Repr(value) + " — try a value like 0.5", nil
	}
	scale := pet.ClampScale(f)
	if _, err := r.updatePetConfig(nil, "scale", scale); err != nil {
		return 0, "", err
	}
	return scale, "", nil
}

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
	entries, err := petFetchManifest(r.ctx, r.env, false)
	if err != nil {
		var me *petManifestError
		if errors.As(err, &me) {
			r.errf("✗ %s", me.msg)
			return 1, nil
		}
		return 1, err
	}
	query := pystr.Lower(pystr.Strip(nsText(r.ns, "query")))
	if query != "" {
		var filtered []petManifestEntry
		for _, e := range entries {
			if strings.Contains(pystr.Lower(e.slug), query) || strings.Contains(pystr.Lower(e.displayName), query) {
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
		matching = " matching " + pyrepr.Repr(query)
	}
	r.printf("petdex gallery — %d pet(s)%s:", len(entries), matching)
	for _, e := range shown {
		mark := " "
		if installed[e.slug] {
			mark = "✓"
		}
		r.printf("  %s %s %s  (%s)", mark, padRight(e.slug, 28), e.displayName, e.kind)
	}
	if limit != 0 && len(entries) > limit {
		r.printf("  … %d more (use --limit 0 or --query to filter)", len(entries)-limit)
	}
	r.printf("\nInstall one with: icebath pets install <slug>")
	return 0, nil
}

func (r *petsRun) cmdInstall() (int, error) {
	slug := pystr.Strip(nsText(r.ns, "slug"))
	p, err := petInstall(r.ctx, r.env, r.h, slug, nsTruthy(r.ns, "force"))
	if err != nil {
		var se *pet.StoreError
		var me *petManifestError
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
	slug := pystr.Strip(nsText(r.ns, "slug"))
	removed, err := petRemove(r.h, slug)
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
	raw := pystr.Strip(answer)
	if raw == "" {
		raw = "1"
	}
	n, ok := pystr.IntText(raw)
	if err != nil || !ok {
		r.errf("✗ cancelled")
		return ""
	}
	if n.IsInt64() && n.Int64()-1 >= 0 && n.Int64()-1 < int64(len(pets)) {
		return pets[n.Int64()-1].Slug
	}
	r.errf("✗ invalid selection")
	return ""
}

func (r *petsRun) cmdSelect() (int, error) {
	slug := pystr.Strip(nsText(r.ns, "slug"))
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
	shown, err := pyrepr.Format(scale, "g")
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
	slug := pystr.Strip(nsText(r.ns, "slug"))
	if slug == "" {
		slug = petCfgText(cfg, "slug")
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
		modeCfg = petCfgText(cfg, "render_mode")
		if modeCfg == "" {
			modeCfg = "auto"
		}
	}
	var scaleVal any = r.ns["scale"]
	if !pyrepr.Truthy(scaleVal) {
		scaleVal = cfg["scale"]
		if _, present := cfg["scale"]; !present {
			scaleVal = pet.DefaultScale
		}
		if !pyrepr.Truthy(scaleVal) {
			scaleVal = pet.DefaultScale
		}
	}
	scale, err := pyint.Float(scaleVal)
	if err != nil {
		return 1, err
	}
	unicodeCols := 0
	if v, ok := cfg["unicode_cols"]; ok && v != nil {
		n, err := pyint.Int64(v)
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
	requested := pystr.Lower(pystr.Strip(nsText(r.ns, "state")))
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
	termCols, _ := pyshutil.GetTerminalSize(80, 24)
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
	enabled := petEnabled(cfg)
	configuredSlug := petCfgText(cfg, "slug")
	modeCfg := petCfgText(cfg, "render_mode")
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
	r.printf("  display.pet.enabled:     %s", pyrepr.Str(enabled))
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
	r := &petsRun{ctx: ctx, h: h, ns: ns, w: w, env: &tools.Env{Home: h, Config: cfg, Cwd: cwd, Log: slog.Default()}}
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
	return 1, fmt.Errorf("unknown pets command %s", pyrepr.Repr(ns["pets_command"]))
}

// Flips display.pet.enabled, adopting the first installed pet when none resolves
func (r *petsRun) togglePetDisplay() (bool, *string, *string, error) {
	cfg, err := r.petConfig()
	if err != nil {
		return false, nil, nil, err
	}
	p, err := pet.ResolveActivePet(r.h, petCfgText(cfg, "slug"))
	if err != nil {
		return false, nil, nil, err
	}
	if petEnabled(cfg) {
		if _, err := r.updatePetConfig(nil, "enabled", false); err != nil {
			return false, nil, nil, err
		}
		if p != nil {
			return false, &p.DisplayName, nil, nil
		}
		return false, nil, nil, nil
	}
	if p == nil {
		installed, err := pet.InstalledPets(r.h)
		if err != nil {
			return false, nil, nil, err
		}
		if len(installed) == 0 {
			msg := "no pets installed — /pet list to browse, or /pet <slug> to adopt"
			return false, nil, &msg, nil
		}
		p = installed[0]
		if err := r.setActive(p.Slug); err != nil {
			return false, nil, nil, err
		}
	} else if _, err := r.updatePetConfig(nil, "enabled", true); err != nil {
		return false, nil, nil, err
	}
	return true, &p.DisplayName, nil, nil
}

// Prints a slice of the public petdex gallery as the CLI and TUI text fallback
func (r *petsRun) printPetGallery(limit int) error {
	entries, err := petFetchManifest(r.ctx, r.env, false)
	if err != nil {
		var me *petManifestError
		if errors.As(err, &me) {
			r.printf("(._.) Couldn't reach the petdex gallery: %s", me.msg)
			return nil
		}
		return err
	}
	installedPets, err := pet.InstalledPets(r.h)
	if err != nil {
		return err
	}
	installed := map[string]bool{}
	for _, p := range installedPets {
		installed[p.Slug] = true
	}
	shown := entries
	if limit > 0 && len(entries) > limit {
		shown = entries[:limit]
	}
	r.printf("(^o^)/ petdex gallery — first %d of %d:", len(shown), len(entries))
	for _, e := range shown {
		mark := "○"
		if installed[e.slug] {
			mark = "●"
		}
		r.printf("  %s %s %s", mark, padRight(e.slug, 24), e.displayName)
	}
	r.printf("  /pet <slug> to adopt · /pet to toggle")
	return nil
}

// Disables and unsets the active pet only when it is the given slug
func (r *petsRun) clearActiveIf(slug string) (bool, error) {
	return r.updatePetConfig(&slug, "slug", "", "enabled", false)
}

// Repoints the active pet to a renamed slug only when the old slug is active, keeping enabled
func (r *petsRun) renameActiveIf(oldSlug, newSlug string) (bool, error) {
	if newSlug == "" || oldSlug == newSlug {
		return false, nil
	}
	return r.updatePetConfig(&oldSlug, "slug", newSlug)
}
