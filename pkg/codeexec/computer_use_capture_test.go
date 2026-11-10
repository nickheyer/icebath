package codeexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/tools"
)

const cuTestPNG8 = "iVBORw0KGgoAAAANSUhEUgAAAAgAAAAICAYAAADED76LAAAADUlEQVR4nG" +
	"NgGAUgAAABCAABgukLHQAAAABJRU5ErkJggg=="

// Real RGB PNG of the given size whose pixels vary with the seed
func cuSeedPNG(t *testing.T, w, h, seed int) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := uint8((x + y + seed) % 256)
			img.SetNRGBA(x, y, color.NRGBA{v, v, v, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func cuRoutedCapture(mode string) *CaptureResult {
	b64 := cuTestPNG8
	raw, _ := base64.StdEncoding.DecodeString(b64)
	return &CaptureResult{Mode: mode, Width: 1280, Height: 800, PNGB64: &b64, App: "Safari", WindowTitle: "GitHub – Issue #24015",
		PNGBytesLen: len(raw), Elements: []UIElement{
			{Index: 0, Role: "AXButton", Label: "Sign in", Bounds: [4]int{10, 20, 80, 30}},
			{Index: 1, Role: "AXTextField", Label: "username", Bounds: [4]int{10, 60, 200, 24}}}}
}

func cuDedupCapture(t *testing.T, seed int, app string) *CaptureResult {
	b64 := cuSeedPNG(t, 64, 64, seed)
	return &CaptureResult{Mode: "som", Width: 64, Height: 64, PNGB64: &b64, App: app, WindowTitle: "notes"}
}

func cuRoute(t *testing.T, route bool) {
	t.Helper()
	cuSwap(t, &shouldRouteThroughAuxVision, func(context.Context, *tools.Env) bool { return route })
}

func cuCaptureEnv(t *testing.T) *tools.Env {
	t.Helper()
	return &tools.Env{Home: t.TempDir()}
}

func cuRespond(t *testing.T, env *tools.Env, c *CaptureResult, sessionID string) any {
	t.Helper()
	var sid *string
	if sessionID != "" {
		sid = &sessionID
	}
	out, err := captureResponse(context.Background(), env, c, cuDefaultMaxElements, sid)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func cuIsMultimodal(v any) bool {
	o, ok := v.(*pyjson.Object)
	return ok && o.Vals["_multimodal"] == true
}

func cuDecode(t *testing.T, v any) *pyjson.Object {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("result %T", v)
	}
	d, err := pyjson.Loads(s)
	if err != nil {
		t.Fatal(err)
	}
	return d.(*pyjson.Object)
}

func TestCaptureResponseReturnsMultimodalEnvelopeWhenNative(t *testing.T) {
	cuRoute(t, false)
	resp := cuRespond(t, cuCaptureEnv(t), cuRoutedCapture("som"), "")
	if !cuIsMultimodal(resp) {
		t.Fatalf("not multimodal %T", resp)
	}
	o := resp.(*pyjson.Object)
	img := o.Vals["content"].([]any)[1].(*pyjson.Object)
	if img.Vals["type"] != "image_url" || !strings.HasPrefix(img.Vals["image_url"].(*pyjson.Object).Vals["url"].(string), "data:image/png;base64,") {
		t.Fatalf("image part %v", img.Vals)
	}
	if _, ok := o.Vals["vision_analysis"]; ok {
		t.Fatal("vision_analysis on the native path")
	}
	meta := o.Vals["meta"].(*pyjson.Object)
	if strings.Join(meta.Keys[:5], ",") != "mode,width,height,elements,png_bytes" || meta.Vals["width"] != 8 {
		t.Fatalf("meta %v %v", meta.Keys, meta.Vals)
	}
}

func TestCaptureResponseRoutedToAuxVisionReturnsTextWithAnalysis(t *testing.T) {
	cuRoute(t, true)
	env := cuCaptureEnv(t)
	var gotPath, gotPrompt string
	cuSwap(t, &cuVisionAnalyze, func(_ context.Context, _ *tools.Env, path, prompt string) string {
		gotPath, gotPrompt = path, prompt
		if _, err := os.Stat(path); err != nil {
			t.Errorf("temp image missing during analysis %v", err)
		}
		return `{"success": true, "analysis": "A Safari window showing a GitHub issue page with a 'Sign in' button and a 'username' text field."}`
	})
	resp := cuRespond(t, env, cuRoutedCapture("som"), "")
	s, ok := resp.(string)
	if !ok || strings.Contains(s, "data:image") || strings.Contains(s, "image_url") {
		t.Fatalf("routed result %T %v", resp, resp)
	}
	body := cuDecode(t, resp)
	if body.Vals["mode"] != "som" || body.Vals["app"] != "Safari" || body.Vals["window_title"] != "GitHub – Issue #24015" ||
		!strings.Contains(body.Vals["vision_analysis"].(string), "Sign in") || body.Vals["vision_analysis_routed_via"] != "auxiliary.vision" ||
		len(body.Vals["elements"].([]any)) != 2 {
		t.Fatalf("body %v", body.Vals)
	}
	if !strings.HasPrefix(gotPath, filepath.Join(env.Home, "cache", "vision")) || !strings.Contains(gotPrompt, "Sign in") {
		t.Fatalf("analysis call %q %q", gotPath, gotPrompt)
	}
	if _, err := os.Stat(gotPath); !os.IsNotExist(err) {
		t.Fatalf("temp image left behind %v", err)
	}
}

func TestCaptureResponseInvalidAuxResponseDegradesToTextPayload(t *testing.T) {
	cuRoute(t, true)
	cuSwap(t, &cuVisionAnalyze, func(context.Context, *tools.Env, string, string) string { return "" })
	body := cuDecode(t, cuRespond(t, cuCaptureEnv(t), cuRoutedCapture("som"), ""))
	if body.Vals["vision_unavailable"] != true || !strings.Contains(body.Vals["summary"].(string), "vision unavailable") {
		t.Fatalf("body %v", body.Vals)
	}
}

func TestRoutingDecisionWiring(t *testing.T) {
	env := &tools.Env{Home: t.TempDir(), Config: config.Tree{
		"model":     map[string]any{"default": "tencent/hy3-preview", "provider": "openrouter"},
		"auxiliary": map[string]any{"vision": map[string]any{"provider": "openrouter", "model": "google/gemini-2.5-flash"}}}}
	if !shouldRouteThroughAuxVision(context.Background(), env) {
		t.Fatal("explicit auxiliary.vision did not route to aux")
	}
	text := &tools.Env{Home: t.TempDir(), Config: config.Tree{
		"model": map[string]any{"default": "claude-opus-4-5", "provider": "anthropic"},
		"agent": map[string]any{"image_input_mode": "text"}}}
	if !shouldRouteThroughAuxVision(context.Background(), text) {
		t.Fatal("image_input_mode text kept pixels in")
	}
	cuSwap(t, &cuNativeVisionFastPath, func(context.Context, *tools.Env) bool { panic(fmt.Errorf("policy bug")) })
	if shouldRouteThroughAuxVision(context.Background(), env) {
		t.Fatal("a failing decision routed to aux")
	}
}

func TestScreenshotDedupOmitsUnchangedFrames(t *testing.T) {
	cuRoute(t, false)
	cuSwap(t, &persistCaptureImage, func(context.Context, *tools.Env, *CaptureResult) *string { return nil })
	env := cuCaptureEnv(t)
	fresh := func() { resetScreenshotDedupState("", true); t.Cleanup(func() { resetScreenshotDedupState("", true) }) }
	fresh()
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")) {
		t.Fatal("first capture lost its image")
	}
	again := cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	data := cuDecode(t, again)
	if data.Vals["screen_unchanged"] != true || !strings.Contains(data.Vals["summary"].(string), "screen unchanged") || data.Vals["app"] != "kate" {
		t.Fatalf("identical frame %v", data.Vals)
	}
	if _, ok := data.Vals["elements"]; !ok {
		t.Fatal("elements missing")
	}
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 2, "kate"), "s1")) {
		t.Fatal("changed pixels not delivered")
	}
	fresh()
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kcalc"), "s1")) {
		t.Fatal("different target deduped")
	}
	fresh()
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s2")) {
		t.Fatal("sessions shared dedup state")
	}
	fresh()
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "")
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "")) {
		t.Fatal("no session id deduped")
	}
	fresh()
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	cuRespond(t, env, cuDedupCapture(t, 3, "kate"), "s1")
	if cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 3, "kate"), "s1")) {
		t.Fatal("did not dedup against the new frame")
	}
	fresh()
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	ReleaseComputerUseSession(context.Background(), "s1")
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")) {
		t.Fatal("release kept dedup state")
	}
}

