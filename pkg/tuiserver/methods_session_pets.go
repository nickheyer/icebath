package tuiserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/pet/generate"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

var petMethodsRegistry = &handlerRegistry{}

// Payload a disabled pet surface answers with
func petOff(map[string]any) map[string]any { return map[string]any{"enabled": false} }

func init() {
	petMethod("pet.info", petOff, false, true, petInfo)
	petMethod("pet.info.meta", petOff, false, true, petInfoMeta)
	petMethod("pet.cells", petOff, false, true, petCells)
	petMethod("pet.gallery", func(map[string]any) map[string]any {
		return map[string]any{"enabled": false, "active": "", "pets": []any{}}
	}, false, true, petGallery)
	petMethod("pet.select", nil, true, true, petSelect)
	petMethod("pet.remove", nil, true, true, petRemove)
	petMethod("pet.export", nil, true, true, petExport)
	petMethod("pet.rename", nil, true, true, petRename)
	petMethod("pet.thumb", func(params map[string]any) map[string]any {
		return map[string]any{"ok": false, "slug": strParam(params, "slug", "")}
	}, true, true, petThumb)
	petMethod("pet.disable", nil, false, true, petDisable)
	petMethod("pet.scale", nil, false, true, petScale)
	petMethodsRegistry.method("pet.cancel", petCancel)
	petMethod("pet.generate.status", func(map[string]any) map[string]any {
		return map[string]any{"available": false, "providers": []any{}}
	}, false, false, petGenerateStatus)
	petMethod("pet.generate", nil, false, false, petGenerate)
	petMethod("pet.hatch", nil, false, false, petHatch)
	petMethodsRegistry.install()
}

// Pet handler body, its slug the third argument when the method takes one
type petBody func(ctx context.Context, rid any, params map[string]any, slug string) (map[string]any, error)

// Registers a pet method whose failures never break the surface, logged then answered with failOpen or 5031
func petMethod(name string, failOpen func(map[string]any) map[string]any, slug, scoped bool, body petBody) {
	handler := func(ctx context.Context, rid any, params map[string]any) (out map[string]any) {
		fail := func(failure any) map[string]any {
			logger().Debug(fmt.Sprintf("%s failed: %s", name, errorMessage(failure)))
			if failOpen != nil {
				return ok(rid, failOpen(params))
			}
			return rpcErr(rid, 5031, fmt.Sprintf("%s failed: %s", name, errorMessage(failure)), nil)
		}
		value := ""
		if slug {
			if value = strParam(params, "slug", ""); value == "" {
				return rpcErr(rid, 4004, "missing slug", nil)
			}
		}
		var err error
		if failure := catchPanic(func() { out, err = body(ctx, rid, params, value) }); failure != nil {
			return fail(failure)
		}
		if err != nil {
			return fail(err)
		}
		return out
	}
	if scoped {
		petMethodsRegistry.scopedMethod(name, handler)
		return
	}
	petMethodsRegistry.method(name, handler)
}

// Tool env of the pet store's network fetches under the bound home
func petEnv(ctx context.Context) *tools.Env {
	if env := tools.ToolEnvFrom(ctx); env != nil {
		return env
	}
	return &tools.Env{Home: home.Dir(ctx), Values: &sync.Map{}}
}

// Progress emit whose failure is logged and never aborts a generation
func petEmit(ctx context.Context, event string, payload map[string]any, what string) {
	if failure := catchPanic(func() { tuigateway.Emit(ctx, event, "", payload) }); failure != nil {
		logger().Debug(fmt.Sprintf("%s emit failed: %s", what, errorMessage(failure)))
	}
}

