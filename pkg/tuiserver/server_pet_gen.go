package tuiserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pypil"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/pet"
)

// Configured display.pet.scale or the engine default, never failing
func petConfigScale(ctx context.Context) float64 {
	raw := petCfg(ctx)["scale"]
	if !pyrepr.Truthy(raw) {
		return pet.DefaultScale
	}
	scale, err := pyFloat(raw)
	if err != nil {
		return pet.DefaultScale
	}
	return scale
}

// Renderer payloads keyed on the sheet stat, slug, name and scale, the eight newest kept
var petPayloadCache = struct {
	sync.Mutex
	order   []string
	entries map[string]map[string]any
}{entries: map[string]map[string]any{}}

// Spritesheet bytes and frame geometry of a pet, the one shape of pet.info and pet.hatch
func petSpritePayload(p *pet.InstalledPet, scale float64) (map[string]any, error) {
	cacheKey := ""
	if info, err := os.Stat(p.Spritesheet); err == nil {
		cacheKey = fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s\x00%.4f", p.Spritesheet, info.ModTime().UnixNano(), info.Size(), p.Slug, p.DisplayName,
			math.RoundToEven(scale*10000)/10000)
	}
	if cacheKey != "" {
		petPayloadCache.Lock()
		cached := petPayloadCache.entries[cacheKey]
		petPayloadCache.Unlock()
		if cached != nil {
			return clonePetPayload(cached), nil
		}
	}
	framesByState := pet.StateFrameCounts(p.Spritesheet)
	if framesByState == nil {
		framesByState = map[string]int{}
	}
	raw, err := os.ReadFile(p.Spritesheet)
	if err != nil {
		return nil, err
	}
	mime := "image/webp"
	if strings.ToLower(filepath.Ext(p.Spritesheet)) == ".png" {
		mime = "image/png"
	}
	payload := map[string]any{"slug": p.Slug, "displayName": p.DisplayName, "mime": mime,
		"spritesheetBase64": base64.StdEncoding.EncodeToString(raw), "spritesheetRevision": petSheetRevision(p.Spritesheet),
		"frameW": pet.FrameW, "frameH": pet.FrameH, "framesPerState": pet.FramesPerState, "framesByState": framesByState,
		"framesByRow": petRowFrameCounts(p.Spritesheet), "loopMs": pet.LoopMS, "scale": scale, "stateRows": petStateRows(p.Spritesheet)}
	if cacheKey != "" {
		petPayloadCache.Lock()
		if _, present := petPayloadCache.entries[cacheKey]; !present {
			petPayloadCache.order = append(petPayloadCache.order, cacheKey)
		}
		petPayloadCache.entries[cacheKey] = payload
		for len(petPayloadCache.order) > 8 {
			delete(petPayloadCache.entries, petPayloadCache.order[0])
			petPayloadCache.order = petPayloadCache.order[1:]
		}
		petPayloadCache.Unlock()
	}
	return clonePetPayload(payload), nil
}

// Profile scoped staging directory of in progress generation drafts
func petGenRoot(ctx context.Context) (string, error) {
	root := filepath.Join(home.Dir(ctx), "cache", "pet-gen")
	return root, os.MkdirAll(root, 0o777)
}

// Drops draft staging directories older than an hour so the cache never grows unbounded
func petGenSweep(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		logger().Debug("pet-gen sweep failed: " + err.Error())
		return
	}
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr != nil || !entry.IsDir() || time.Since(info.ModTime()) <= maxAge {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, entry.Name()))
	}
}