func TestScreenshotDedupStreakCapRedeliversPixels(t *testing.T) {
	cuRoute(t, false)
	cuSwap(t, &persistCaptureImage, func(context.Context, *tools.Env, *CaptureResult) *string { return nil })
	resetScreenshotDedupState("", true)
	t.Cleanup(func() { resetScreenshotDedupState("", true) })
	env := cuCaptureEnv(t)
	cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")
	for i := 0; i < cuDedupMaxStreak; i++ {
		if cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")) {
			t.Fatalf("repeat %d delivered pixels", i)
		}
	}
	if !cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")) {
		t.Fatal("streak cap did not redeliver")
	}
	if cuIsMultimodal(cuRespond(t, env, cuDedupCapture(t, 1, "kate"), "s1")) {
		t.Fatal("streak did not restart")
	}
	ax := &CaptureResult{Mode: "ax", Width: 64, Height: 64, App: "kate", WindowTitle: "notes"}
	if body := cuDecode(t, cuRespond(t, env, ax, "s1")); body.Vals["screen_unchanged"] != nil {
		t.Fatalf("ax mode deduped %v", body.Vals)
	}
}

type cuFrameBackend struct {
	noopBackend
	frame    *CaptureResult
	onCapure func()
}

func (b *cuFrameBackend) Capture(string, *string, *int, *int) (*CaptureResult, error) {
	if b.onCapure != nil {
		b.onCapure()
	}
	c := *b.frame
	return &c, nil
}

