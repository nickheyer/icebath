package generate

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pet"
	"github.com/nickheyer/icebath/pkg/tools"
)

const (
	maxParallelGenerations = 4
	rowGenAttempts         = 3
	minFilledStates        = 6
)

var requiredStates = []string{"idle", "running-right", "waving"}

// Substrings of a raw provider error and the friendly message the first match gives
var imageErrorHints = []struct {
	needles []string
	message string
}{
	{[]string{"moderation_blocked", "safety system", "content policy", "content_policy"},
		"The image provider blocked this prompt — its safety filter rejects " +
			"trademarked characters and real people. Try an original description."},
	{[]string{"api key", "unauthorized", "401", "auth"},
		"The image provider rejected the request — check your API key in Settings → Providers."},
	{[]string{"rate limit", "429"}, "The image provider is rate-limiting — wait a moment and try again."},
}

// ProgressFn receives hatch progress events such as row, compose and save
type ProgressFn func(event, detail string)

// HatchResult is the outcome of a successful hatch
type HatchResult struct {
	Slug, DisplayName, Spritesheet string
	States                         []string
	Validation                     AtlasValidation
}

// Removes a file, ignoring any error
func unlinkQuietly(path string) { _ = os.Remove(path) }

// Keys out any solid backdrop of a draft and saves it as an RGBA PNG, the original kept on failure
func hardenTransparency(path string) string {
	out, err := func() (string, error) {
		img, err := loadRGBA(path)
		if err != nil {
			return "", err
		}
		keyed := ClearTransparentRGB(RemoveBackground(img, nil, 90.0))
		out := path
		if ext := filepath.Ext(path); strings.ToLower(ext) != ".png" {
			out = strings.TrimSuffix(path, ext) + ".png"
		}
		f, err := os.Create(out)
		if err != nil {
			return "", err
		}
		if err := png.Encode(f, keyed); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		if out != path {
			unlinkQuietly(path)
		}
		return out, nil
	}()
	if err != nil {
		genLog.Debug(fmt.Sprintf("base draft transparency hardening failed for %s: %s", path, pyrepr.ExceptionMessage(err)))
		return path
	}
	return out
}

// Runs fn over the items on a bounded pool, handing results back in completion order until cancelled
func runParallel[I, R any](items []I, fn func(I) R, cancelled func() bool, onCancelLog string, each func(R)) {
	workers := max(1, min(len(items), maxParallelGenerations))
	jobs := make(chan I)
	results := make(chan R, len(items))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				results <- fn(item)
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, item := range items {
			select {
			case jobs <- item:
			case <-stop:
				return
			}
		}
	}()
	for received := 0; received < len(items); received++ {
		r := <-results
		if cancelled() {
			genLog.Info(onCancelLog)
			close(stop)
			go func() {
				wg.Wait()
				close(results)
			}()
			return
		}
		each(r)
	}
	wg.Wait()
}

// Draft generation options, zero N taking four and empty Style auto
type DraftOptions struct {
	N           int
	Style       string
	References  []string
	Provider    *SpriteProvider
	OnDraft     func(index int, path string)
	IsCancelled func() bool
}

