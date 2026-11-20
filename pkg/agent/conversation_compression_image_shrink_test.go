package agent

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Classifies a fake provider rejection the way classify_api_error reads status, message and body
func classifyShrinkRejection(t *testing.T, message, body, prov, model string) *provider.Classified {
	t.Helper()
	if body == "" {
		body = fmt.Sprintf(`{"error": {"message": %q}}`, message)
	}
	return provider.ClassifyError(provider.ClassifyInput{Err: errors.New(message), Status: 400, Body: body, Provider: prov, Model: model})
}

func TestAnthropic400ImageExceedsMessage(t *testing.T) {
	r := classifyShrinkRejection(t, "messages.0.content.1.image.source.base64: image exceeds 5 MB maximum: 12966600 bytes > 5242880 bytes", "", "anthropic", "claude-sonnet-4-6")
	if r.Reason != provider.ReasonImageTooLarge || !r.Retryable {
		t.Fatalf("got %s retryable=%v", r.Reason, r.Retryable)
	}
}

func TestMinimax400MediaExceedsMessage(t *testing.T) {
	msg := "media exceeds size limit: max 10485760 bytes (2013)"
	body := `{"type": "error", "error": {"type": "invalid_request_error", "message": "` + msg + `"}}`
	r := classifyShrinkRejection(t, msg, body, "minimax", "MiniMax-M3")
	if r.Reason != provider.ReasonImageTooLarge || !r.Retryable {
		t.Fatalf("got %s retryable=%v", r.Reason, r.Retryable)
	}
}

func TestCodex400PatchBudgetMessage(t *testing.T) {
	r := classifyShrinkRejection(t, "The image you provided requires 33174 patches after processing, exceeding the limit of 30000. Please resize the image and try again.", "", "openai-codex", "gpt-5.6-sol")
	if r.Reason != provider.ReasonImageTooLarge || !r.Retryable {
		t.Fatalf("got %s retryable=%v", r.Reason, r.Retryable)
	}
}

func TestUnrelated400StillNotImageTooLarge(t *testing.T) {
	r := classifyShrinkRejection(t, `messages: unexpected role "tool" at index 3`, "", "minimax", "MiniMax-M3")
	if r.Reason == provider.ReasonImageTooLarge {
		t.Fatalf("an unrelated 400 classified as %s", r.Reason)
	}
}

// Message and body of a pydantic string_type rejection whose input carries an inline image
func pydanticShrink400(loc, url string) (string, string) {
	body := `{"detail": [{"type": "string_type", "loc": ` + loc + `, "msg": "Input should be a valid string", "input": [{"type": "text", "text": "what is this"}, {"type": "image_url", "image_url": {"url": "` + url + `"}}]}]}`
	return "Error code: 400 - " + body, body
}

func TestOversizeRejectionsReportedAs400Shrink(t *testing.T) {
	nim := "Please make sure your payload is below 26214400 bytes in size. For larger payloads, please use the Assets API."
	dashscope := "String value length (28049408) exceeds the maximum allowed (28000000, from `StreamReadConstraints.getMaxStringLength()`)"
	big := "data:image/png;base64," + strings.Repeat("A", 11*1024*1024)
	small := "data:image/png;base64," + strings.Repeat("A", 64*1024)
	type rejection struct{ message, body string }
	pyd := func(loc, url string) rejection {
		m, b := pydanticShrink400(loc, url)
		return rejection{m, b}
	}
	oversize := []rejection{
		{nim, fmt.Sprintf(`{"error": {"message": %q, "code": "invalid_image_format"}}`, nim)},
		{dashscope, ""},
		pyd(`["body", "messages", 0, "content", "str"]`, big),
	}
	for _, e := range oversize {
		r := classifyShrinkRejection(t, e.message, e.body, "custom", "x")
		if r.Reason != provider.ReasonImageTooLarge || !r.Retryable {
			t.Fatalf("%.120s: got %s retryable=%v", e.message, r.Reason, r.Retryable)
		}
	}
	controls := map[provider.FailoverReason][]rejection{
		provider.ReasonMultimodalToolContent: {
			pyd(`["body", "messages", 3, "tool", "content", "str"]`, big),
			pyd(`["body", "messages", 0, "content", "str"]`, small),
		},
		provider.ReasonFormatError: {{"Unsupported parameter: 'max_tokens' is not supported with this model.", ""}},
	}
	for want, errs := range controls {
		for _, e := range errs {
			if r := classifyShrinkRejection(t, e.message, e.body, "custom", "x"); r.Reason != want {
				t.Fatalf("%.120s: got %s want %s", e.message, r.Reason, want)
			}
		}
	}
}