// Active pet for sprite renderers, a known revision answered with metadata only
func petInfo(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	active, scale, err := activePet(ctx)
	if err != nil || active == nil {
		return ok(rid, map[string]any{"enabled": false}), err
	}
	sprite, err := petSpritePayload(active, scale)
	if err != nil {
		return nil, err
	}
	payload := map[string]any{"enabled": true}
	for key, value := range sprite {
		payload[key] = value
	}
	known := ""
	if raw := params["knownRevision"]; truthy(raw) {
		known = pyrepr.Str(raw)
	}
	if known != "" && known == payload["spritesheetRevision"] {
		delete(payload, "spritesheetBase64")
		payload["spritesheetUnchanged"] = true
	}
	return ok(rid, payload), nil
}

// Cheap active pet metadata used to avoid full payload refreshes
func petInfoMeta(ctx context.Context, rid any, _ map[string]any, _ string) (map[string]any, error) {
	active, scale, err := activePet(ctx)
	if err != nil || active == nil {
		return ok(rid, map[string]any{"enabled": false}), err
	}
	return ok(rid, map[string]any{"enabled": true, "slug": active.Slug, "displayName": active.DisplayName, "scale": scale,
		"spritesheetRevision": petSheetRevision(active.Spritesheet)}), nil
}

// Kitty placement payload of a state for a terminal that speaks it, nil otherwise
func petKittyCells(p *pet.InstalledPet, petConf map[string]any, state string, scale float64) map[string]any {
	configured := "auto"
	if raw := petConf["render_mode"]; truthy(raw) {
		configured = strings.ToLower(pyrepr.Str(raw))
	} else if _, present := petConf["render_mode"]; present {
		configured = ""
	}
	mode := configured
	if configured == "" || configured == "auto" {
		mode = pet.DetectTerminalGraphics()
	}
	if mode != "kitty" {
		return nil
	}
	imageID := pet.KittyImageID(p.Slug)
	payload := pet.NewRenderer(p.Spritesheet, "kitty", scale, 0).KittyPayload(state, imageID)
	if payload == nil {
		return nil
	}
	frames := make([]any, len(payload.Frames))
	for i, frame := range payload.Frames {
		frames[i] = frame
	}
	placeholder := make([]any, len(payload.Placeholder))
	for i, row := range payload.Placeholder {
		placeholder[i] = row
	}
	return map[string]any{"graphics": "kitty", "imageId": imageID, "color": pet.KittyColorHex(imageID), "cols": payload.Cols,
		"rows": payload.Rows, "placeholder": placeholder, "frames": frames, "frameMs": float64(pet.LoopMS) / float64(max(1, len(payload.Frames))),
		"scale": scale}
}

// Half block cell frames of one pet state, a kitty payload instead when graphics are asked for and spoken
func petCells(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	petConf := petCfg(ctx)
	var active *pet.InstalledPet
	if provider.IsTruthyValue(petConf["enabled"]) {
		slug := ""
		if raw := petConf["slug"]; truthy(raw) {
			slug = pyrepr.Str(raw)
		}
		resolved, err := pet.ResolveActivePet(home.Dir(ctx), slug)
		if err != nil {
			return nil, err
		}
		active = resolved
	}
	if active == nil || !active.Exists() {
		return ok(rid, map[string]any{"enabled": false}), nil
	}
	state := string(pet.StateIdle)
	if raw := params["state"]; truthy(raw) {
		state = pyrepr.Str(raw)
	}
	scale := pet.DefaultScale
	if raw := petConf["scale"]; truthy(raw) {
		parsed, err := pyFloat(raw)
		if err != nil {
			return nil, err
		}
		scale = parsed
	}
	cols := 0
	if raw := params["cols"]; truthy(raw) {
		cols = intParamOr(raw, 0)
	}
	if cols == 0 {
		unicodeCols := 0
		if raw := petConf["unicode_cols"]; truthy(raw) {
			unicodeCols = intParamOr(raw, 0)
		}
		cols = pet.ResolveCols(scale, unicodeCols)
	}
	base := map[string]any{"enabled": true, "slug": active.Slug, "displayName": active.DisplayName, "state": state}
	if truthy(params["graphics"]) {
		if kitty := petKittyCells(active, petConf, state, scale); kitty != nil {
			for key, value := range kitty {
				base[key] = value
			}
			return ok(rid, base), nil
		}
	}
	renderer := pet.NewRenderer(active.Spritesheet, "unicode", scale, cols)
	count := renderer.FrameCount(state)
	if count == 0 {
		count = 1
	}
	frames := make([]any, count)
	for i := range count {
		grid := renderer.Cells(state, i, cols)
		rows := make([]any, len(grid))
		for r, row := range grid {
			cells := make([]any, len(row))
			for c, cell := range row {
				cells[c] = []any{int(cell[0][0]), int(cell[0][1]), int(cell[0][2]), int(cell[0][3]),
					int(cell[1][0]), int(cell[1][1]), int(cell[1][2]), int(cell[1][3])}
			}
			rows[r] = cells
		}
		frames[i] = rows
	}
	for key, value := range map[string]any{"cols": cols, "frameMs": float64(pet.LoopMS) / float64(max(1, count)), "frames": frames, "scale": scale} {
		base[key] = value
	}
	return ok(rid, base), nil
}