// Generates candidate base looks concurrently and returns their hardened image paths
func GenerateBaseDrafts(ctx context.Context, env *tools.Env, concept string, o DraftOptions) ([]string, error) {
	if o.N == 0 {
		o.N = 4
	}
	if o.Style == "" {
		o.Style = "auto"
	}
	refs := o.References
	sprite := o.Provider
	if sprite == nil {
		var err error
		if sprite, err = ResolveProvider(ctx, env, len(refs) > 0, ""); err != nil {
			return nil, err
		}
	}
	cancelled := o.IsCancelled
	if cancelled == nil {
		cancelled = func() bool { return false }
	}
	genLog.Info(fmt.Sprintf("pet generate: drafting %d base looks for %s (style=%s)", o.N, pyrepr.Repr(concept), o.Style))
	type draft struct {
		index int
		path  string
		err   string
	}
	one := func(index int) draft {
		if cancelled() {
			return draft{index: index}
		}
		t0 := time.Now()
		variation := BaseVariations[index%len(BaseVariations)]
		out, err := Generate(ctx, env, BuildBasePrompt(concept, o.Style, variation), 1, refs, sprite, "pet_base", "square")
		if err != nil {
			genLog.Warn(fmt.Sprintf("pet generate: draft %d failed after %.1fs: %s", index, time.Since(t0).Seconds(), pyrepr.ExceptionMessage(err)))
			return draft{index: index, err: pyrepr.ExceptionMessage(err)}
		}
		if len(out) == 0 {
			genLog.Warn(fmt.Sprintf("pet generate: draft %d produced no image", index))
			return draft{index: index, err: "the image provider returned no image"}
		}
		genLog.Info(fmt.Sprintf("pet generate: draft %d ready in %.1fs", index, time.Since(t0).Seconds()))
		return draft{index: index, path: hardenTransparency(out[0])}
	}
	indices := make([]int, o.N)
	for i := range indices {
		indices[i] = i
	}
	results := map[int]string{}
	var errs []string
	runParallel(indices, one, cancelled, "pet generate: cancelled — dropping remaining drafts", func(d draft) {
		if d.path == "" {
			if d.err != "" {
				errs = append(errs, d.err)
			}
			return
		}
		results[d.index] = d.path
		if o.OnDraft != nil {
			func() {
				defer func() {
					if r := recover(); r != nil {
						genLog.Debug(fmt.Sprintf("on_draft callback failed: %v", r))
					}
				}()
				o.OnDraft(d.index, d.path)
			}()
		}
	})
	keys := make([]int, 0, len(results))
	for k := range results {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	drafts := make([]string, len(keys))
	for i, k := range keys {
		drafts[i] = results[k]
	}
	if len(drafts) == 0 && !cancelled() {
		if len(errs) == 0 {
			return nil, &GenerationError{Msg: "image generation produced no usable drafts"}
		}
		return nil, &GenerationError{Msg: humanizeImageError(mostCommon(errs))}
	}
	return drafts, nil
}

// Most frequent string, the first seen winning ties
func mostCommon(values []string) string {
	counts := map[string]int{}
	best, bestN := "", 0
	for _, v := range values {
		counts[v]++
	}
	for _, v := range values {
		if counts[v] > bestN {
			best, bestN = v, counts[v]
		}
	}
	return best
}

// Friendly actionable sentence for a raw provider error, else its first line
func humanizeImageError(errText string) string {
	low := pystr.Lower(errText)
	for _, hint := range imageErrorHints {
		for _, needle := range hint.needles {
			if strings.Contains(low, needle) {
				return hint.message
			}
		}
	}
	lines := pystr.Splitlines(errText, false)
	first := ""
	if len(lines) > 0 {
		first = pystr.Strip(lines[0])
	}
	return pystr.Slice(first, 0, 200)
}

// Generates and slices one animation row with retries, nil frames when cancelled or every attempt failed
func generateRow(ctx context.Context, env *tools.Env, spec RowSpec, base, style, slug string, sprite *SpriteProvider, cancelled func() bool) StateFrames {
	t0 := time.Now()
	var lastErr error
	for attempt := 0; attempt < rowGenAttempts; attempt++ {
		if cancelled() {
			return StateFrames{State: spec.State}
		}
		frames, err := func() ([]*image.NRGBA, error) {
			strips, err := Generate(ctx, env, BuildRowPrompt(spec.State, spec.Frames, style), 1, []string{base}, sprite, "pet_row_"+spec.State, "landscape")
			defer func() {
				for _, s := range strips {
					unlinkQuietly(s)
				}
			}()
			if err != nil {
				return nil, err
			}
			method := "auto"
			if attempt < rowGenAttempts-1 {
				method = "components"
			}
			return ExtractStripFrames(strips[0], spec.Frames, method, false)
		}()
		if err == nil {
			genLog.Info(fmt.Sprintf("pet hatch %s: row %s ready in %.1fs (attempt %d)", pyrepr.Repr(slug), pyrepr.Repr(spec.State), time.Since(t0).Seconds(), attempt+1))
			return StateFrames{State: spec.State, Frames: frames}
		}
		lastErr = err
		genLog.Warn(fmt.Sprintf("pet hatch %s: row %s attempt %d/%d failed: %s", pyrepr.Repr(slug), pyrepr.Repr(spec.State), attempt+1, rowGenAttempts, pyrepr.ExceptionMessage(err)))
	}
	genLog.Warn(fmt.Sprintf("pet hatch %s: row %s gave up after %.1fs: %s", pyrepr.Repr(slug), pyrepr.Repr(spec.State), time.Since(t0).Seconds(), pyrepr.ExceptionMessage(lastErr)))
	return StateFrames{State: spec.State}
}

// Hatch inputs, empty Style meaning auto
type HatchOptions struct {
	BaseImage, Slug, DisplayName, Description, Concept, Style string
	OnProgress                                                ProgressFn
	Provider                                                  *SpriteProvider
	IsCancelled                                               func() bool
}

// Turns an approved base image into a full installed pet, idle falling back to the base look
func HatchPet(ctx context.Context, env *tools.Env, o HatchOptions) (*HatchResult, error) {
	if o.Style == "" {
		o.Style = "auto"
	}
	if fi, err := os.Stat(o.BaseImage); err != nil || !fi.Mode().IsRegular() {
		return nil, &GenerationError{Msg: "base image not found: " + o.BaseImage}
	}
	sprite := o.Provider
	if sprite == nil {
		var err error
		if sprite, err = ResolveProvider(ctx, env, true, ""); err != nil {
			return nil, err
		}
	}
	progress := o.OnProgress
	if progress == nil {
		progress = func(string, string) {}
	}
	cancelled := o.IsCancelled
	if cancelled == nil {
		cancelled = func() bool { return false }
	}
	var framesByState []StateFrames
	totalRows := len(RowSpecs)
	genLog.Info(fmt.Sprintf("pet hatch %s: generating %d animation rows", pyrepr.Repr(o.Slug), totalRows))
	var generated []RowSpec
	for _, spec := range RowSpecs {
		if spec.State != "running-left" {
			generated = append(generated, spec)
		}
	}
	done := 0
	runParallel(generated, func(spec RowSpec) StateFrames {
		return generateRow(ctx, env, spec, o.BaseImage, o.Style, o.Slug, sprite, cancelled)
	}, cancelled, fmt.Sprintf("pet hatch %s: cancelled — dropping remaining rows", pyrepr.Repr(o.Slug)), func(sf StateFrames) {
		done++
		progress("row", fmt.Sprintf("%s:%d:%d", sf.State, done, totalRows))
		if len(sf.Frames) > 0 {
			framesByState = append(framesByState, sf)
		}
	})
	if cancelled() {
		return nil, &GenerationError{Msg: "hatch cancelled"}
	}
	lookup := func(state string) []*image.NRGBA {
		for _, sf := range framesByState {
			if sf.State == state {
				return sf.Frames
			}
		}
		return nil
	}
	if right := lookup("running-right"); len(right) > 0 {
		done++
		progress("row", fmt.Sprintf("running-left:%d:%d", done, totalRows))
		framesByState = append(framesByState, StateFrames{State: "running-left", Frames: MirrorFrames(right)})
		genLog.Info(fmt.Sprintf("pet hatch %s: row 'running-left' mirrored from running-right", pyrepr.Repr(o.Slug)))
	} else {
		genLog.Warn(fmt.Sprintf("pet hatch %s: no running-right to mirror; left walk left empty", pyrepr.Repr(o.Slug)))
	}
	if len(lookup("idle")) == 0 {
		progress("row", "idle-fallback")
		frame, err := SingleFrame(o.BaseImage, false)
		if err != nil {
			return nil, err
		}
		framesByState = append(framesByState, StateFrames{State: "idle", Frames: []*image.NRGBA{frame}})
	}
	progress("compose", "")
	genLog.Info(fmt.Sprintf("pet hatch %s: composing atlas from %d states", pyrepr.Repr(o.Slug), len(framesByState)))
	sheet := ComposeAtlas(NormalizeCells(framesByState, normalizePad))
	validation := ValidateAtlas(sheet)
	if !validation.OK {
		msg := strings.Join(validation.Errors, "; ")
		if msg == "" {
			msg = "atlas validation failed"
		}
		return nil, &GenerationError{Msg: msg}
	}
	filled := map[string]bool{}
	for _, s := range validation.FilledStates {
		filled[s] = true
	}
	var missing []string
	for _, s := range requiredStates {
		if !filled[s] {
			missing = append(missing, s)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return nil, &GenerationError{Msg: "missing required animation row(s): " + strings.Join(missing, ", ")}
	}
	if len(filled) < minFilledStates {
		return nil, &GenerationError{Msg: fmt.Sprintf("only %d/%d animation rows were usable; regenerate", len(filled), totalRows)}
	}
	progress("save", o.Slug)
	genLog.Info(fmt.Sprintf("pet hatch %s: saving pet", pyrepr.Repr(o.Slug)))
	displayName := o.DisplayName
	if displayName == "" {
		displayName = o.Slug
	}
	p, err := pet.RegisterLocalPet(home.Dir(ctx), sheet, nil, o.Slug, displayName, o.Description)
	if err != nil {
		return nil, err
	}
	return &HatchResult{Slug: p.Slug, DisplayName: p.DisplayName, Spritesheet: p.Spritesheet, States: validation.FilledStates, Validation: validation}, nil
}