// Data URL with a PNG signature followed by size kilobytes of filler
func bigPNGDataURL(sizeKB int) string {
	raw := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("X"), sizeKB*1024)...)
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

// Replaces the pixel decoder with a fixed sequence of sizes, the last repeated once the sequence runs out
func installFakeShrinkPixels(t *testing.T, sizes ...[2]int) {
	t.Helper()
	calls := 0
	prev := imageShrinkOpenSize
	imageShrinkOpenSize = func([]byte) (int, int, error) {
		s := sizes[min(calls, len(sizes)-1)]
		calls++
		return s[0], s[1], nil
	}
	t.Cleanup(func() { imageShrinkOpenSize = prev })
}

// Replaces the vision resizer for one test
func installFakeShrinkResize(t *testing.T, fn func(path, mime string, maxBase64Bytes, maxDimension int) (string, error)) {
	t.Helper()
	prev := imageShrinkResize
	imageShrinkResize = fn
	t.Cleanup(func() { imageShrinkResize = prev })
}

func returningShrunk(url string) func(string, string, int, int) (string, error) {
	return func(string, string, int, int) (string, error) { return url, nil }
}

func imageURLPart(url string) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func partURL(t *testing.T, msgs []any, i int) string {
	t.Helper()
	part := msgs[0].(map[string]any)["content"].([]any)[i].(map[string]any)
	return part["image_url"].(map[string]any)["url"].(string)
}

func TestShrinkNoMessagesReturnsFalse(t *testing.T) {
	if tryShrinkImagePartsInMessages([]any{}, imageShrinkDefaultMaxDimension) {
		t.Fatal("an empty list shrank")
	}
	if tryShrinkImagePartsInMessages(nil, imageShrinkDefaultMaxDimension) {
		t.Fatal("None shrank")
	}
}

func TestShrinkSmallImagePartNotShrunk(t *testing.T) {
	small := bigPNGDataURL(100)
	hits := 0
	installFakeShrinkResize(t, func(string, string, int, int) (string, error) { hits++; return small, nil })
	msgs := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "hi"}, imageURLPart(small),
	}}}
	if tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) {
		t.Fatal("a small image reported progress")
	}
	if hits != 0 || partURL(t, msgs, 1) != small {
		t.Fatalf("resize hits %d, url changed %v", hits, partURL(t, msgs, 1) != small)
	}
}

func TestShrinkOversizedImageURLDictShapeRewritten(t *testing.T) {
	shrunk := "data:image/jpeg;base64," + strings.Repeat("A", 1000)
	installFakeShrinkResize(t, returningShrunk(shrunk))
	msgs := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look"}, imageURLPart(bigPNGDataURL(5000)),
	}}}
	if !tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) || partURL(t, msgs, 1) != shrunk {
		t.Fatal("the chat completions image_url dict was not rewritten")
	}
}

func TestShrinkAnthropicBase64ImageSourceRewritten(t *testing.T) {
	installFakeShrinkPixels(t, [2]int{2501, 100}, [2]int{1500, 60})
	_, originalData, _ := strings.Cut(bigPNGDataURL(100), ",")
	shrunk := "data:image/jpeg;base64," + strings.Repeat("N", 1000)
	var seenMIME string
	var seenMax int
	installFakeShrinkResize(t, func(_, mime string, _, maxDimension int) (string, error) {
		seenMIME, seenMax = mime, maxDimension
		return shrunk, nil
	})
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{
		"type":   "image",
		"source": map[string]any{"type": "base64", "media_type": "image/png", "data": originalData},
	}}}}
	changed := tryShrinkImagePartsInMessages(msgs, 2000)
	source := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if !changed || seenMIME != "image/png" || seenMax != 2000 {
		t.Fatalf("changed=%v mime=%q max=%d", changed, seenMIME, seenMax)
	}
	if source["type"] != "base64" || source["media_type"] != "image/jpeg" || source["data"] != strings.Repeat("N", 1000) {
		t.Fatalf("source %v", source["media_type"])
	}
}

func TestShrinkMultipleImagesAllShrunk(t *testing.T) {
	shrunk := "data:image/jpeg;base64," + strings.Repeat("C", 500)
	installFakeShrinkResize(t, returningShrunk(shrunk))
	msgs := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "compare"}, imageURLPart(bigPNGDataURL(5000)), imageURLPart(bigPNGDataURL(6000)),
	}}}
	if !tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) || partURL(t, msgs, 1) != shrunk || partURL(t, msgs, 2) != shrunk {
		t.Fatal("both oversized images must be rewritten")
	}
}