// Downscaled PNG data URI of a draft image, its longest side at most maxPx
func petPNGDataURI(path string, maxPx int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	decoded, _, err := image.Decode(f)
	if err != nil {
		return "", err
	}
	img := pypil.ToNRGBA(decoded)
	if w, h, resize := pypil.ThumbnailSize(img.Rect.Dx(), img.Rect.Dy(), maxPx); resize {
		img = pypil.ResizeLanczos(img, w, h)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", err
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// Tokens of generate and hatch runs whose Stop was requested, polled between provider calls
var petCancelled = struct {
	sync.Mutex
	tokens map[string]bool
}{tokens: map[string]bool{}}

// Reference image extension of each accepted data URL image type
var petReferenceMimeExt = map[string]string{"png": "png", "jpeg": "jpg", "jpg": "jpg", "webp": "webp", "gif": "gif"}

// Largest accepted reference image, ICEBATH_PET_REFERENCE_MAX_BYTES when it is a positive integer
var petReferenceMaxBytes = func() int {
	raw := os.Getenv("ICEBATH_PET_REFERENCE_MAX_BYTES")
	if raw == "" {
		raw = strconv.Itoa(16 * 1024 * 1024)
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 16 * 1024 * 1024
	}
	return max(1, n)
}()

var petReferenceRe = regexp.MustCompile(`(?s)^data:image/([a-zA-Z0-9.+-]+);base64,(.*)$`)

// Value error of a rejected reference image
type petReferenceError struct{ msg string }

func (e *petReferenceError) Error() string      { return e.msg }
func (e *petReferenceError) PyTypeName() string { return "ValueError" }

// Decodes and validates a reference image data URL into the stage directory
func petReferenceImagesFromDataURL(refRaw, stage string) ([]string, error) {
	match := petReferenceRe.FindStringSubmatch(refRaw)
	if match == nil {
		return nil, &petReferenceError{"invalid reference image format"}
	}
	ext, known := petReferenceMimeExt[strings.ToLower(match[1])]
	if !known {
		return nil, &petReferenceError{"unsupported reference image type"}
	}
	payload := strings.Join(strings.Fields(match[2]), "")
	if len(payload)*3/4 > petReferenceMaxBytes {
		return nil, &petReferenceError{"reference image too large"}
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(payload)
	if err != nil {
		return nil, &petReferenceError{"invalid reference image data"}
	}
	if len(raw) > petReferenceMaxBytes {
		return nil, &petReferenceError{"reference image too large"}
	}
	refPath := filepath.Join(stage, "reference."+ext)
	if err := os.WriteFile(refPath, raw, 0o666); err != nil {
		return nil, err
	}
	return []string{refPath}, nil
}

// Clears a stale cancel flag at the start of a generate or hatch run
func petCancelArm(token string) {
	petCancelled.Lock()
	defer petCancelled.Unlock()
	delete(petCancelled.tokens, token)
}

// Releases a run's cancel flag once it ended, the same clear as the arm
var petCancelRelease = petCancelArm

// Flags a run's token so its next poll stops the generation
func petCancelRequest(token string) {
	petCancelled.Lock()
	defer petCancelled.Unlock()
	petCancelled.tokens[token] = true
}

// Whether Stop was requested for a run's token
func petIsCancelled(token string) bool {
	petCancelled.Lock()
	defer petCancelled.Unlock()
	return petCancelled.tokens[token]
}

// The display.pet block of the canonical config, created when missing, a failure when display or pet is no mapping
func petConfigBlock(cfg map[string]any) (map[string]any, any, error) {
	display, present := cfg["display"]
	if !present {
		display = map[string]any{}
		cfg["display"] = display
	}
	displayMap, isDict := display.(map[string]any)
	if !isDict {
		return nil, nil, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(display) + "' object has no attribute 'setdefault'"}
	}
	petVal, present := displayMap["pet"]
	if !present {
		petVal = map[string]any{}
		displayMap["pet"] = petVal
	}
	petMap, _ := petVal.(map[string]any)
	return petMap, petVal, nil
}

// Writes display.pet keys and saves the config, only when the configured slug equals whenSlug if given
func updatePetConfig(ctx context.Context, whenSlug *string, values map[string]any) (bool, error) {
	tree, err := loadConfig(ctx)
	if err != nil {
		return false, err
	}
	cfg := map[string]any(tree)
	petMap, petVal, err := petConfigBlock(cfg)
	if err != nil {
		return false, err
	}
	if whenSlug != nil {
		current := ""
		if petMap != nil && pyrepr.Truthy(petMap["slug"]) {
			current = pyrepr.Str(petMap["slug"])
		}
		if petMap == nil || current != *whenSlug {
			return false, nil
		}
	}
	if petMap == nil {
		return false, &pyrepr.FormatError{Class: "AttributeError", Msg: "'" + pyrepr.TypeName(petVal) + "' object has no attribute 'update'"}
	}
	for key, value := range values {
		petMap[key] = value
	}
	if err := config.SaveConfig(ctx, home.Dir(ctx), cfg, config.SaveOptions{}); err != nil {
		return false, err
	}
	return true, nil
}

// Activates a pet and turns the display on
func setActivePet(ctx context.Context, slug string) error {
	_, err := updatePetConfig(ctx, nil, map[string]any{"slug": slug, "enabled": true})
	return err
}

// Turns the pet display on or off
func setPetEnabled(ctx context.Context, enabled bool) error {
	_, err := updatePetConfig(ctx, nil, map[string]any{"enabled": enabled})
	return err
}

// Sets display.pet.scale clamped to the engine bounds, an error text and no write when the value is no number
func setPetScale(ctx context.Context, value any) (float64, string, error) {
	scale, err := pyFloat(value)
	if err != nil {
		return 0, "not a number: " + pyrepr.Repr(value) + " — try a value like 0.5", nil
	}
	scale = pet.ClampScale(scale)
	if _, err := updatePetConfig(ctx, nil, map[string]any{"scale": scale}); err != nil {
		return 0, "", err
	}
	return scale, "", nil
}

// Disables and unsets the active pet when it is the given slug
func clearActivePetIf(ctx context.Context, slug string) (bool, error) {
	return updatePetConfig(ctx, &slug, map[string]any{"slug": "", "enabled": false})
}

// Repoints the active pet from the old slug to the new one when it is active, enabled preserved
func renameActivePetIf(ctx context.Context, oldSlug, newSlug string) (bool, error) {
	if newSlug == "" || oldSlug == newSlug {
		return false, nil
	}
	return updatePetConfig(ctx, &oldSlug, map[string]any{"slug": newSlug})
}