// Petdex gallery with the local install state, the remote manifest skipped for localOnly
func petGallery(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	localOnly := truthy(params["localOnly"])
	petConf := petCfg(ctx)
	h := home.Dir(ctx)
	pets, err := pet.InstalledPets(h)
	if err != nil {
		return nil, err
	}
	installed := map[string]*pet.InstalledPet{}
	var order []string
	for _, p := range pets {
		if _, seen := installed[p.Slug]; !seen {
			order = append(order, p.Slug)
		}
		installed[p.Slug] = p
	}
	gallery := []any{}
	seen := map[string]bool{}
	if failure := catchPanic(func() {
		env := petEnv(ctx)
		if localOnly {
			pet.Prefetch(ctx, env)
			return
		}
		entries, fetchErr := pet.FetchManifest(ctx, env, false)
		panicIf(fetchErr)
		for _, entry := range entries {
			local, isInstalled := installed[entry.Slug]
			gallery = append(gallery, map[string]any{"slug": entry.Slug, "displayName": entry.DisplayName, "installed": isInstalled,
				"spritesheetUrl": entry.SpritesheetURL, "curated": strings.Contains(entry.SpritesheetURL, "/curated/"),
				"generated": isInstalled && local.Generated()})
			seen[entry.Slug] = true
		}
	}); failure != nil {
		logger().Debug("pet.gallery manifest fetch failed: " + errorMessage(failure))
	}
	for _, slug := range order {
		if seen[slug] {
			continue
		}
		local := installed[slug]
		gallery = append(gallery, map[string]any{"slug": slug, "displayName": local.DisplayName, "installed": true, "spritesheetUrl": "",
			"generated": local.Generated()})
	}
	active := ""
	if raw := petConf["slug"]; truthy(raw) {
		active = pyrepr.Str(raw)
	}
	return ok(rid, map[string]any{"enabled": provider.IsTruthyValue(petConf["enabled"]), "active": active, "pets": gallery}), nil
}

// Adopts a pet, installing it when needed and activating it in config
func petSelect(ctx context.Context, rid any, _ map[string]any, slug string) (map[string]any, error) {
	installed, err := pet.InstallPet(ctx, petEnv(ctx), home.Dir(ctx), slug, false)
	if err != nil {
		var storeErr *pet.StoreError
		var manifestErr *pet.ManifestError
		if asStore, isStore := err.(*pet.StoreError); isStore {
			storeErr = asStore
		}
		if asManifest, isManifest := err.(*pet.ManifestError); isManifest {
			manifestErr = asManifest
		}
		if storeErr != nil || manifestErr != nil {
			return rpcErr(rid, 5031, fmt.Sprintf("could not adopt '%s': %s", slug, err.Error()), nil), nil
		}
		return nil, err
	}
	if err := setActivePet(ctx, slug); err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"ok": true, "slug": slug, "displayName": installed.DisplayName}), nil
}