func TestShrinkFailureReturnsFalseAndLeavesURLIntact(t *testing.T) {
	oversized := bigPNGDataURL(5000)
	installFakeShrinkResize(t, returningShrunk(""))
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(oversized)}}}
	if tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) || partURL(t, msgs, 0) != oversized {
		t.Fatal("a failed re-encode must leave the url and report no progress")
	}
}

func TestShrinkResizeErrorCountsAsUnshrinkable(t *testing.T) {
	oversized := bigPNGDataURL(5000)
	installFakeShrinkResize(t, func(string, string, int, int) (string, error) { return "", errors.New("cannot write mode P as JPEG") })
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(oversized)}}}
	if tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) || partURL(t, msgs, 0) != oversized {
		t.Fatal("a raised re-encode must leave the url and report no progress")
	}
}

func TestShrinkMixedOneShrinkableOneNotReturnsFalse(t *testing.T) {
	shrinkable, unshrinkable := bigPNGDataURL(5000), bigPNGDataURL(6000)
	small := "data:image/jpeg;base64," + strings.Repeat("C", 500)
	installFakeShrinkResize(t, func(path string, _ string, _, _ int) (string, error) {
		var size int64
		if st, err := os.Stat(path); err == nil {
			size = st.Size()
		}
		if size > 5500*1024 {
			return unshrinkable, nil
		}
		return small, nil
	})
	msgs := []any{map[string]any{"role": "tool", "content": []any{imageURLPart(shrinkable), imageURLPart(unshrinkable)}}}
	if tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) {
		t.Fatal("a surviving oversized image must block the retry")
	}
	if partURL(t, msgs, 0) != small || partURL(t, msgs, 1) != unshrinkable {
		t.Fatal("the shrinkable part is rewritten and the unshrinkable one kept")
	}
}

func TestShrinkDimensionShrinkWithByteGrowthAccepted(t *testing.T) {
	installFakeShrinkPixels(t, [2]int{2501, 100}, [2]int{1500, 60})
	original := bigPNGDataURL(100)
	grown := "data:image/png;base64," + strings.Repeat("G", 200*1024)
	seenMax := 0
	installFakeShrinkResize(t, func(_, _ string, _, maxDimension int) (string, error) { seenMax = maxDimension; return grown, nil })
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(original)}}}
	if len(grown) <= len(original) {
		t.Fatal("the re-encode must be byte larger")
	}
	if !tryShrinkImagePartsInMessages(msgs, 2000) || seenMax != 2000 || partURL(t, msgs, 0) != grown {
		t.Fatal("a dimension correct re-encode must be kept even when its bytes grew")
	}
}

func TestShrinkDimensionShrinkFailureStillBlocksRetry(t *testing.T) {
	installFakeShrinkPixels(t, [2]int{2501, 100})
	original := bigPNGDataURL(100)
	installFakeShrinkResize(t, returningShrunk("data:image/png;base64,"+strings.Repeat("H", 120*1024)))
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(original)}}}
	if tryShrinkImagePartsInMessages(msgs, 2000) || partURL(t, msgs, 0) != original {
		t.Fatal("a still oversized re-encode must block the retry and keep the original")
	}
}

func TestShrinkMixedDimensionPartialProgressReturnsFalse(t *testing.T) {
	installFakeShrinkPixels(t, [2]int{2501, 100}, [2]int{1500, 60}, [2]int{2501, 100}, [2]int{2501, 100})
	calls := 0
	installFakeShrinkResize(t, func(string, string, int, int) (string, error) {
		calls++
		if calls == 1 {
			return "data:image/png;base64," + strings.Repeat("G", 200*1024), nil
		}
		return "data:image/png;base64," + strings.Repeat("H", 120*1024), nil
	})
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(bigPNGDataURL(100)), imageURLPart(bigPNGDataURL(90))}}}
	if tryShrinkImagePartsInMessages(msgs, 2000) {
		t.Fatal("partial dimension progress must not burn the retry")
	}
}