func TestExplicitCaptureAfterInputIsSubjectToDedup(t *testing.T) {
	cuRoute(t, false)
	cuSwap(t, &persistCaptureImage, func(context.Context, *tools.Env, *CaptureResult) *string { return nil })
	resetScreenshotDedupState("", true)
	t.Cleanup(func() { resetScreenshotDedupState("", true) })
	env := cuCaptureEnv(t)
	b := &cuFrameBackend{frame: cuDedupCapture(t, 1, "kate")}
	sid := "s1"
	run := func(action string, args map[string]any) any {
		out, err := dispatchComputerUse(context.Background(), env, b, action, args, func() error { return nil }, &sid)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if !cuIsMultimodal(run("capture", map[string]any{"mode": "som"})) {
		t.Fatal("first capture")
	}
	after := run("click", map[string]any{"element": int64(1), "capture_after": true})
	body := cuDecode(t, after)
	if body.Vals["screen_unchanged"] != true || body.Vals["ok"] != true {
		t.Fatalf("capture_after %v", body.Vals)
	}
	if cuIsMultimodal(run("capture", map[string]any{"mode": "som"})) {
		t.Fatal("explicit capture not deduped")
	}
	if !cuIsMultimodal(run("capture", map[string]any{"mode": "som"})) {
		t.Fatal("streak cap on the dispatch path")
	}
}

func TestElementBoundsNulledOnlyForZeroRect(t *testing.T) {
	zero := elementToDict(UIElement{Index: 2, Role: "radio button", Label: "Deg"})
	if zero.Vals["bounds"] != nil || zero.Vals["index"] != 2 {
		t.Fatalf("zero rect %v", zero.Vals)
	}
	real := elementToDict(UIElement{Index: 2, Role: "radio button", Label: "Deg", Bounds: [4]int{10, 20, 30, 40}})
	if fmt.Sprint(real.Vals["bounds"]) != "[10 20 30 40]" {
		t.Fatalf("real rect %v", real.Vals["bounds"])
	}
}

func TestCappedWalkHintNamesTheCapOnlyWhenHit(t *testing.T) {
	cuSwap(t, &spillElementsToFile, func(context.Context, *tools.Env, *CaptureResult) *string { p := "/tmp/elements.json"; return &p })
	for _, c := range []struct {
		n, bound int
		capped   bool
	}{{8, 8, true}, {8, 50, false}, {8, 0, false}} {
		var els []UIElement
		for i := 0; i < c.n; i++ {
			els = append(els, UIElement{Index: i + 1, Role: "AXButton", Label: fmt.Sprintf("b%d", i)})
		}
		capture := &CaptureResult{Mode: "ax", Width: 800, Height: 600, AXMaxElements: c.bound, Elements: els}
		text := strings.Join(captureSummaryLines(newCaptureView(context.Background(), cuCaptureEnv(t), capture, 5)), "\n")
		if strings.Contains(text, "accessibility walk capped") != c.capped ||
			strings.Contains(text, "full element tree with untruncated labels saved to") == c.capped ||
			!strings.Contains(text, "element tree with untruncated labels saved to") {
			t.Fatalf("%+v %s", c, text)
		}
	}
}

func TestCaptureResponseAppendsNoteLine(t *testing.T) {
	cuRoute(t, false)
	b64 := cuTestPNG8
	raw, _ := base64.StdEncoding.DecodeString(b64)
	capture := &CaptureResult{Mode: "vision", Width: 8, Height: 8, PNGB64: &b64, App: "screen", WindowTitle: "Full screen (composited)",
		PNGBytesLen: len(raw), Note: "full-screen capture has no interactable elements; call capture(app='desktop') for the desktop shell"}
	resp := cuRespond(t, cuCaptureEnv(t), capture, "")
	if !strings.Contains(resp.(*pyjson.Object).Vals["text_summary"].(string), "full-screen capture has no interactable elements") {
		t.Fatalf("note missing %v", resp)
	}
}

func TestFrameCapturedAcrossATakeoverIsDroppedBeforeAnySink(t *testing.T) {
	for _, args := range []map[string]any{{"action": "capture"}, {"action": "click", "coordinate": []any{int64(1), int64(1)}, "capture_after": true}} {
		human, epoch := false, 1
		cuLease(t, &human, &epoch)
		env, _ := cuDispatchEnv(t, "off")
		secret := base64.StdEncoding.EncodeToString([]byte("SECRETPNG"))
		b := &cuFrameBackend{frame: &CaptureResult{Mode: "som", Width: 64, Height: 64, PNGB64: &secret, App: "Bank", WindowTitle: "login"},
			onCapure: func() { epoch++ }}
		cuBackendMu.Lock()
		cuInjectedBackend = b
		cuBackendMu.Unlock()
		var leaked []string
		cuSwap(t, &persistCaptureImage, func(context.Context, *tools.Env, *CaptureResult) *string {
			leaked = append(leaked, "persist")
			return nil
		})
		cuSwap(t, &spillElementsToFile, func(context.Context, *tools.Env, *CaptureResult) *string {
			leaked = append(leaked, "spill")
			return nil
		})
		cuSwap(t, &shouldRouteThroughAuxVision, func(context.Context, *tools.Env) bool { leaked = append(leaked, "aux-decide"); return true })
		cuSwap(t, &routeCaptureThroughAuxVision, func(context.Context, *tools.Env, *CaptureResult, string, []UIElement, int, *string, *string) *string {
			leaked = append(leaked, "aux")
			return nil
		})
		got := cuHandle(t, env, args)
		if !strings.Contains(got, `"code": "human_has_control"`) || len(leaked) != 0 || strings.Contains(got, secret) {
			t.Fatalf("%v leaked %v result %s", args, leaked, got)
		}
	}
}

func TestShrinkCaptureForVisionNoteRecoversOriginalPosition(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 3024, 1964))
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	for x := 2400; x < 2410; x++ {
		for y := 1500; y < 1510; y++ {
			img.SetNRGBA(x, y, color.NRGBA{255, 0, 0, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	out, note := shrinkCaptureForVision(buf.Bytes(), ".png", cuMaxVisionDim)
	if note == nil || !strings.Contains(*note, "downscaled") || !strings.Contains(*note, "3024x1964") || !strings.Contains(*note, "2.08") {
		t.Fatalf("note %v", note)
	}
	var ow, oh, nw, nh int
	if _, err := fmt.Sscanf((*note)[strings.Index(*note, "from "):], "from %dx%d to %dx%d", &ow, &oh, &nw, &nh); err != nil {
		t.Fatal(err)
	}
	shrunk, err := png.Decode(bytes.NewReader(out))
	if err != nil || shrunk.Bounds().Dx() != nw || shrunk.Bounds().Dy() != nh || max(nw, nh) > 1456 {
		t.Fatalf("shrunk %v %v", shrunk.Bounds(), err)
	}
	var xs, ys []int
	for x := 0; x < nw; x++ {
		for y := 0; y < nh; y++ {
			r, g, b, _ := shrunk.At(x, y).RGBA()
			if r>>8 > 100 && g>>8 < 100 && b>>8 < 100 {
				xs, ys = append(xs, x), append(ys, y)
			}
		}
	}
	if len(xs) == 0 {
		t.Fatal("red marker square not found in image")
	}
	cx := float64(xs[0]+xs[len(xs)-1]) / 2
	minY, maxY := ys[0], ys[0]
	for _, y := range ys {
		minY, maxY = min(minY, y), max(maxY, y)
	}
	cy := float64(minY+maxY) / 2
	rx, ry := cx*float64(ow)/float64(nw), cy*float64(oh)/float64(nh)
	if rx < 2404.5-2 || rx > 2404.5+2 || ry < 1504.5-2 || ry > 1504.5+2 {
		t.Fatalf("recovered %.1f %.1f", rx, ry)
	}
}

func TestShrinkCaptureForVisionLeavesSmallAndUndecodableBytes(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 800, 600))); err != nil {
		t.Fatal(err)
	}
	if out, note := shrinkCaptureForVision(buf.Bytes(), ".png", cuMaxVisionDim); note != nil || !bytes.Equal(out, buf.Bytes()) {
		t.Fatalf("under cap changed %v", note)
	}
	if out, note := shrinkCaptureForVision([]byte("not an image at all"), ".png", cuMaxVisionDim); note != nil || string(out) != "not an image at all" {
		t.Fatalf("undecodable changed %v", note)
	}
}

func TestComputerUseCacheFilesOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		return
	}
	t.Setenv("ICEBATH_MANAGED", "")
	env := &tools.Env{Home: filepath.Join(t.TempDir(), "hh")}
	path, err := cacheFile(context.Background(), env, "cache/vision", "temp_vision_images", "capture.png", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateBytes(path, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	dirSt, _ := os.Stat(filepath.Dir(path))
	fileSt, _ := os.Stat(path)
	if string(data) != "frame" || dirSt.Mode().Perm() != 0o700 || fileSt.Mode().Perm() != 0o600 {
		t.Fatalf("data %q dir %v file %v", data, dirSt.Mode().Perm(), fileSt.Mode().Perm())
	}
}

func TestCacheFilePrunesOldestDownToTheCap(t *testing.T) {
	env := &tools.Env{Home: t.TempDir()}
	dir := filepath.Join(env.Home, "cache", "computer_use")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cuMaxSpillFiles+3; i++ {
		p := filepath.Join(dir, fmt.Sprintf("elements_%02d.json", i))
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(1_700_000_000+int64(i), 0)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	capture := &CaptureResult{Mode: "ax", App: "kate", Elements: []UIElement{{Index: 1, Role: "AXButton", Label: "é"}}}
	spilled := spillElementsToFile(context.Background(), env, capture)
	if spilled == nil {
		t.Fatal("spill failed")
	}
	left, _ := filepath.Glob(filepath.Join(dir, "elements_*.json"))
	if len(left) != cuMaxSpillFiles {
		t.Fatalf("%d files left", len(left))
	}
	if _, err := os.Stat(filepath.Join(dir, "elements_03.json")); !os.IsNotExist(err) {
		t.Fatal("fourth oldest file survived the prune")
	}
	if _, err := os.Stat(filepath.Join(dir, "elements_04.json")); err != nil {
		t.Fatalf("fifth oldest file pruned %v", err)
	}
	text, _ := os.ReadFile(*spilled)
	if !strings.Contains(string(text), "\"label\": \"é\"") || !strings.Contains(string(text), "\n \"window_title\": \"\"") {
		t.Fatalf("spill text %s", text)
	}
}

func TestAuxVisionRouteFollowsAConfigChangeWithoutRestart(t *testing.T) {
	agent := map[string]any{"image_input_mode": "native"}
	env := &tools.Env{Home: t.TempDir(), Config: config.Tree{
		"model": map[string]any{"default": "claude-opus-4-5", "provider": "anthropic"}, "agent": agent}}
	if shouldRouteThroughAuxVision(context.Background(), env) {
		t.Fatal("native mode on a vision main model routed to aux")
	}
	agent["image_input_mode"] = "text"
	if !shouldRouteThroughAuxVision(context.Background(), env) {
		t.Fatal("the image_input_mode edit did not reach the next capture")
	}
}