// Active slug follow up after a store operation that already succeeded, a failure only logged
func petConfigFollowup(what string, update func() (bool, error)) {
	var err error
	if failure := catchPanic(func() { _, err = update() }); failure != nil {
		logger().Debug(fmt.Sprintf("%s config update failed: %s", what, errorMessage(failure)))
		return
	}
	if err != nil {
		logger().Debug(fmt.Sprintf("%s config update failed: %s", what, err.Error()))
	}
}

// Uninstalls a pet, turning the display off when it was the active one
func petRemove(ctx context.Context, rid any, _ map[string]any, slug string) (map[string]any, error) {
	removed, err := pet.RemovePet(home.Dir(ctx), slug)
	if err != nil {
		return nil, err
	}
	petConfigFollowup("pet.remove", func() (bool, error) { return clearActivePetIf(ctx, slug) })
	return ok(rid, map[string]any{"ok": removed, "slug": slug}), nil
}

// Exports an installed pet as a reimportable zip in base64
func petExport(ctx context.Context, rid any, _ map[string]any, slug string) (map[string]any, error) {
	filename, data, err := pet.ExportPet(home.Dir(ctx), slug)
	if err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"ok": true, "filename": filename, "zipBase64": base64.StdEncoding.EncodeToString(data)}), nil
}

// Renames a pet's display name and slug, the active slug in config following it
func petRename(ctx context.Context, rid any, params map[string]any, slug string) (map[string]any, error) {
	name := strParam(params, "name", "")
	if name == "" {
		return rpcErr(rid, 4004, "missing name", nil), nil
	}
	newSlug, err := pet.RenamePet(home.Dir(ctx), slug, name)
	if err != nil {
		return nil, err
	}
	if newSlug == "" {
		return rpcErr(rid, 5031, "pet.rename failed", nil), nil
	}
	if newSlug != slug {
		petConfigFollowup("pet.rename", func() (bool, error) { return renameActivePetIf(ctx, slug, newSlug) })
	}
	return ok(rid, map[string]any{"ok": true, "slug": newSlug, "displayName": name}), nil
}

// Idle frame PNG data URI of a pet for the picker, a petdex url thumbnailing one not installed yet
func petThumb(ctx context.Context, rid any, params map[string]any, slug string) (map[string]any, error) {
	sourceURL := ""
	if raw := params["url"]; truthy(raw) {
		sourceURL = pyrepr.Str(raw)
	}
	data := pet.ThumbnailPNG(ctx, petEnv(ctx), home.Dir(ctx), slug, sourceURL)
	if len(data) == 0 {
		return ok(rid, map[string]any{"ok": false, "slug": slug}), nil
	}
	return ok(rid, map[string]any{"ok": true, "slug": slug, "dataUri": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}), nil
}

// Turns the pet display off from the desktop picker
func petDisable(ctx context.Context, rid any, _ map[string]any, _ string) (map[string]any, error) {
	if err := setPetEnabled(ctx, false); err != nil {
		return nil, err
	}
	return ok(rid, map[string]any{"ok": true}), nil
}

// Persists display.pet.scale clamped to the engine bounds from the desktop slider
func petScale(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	scale, failure, err := setPetScale(ctx, params["scale"])
	if err != nil {
		return nil, err
	}
	if failure != "" {
		return rpcErr(rid, 4004, failure, nil), nil
	}
	return ok(rid, map[string]any{"ok": true, "scale": scale}), nil
}

// Stops an in flight generate or hatch by its token, idempotent
func petCancel(_ context.Context, rid any, params map[string]any) map[string]any {
	if token := strParam(params, "token", ""); token != "" {
		petCancelRequest(token)
	}
	return ok(rid, map[string]any{"ok": true})
}