func TestShrinkByteOversizedButPixelOversizedAfterShrinkBlocksRetry(t *testing.T) {
	installFakeShrinkPixels(t, [2]int{2560, 64})
	oversized := bigPNGDataURL(5000)
	smaller := "data:image/png;base64," + strings.Repeat("K", 1000)
	installFakeShrinkResize(t, returningShrunk(smaller))
	msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(oversized)}}}
	if len(smaller) >= len(oversized) {
		t.Fatal("the re-encode must be byte smaller")
	}
	if tryShrinkImagePartsInMessages(msgs, 2000) || partURL(t, msgs, 0) != oversized {
		t.Fatal("a byte shrink still over the pixel cap must block the retry")
	}
}

func TestShrinkResponsesInputImageStringAndMapContent(t *testing.T) {
	shrunk := "data:image/jpeg;base64," + strings.Repeat("R", 800)
	installFakeShrinkResize(t, returningShrunk(shrunk))
	msgs := []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_image", "image_url": bigPNGDataURL(5000)}}}}
	if !tryShrinkImagePartsInMessages(msgs, imageShrinkDefaultMaxDimension) {
		t.Fatal("the Responses input_image string shape must shrink")
	}
	if got := msgs[0]["content"].([]map[string]any)[0]["image_url"]; got != shrunk {
		t.Fatal("the input_image url was not replaced")
	}
}

func TestShrinkDoesNotMutateAliasedHistoryParts(t *testing.T) {
	oversized := bigPNGDataURL(5000)
	shrunk := "data:image/jpeg;base64," + strings.Repeat("C", 1000)
	installFakeShrinkResize(t, returningShrunk(shrunk))
	historyPart := imageURLPart(oversized)
	historyContent := []any{historyPart}
	historyMsg := map[string]any{"role": "user", "content": historyContent}
	apiMsg := map[string]any{}
	for k, v := range historyMsg {
		apiMsg[k] = v
	}
	if !tryShrinkImagePartsInMessages([]any{apiMsg}, imageShrinkDefaultMaxDimension) {
		t.Fatal("the per call copy must shrink")
	}
	if apiMsg["content"].([]any)[0].(map[string]any)["image_url"].(map[string]any)["url"] != shrunk {
		t.Fatal("the per call part was not rewritten")
	}
	stored := historyMsg["content"].([]any)[0].(map[string]any)
	if fmt.Sprintf("%p", stored) != fmt.Sprintf("%p", historyPart) || historyPart["image_url"].(map[string]any)["url"] != oversized {
		t.Fatal("the stored history part was mutated")
	}
}

// Flat UI screenshot PNG that compresses under the byte budget while its sides exceed the pixel cap
func screenshotPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{245, 245, 247, 255}}, image.Point{}, draw.Src)
	for y := 0; y < height; y += 40 {
		shade := uint8(255 - (y/40)%6*4)
		draw.Draw(img, image.Rect(20, y+5, width-20, min(y+31, height)), &image.Uniform{color.RGBA{shade, 250, 250, 255}}, image.Point{}, draw.Src)
	}
	for x := 0; x < width; x += 160 {
		draw.Draw(img, image.Rect(x, 0, min(x+3, width), height), &image.Uniform{color.RGBA{220, 220, 225, 255}}, image.Point{}, draw.Src)
	}
	for i := 0; i < 360 && 40+i*6+4 < width; i++ {
		if i%13 != 12 {
			draw.Draw(img, image.Rect(40+i*6, 40, 40+i*6+4, 48), &image.Uniform{color.RGBA{20, 20, 20, 255}}, image.Point{}, draw.Src)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestIssue48013DimensionShrinkDoesNotBrick(t *testing.T) {
	const manyImageCap = 2000
	cases := [][2]int{{2344, 778}, {2374, 1144}, {2097, 1476}, {2247, 1544}, {2263, 1644}}
	for _, c := range cases {
		url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(screenshotPNG(t, c[0], c[1]))
		if len(url) > imageShrinkTargetBytes || max(c[0], c[1]) <= manyImageCap {
			t.Fatalf("%v must be under the byte budget and over the pixel cap", c)
		}
		msgs := []any{map[string]any{"role": "user", "content": []any{imageURLPart(url)}}}
		changed := tryShrinkImagePartsInMessages(msgs, manyImageCap)
		_, payload, _ := strings.Cut(partURL(t, msgs, 0), ",")
		raw, err := base64.StdEncoding.DecodeString(payload)
		if err != nil {
			t.Fatal(err)
		}
		w, h, err := tools.RasterSize(raw)
		if err != nil {
			t.Fatal(err)
		}
		if max(w, h) > manyImageCap || !changed {
			t.Fatalf("BRICK: %v left at %dx%d changed=%v", c, w, h, changed)
		}
	}
}
