package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/provider"
)

func TestImageErrorMaxDimensionReadsPatchBudgetAndPixelCeilings(t *testing.T) {
	patch := &provider.APIError{StatusCode: 400, Message: "The image you provided requires 33174 patches after processing, " +
		"exceeding the limit of 30000. Please resize the image and try again."}
	if got := imageErrorMaxDimension(patch); got == nil || *got != 5536 {
		t.Fatalf("patch budget cap %v", got)
	}
	for _, msg := range []string{
		"request exceeds the limit of 100 images per minute",
		"image dimensions exceed max allowed size: 10000 pixels",
		"something unrelated",
	} {
		if got := imageErrorMaxDimension(&provider.APIError{StatusCode: 400, Message: msg}); got != nil {
			t.Fatalf("%q read as cap %d", msg, *got)
		}
	}
	pixel := &provider.APIError{StatusCode: 400, Body: `{"error":{"message":"image dimensions exceed max allowed size for many-image requests: 2000 pixels"}}`}
	if got := imageErrorMaxDimension(pixel); got == nil || *got != 2000 {
		t.Fatalf("pixel ceiling %v", got)
	}
	if imageErrorMaxDimension(nil) != nil {
		t.Fatal("nil error read as a cap")
	}
}

func TestCodexPatchBudgetShrinksResponsesImageUnderBudget(t *testing.T) {
	err := &provider.APIError{StatusCode: 400, Message: "The image you provided requires 33174 patches after processing, " +
		"exceeding the limit of 30000. Please resize the image and try again."}
	limit := imageErrorMaxDimension(err)
	if limit == nil || *limit >= 8000 {
		t.Fatalf("cap %v", limit)
	}
	img := image.NewPaletted(image.Rect(0, 0, 5444, 6200), color.Palette{color.RGBA{200, 30, 30, 255}})
	var buf bytes.Buffer
	if e := png.Encode(&buf, img); e != nil {
		t.Fatal(e)
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
	msgs := []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": url}}}}
	if !tryShrinkImagePartsInMessages(msgs, *limit) {
		t.Fatal("shrink pass left the image unchanged")
	}
	shrunk := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)["image_url"].(string)
	raw, e := base64.StdEncoding.DecodeString(shrunk[strings.Index(shrunk, ",")+1:])
	if e != nil {
		t.Fatal(e)
	}
	cfg, _, e := image.DecodeConfig(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	if patches := ((cfg.Width + 31) / 32) * ((cfg.Height + 31) / 32); patches > 30000 {
		t.Fatalf("%dx%d still needs %d patches", cfg.Width, cfg.Height, patches)
	}
}

func TestRuntimeUsesASCIIEncodingReadsTheLocaleCodeset(t *testing.T) {
	prev := recoveryLocaleEnv
	t.Cleanup(func() { recoveryLocaleEnv = prev })
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{"LANG": "en_US.UTF-8"}, false},
		{map[string]string{"LANG": "C"}, false},
		{map[string]string{}, false},
		{map[string]string{"LANG": "en_US.ANSI_X3.4-1968"}, true},
		{map[string]string{"LC_ALL": "C.ascii", "LANG": "en_US.UTF-8"}, true},
		{map[string]string{"LC_CTYPE": "en_US.US-ASCII@euro"}, true},
		{map[string]string{"LC_ALL": "en_US.UTF-8", "LC_CTYPE": "C.ascii"}, false},
	} {
		recoveryLocaleEnv = func(key string) string { return tc.env[key] }
		if got := runtimeUsesASCIIEncoding(); got != tc.want {
			t.Fatalf("%v read as ascii %v", tc.env, got)
		}
	}
}

func TestPlinesPrintsEveryLineBehindTheLogPrefix(t *testing.T) {
	t.Setenv("ICEBATH_HOME", t.TempDir())
	var printed []string
	prev := recoveryPrint
	recoveryPrint = func(line string) { printed = append(printed, line) }
	t.Cleanup(func() { recoveryPrint = prev })
	plines(&AIAgent{logPrefix: "[w1] ", platform: "cli"}, "first", "second")
	if strings.Join(printed, "|") != "[w1] first|[w1] second" {
		t.Fatalf("printed %q", printed)
	}
}

func TestStripRequestImagesAndRetryStripsOnlyTheRequestCopy(t *testing.T) {
	a := &AIAgent{}
	rows := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "look"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
	}}}
	if !stripRequestImagesAndRetry(a, &rows) {
		t.Fatal("image part not stripped")
	}
	if strings.Contains(strings.ToLower(fmt.Sprint(rows)), "image_url") {
		t.Fatalf("image left in %v", rows)
	}
	if stripRequestImagesAndRetry(a, &rows) {
		t.Fatal("a text only request retried")
	}
	if stripRequestImagesAndRetry(a, nil) {
		t.Fatal("nil request retried")
	}
}

func TestActivateCodexAppServerFallbackSkipsTurnsTheChainCannotTake(t *testing.T) {
	ctx := context.Background()
	a := &AIAgent{provider: "openai-codex", model: "gpt-5.5"}
	if activateCodexAppServerFallback(ctx, a, map[string]any{"error": "You've hit your usage limit (429)"}) {
		t.Fatal("no fallback configured must not activate")
	}
	a.fallbackChain = []map[string]any{{"provider": "openrouter", "model": "x"}}
	for _, result := range []map[string]any{
		{},
		{"error": ""},
		{"error": "rate limit exceeded", "interrupted": true},
		{"error": "the model printed something unexpected"},
	} {
		if activateCodexAppServerFallback(ctx, a, result) {
			t.Fatalf("result %v activated a fallback", result)
		}
	}
	if len(a.retryStatusBuffer) != 0 {
		t.Fatalf("buffered %v", a.retryStatusBuffer)
	}
}