// Whether pet generation is possible, a reference capable image backend configured, and the providers listed
func petGenerateStatus(ctx context.Context, rid any, _ map[string]any, _ string) (map[string]any, error) {
	env := petEnv(ctx)
	available := true
	if _, err := generate.ResolveProvider(ctx, env, true, ""); err != nil {
		if _, isGenErr := err.(*generate.GenerationError); !isGenErr {
			return nil, err
		}
		available = false
	}
	providers := []map[string]any{}
	if failure := catchPanic(func() { providers = generate.ListSpriteProviders(ctx, env) }); failure != nil {
		logger().Debug("pet provider list failed: " + errorMessage(failure))
		providers = []map[string]any{}
	}
	listed := make([]any, len(providers))
	for i, entry := range providers {
		listed[i] = entry
	}
	return ok(rid, map[string]any{"available": available, "providers": listed}), nil
}

// Picker chosen provider resolved up front, nil when none was named
func petPickProvider(ctx context.Context, params map[string]any, requireReferences bool) (*generate.SpriteProvider, error) {
	name := strParam(params, "provider", "")
	if name == "" {
		return nil, nil
	}
	return generate.ResolveProvider(ctx, petEnv(ctx), requireReferences, name)
}

// Releases the run's cancel arm and answers the error
func petGenAbort(rid any, token string, code int, message string) map[string]any {
	petCancelRelease(token)
	return rpcErr(rid, code, message, nil)
}

// Copies a file
func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// Candidate base looks for a new pet, each draft staged and streamed the moment it lands
func petGenerate(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	prompt := strParam(params, "prompt", "")
	refRaw := strParam(params, "referenceImage", "")
	if prompt == "" && refRaw == "" {
		return rpcErr(rid, 4004, "missing prompt", nil), nil
	}
	count := intParam(params, "count", 4)
	if count == 0 {
		count = 4
	}
	count = max(1, min(4, count))
	root, err := petGenRoot(ctx)
	if err != nil {
		return nil, err
	}
	petGenSweep(root, 3600e9)
	token := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	petCancelArm(token)
	stage := filepath.Join(root, token)
	if err := os.MkdirAll(stage, 0o777); err != nil {
		return nil, err
	}
	var references []string
	if refRaw != "" {
		refs, err := petReferenceImagesFromDataURL(refRaw, stage)
		if err != nil {
			if refErr, isRef := err.(*petReferenceError); isRef {
				return petGenAbort(rid, token, 4004, refErr.msg), nil
			}
			return nil, err
		}
		references = refs
	}
	sprite, err := petPickProvider(ctx, params, len(references) > 0)
	if err != nil {
		if genErr, isGen := err.(*generate.GenerationError); isGen {
			return petGenAbort(rid, token, 5031, genErr.Error()), nil
		}
		return nil, err
	}
	var mu sync.Mutex
	out := []map[string]any{}
	petEmit(ctx, "pet.generate.progress", map[string]any{"token": token, "count": count}, "pet.generate init")
	onDraft := func(index int, src string) {
		dest := filepath.Join(stage, fmt.Sprintf("draft-%d.png", index))
		dataURI := ""
		if failure := catchPanic(func() {
			panicIf(copyFile(src, dest))
			uri, uriErr := petPNGDataURI(dest, 160)
			panicIf(uriErr)
			dataURI = uri
		}); failure != nil {
			logger().Debug(fmt.Sprintf("pet.generate draft %d failed: %s", index, errorMessage(failure)))
			return
		}
		mu.Lock()
		out = append(out, map[string]any{"index": index, "dataUri": dataURI})
		mu.Unlock()
		petEmit(ctx, "pet.generate.progress", map[string]any{"token": token, "index": index, "dataUri": dataURI, "count": count},
			"pet.generate progress")
	}
	concept := prompt
	if concept == "" {
		concept = "a pet based on the reference image"
	}
	_, err = generate.GenerateBaseDrafts(ctx, petEnv(ctx), concept, generate.DraftOptions{N: count, Style: strParam(params, "style", "auto"),
		References: references, Provider: sprite, OnDraft: onDraft, IsCancelled: func() bool { return petIsCancelled(token) }})
	if err != nil {
		if genErr, isGen := err.(*generate.GenerationError); isGen {
			return petGenAbort(rid, token, 5031, genErr.Error()), nil
		}
		return nil, err
	}
	cancelled := petIsCancelled(token)
	petCancelRelease(token)
	mu.Lock()
	drafts := append([]map[string]any(nil), out...)
	mu.Unlock()
	if cancelled || len(drafts) == 0 {
		message := "generation produced no usable drafts"
		if cancelled {
			message = "generation cancelled"
		}
		return rpcErr(rid, 5031, message, nil), nil
	}
	sort.SliceStable(drafts, func(i, j int) bool { return drafts[i]["index"].(int) < drafts[j]["index"].(int) })
	listed := make([]any, len(drafts))
	for i, draft := range drafts {
		listed[i] = draft
	}
	return ok(rid, map[string]any{"ok": true, "token": token, "drafts": listed}), nil
}

// Turns a base draft into a full installed pet that is not yet active, its row progress streamed
func petHatch(ctx context.Context, rid any, params map[string]any, _ string) (map[string]any, error) {
	token, name := strParam(params, "token", ""), strParam(params, "name", "")
	if token == "" {
		return rpcErr(rid, 4004, "missing token", nil), nil
	}
	if name == "" {
		return rpcErr(rid, 4004, "missing name", nil), nil
	}
	cancelToken := strParam(params, "cancelToken", "")
	if cancelToken == "" {
		cancelToken = token
	}
	root, err := petGenRoot(ctx)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(root, token, fmt.Sprintf("draft-%d.png", intParam(params, "index", 0)))
	if info, statErr := os.Stat(base); statErr != nil || !info.Mode().IsRegular() {
		return rpcErr(rid, 4004, "draft expired — generate again", nil), nil
	}
	sprite, err := petPickProvider(ctx, params, true)
	if err != nil {
		if genErr, isGen := err.(*generate.GenerationError); isGen {
			return rpcErr(rid, 5031, genErr.Error(), nil), nil
		}
		return nil, err
	}
	petCancelArm(cancelToken)
	defer petCancelRelease(cancelToken)
	h := home.Dir(ctx)
	slug, err := pet.UniqueSlug(h, name)
	if err != nil {
		return nil, err
	}
	onProgress := func(event, detail string) {
		payload := map[string]any{"event": event, "detail": detail}
		if event == "row" && strings.Count(detail, ":") == 2 {
			parts := strings.SplitN(detail, ":", 3)
			payload = map[string]any{"event": "row", "state": parts[0], "done": parts[1], "total": parts[2]}
		}
		petEmit(ctx, "pet.hatch.progress", payload, "pet.hatch progress")
	}
	description := ""
	if raw := params["description"]; truthy(raw) {
		description = pyrepr.Str(raw)
	}
	concept := name
	if raw := params["prompt"]; truthy(raw) {
		concept = pyrepr.Str(raw)
	}
	result, err := generate.HatchPet(ctx, petEnv(ctx), generate.HatchOptions{BaseImage: base, Slug: slug, DisplayName: name,
		Description: description, Concept: concept, Style: strParam(params, "style", "auto"), Provider: sprite, OnProgress: onProgress,
		IsCancelled: func() bool { return petIsCancelled(cancelToken) }})
	if err != nil {
		if genErr, isGen := err.(*generate.GenerationError); isGen {
			return rpcErr(rid, 5031, genErr.Error(), nil), nil
		}
		return nil, err
	}
	loaded, err := pet.LoadPet(h, result.Slug)
	if err != nil {
		return nil, err
	}
	spritePayload := map[string]any{}
	if loaded != nil {
		if spritePayload, err = petSpritePayload(loaded, petConfigScale(ctx)); err != nil {
			return nil, err
		}
	}
	warnings := make([]any, len(result.Validation.Warnings))
	for i, warning := range result.Validation.Warnings {
		warnings[i] = warning
	}
	return ok(rid, map[string]any{"ok": true, "slug": result.Slug, "displayName": result.DisplayName, "warnings": warnings,
		"pet": spritePayload}), nil
}
