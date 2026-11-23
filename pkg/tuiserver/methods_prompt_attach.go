package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/filedrop"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Clipboard image helpers set by the binary
var (
	SaveClipboardImage    func(dest string) bool
	HasClipboardImage     func() bool
	ClipboardImageBlocker func() (missing string, noDesktop bool)
)

var promptAttachRegistry = &handlerRegistry{}

func init() {
	promptAttachRegistry.method("input.detect_drop", inputDetectDrop)
	promptAttachRegistry.install()
}

// File name of an image with its pixel size and token estimate, the size nil when it does not decode
func imageSize(path string) (name string, width, height, tokens *int64) {
	meta := imageMeta(path)
	name, _ = meta["name"].(string)
	if w, decoded := meta["width"].(int); decoded {
		h, _ := meta["height"].(int)
		t, _ := meta["token_estimate"].(int)
		return name, ptrInt64(w), ptrInt64(h), ptrInt64(t)
	}
	return name, nil, nil, nil
}

// Pointer to an int64 of an int
func ptrInt64(n int) *int64 {
	v := int64(n)
	return &v
}

// Detects a dropped path in typed text, queuing an image or describing a file
func inputDetectDrop(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessNowait(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	text := ""
	if truthy(params["text"]) {
		text = strOr(params["text"])
	}
	dropped := filedrop.DetectFileDrop(text)
	if dropped == nil {
		return ok(rid, tuigateway.Plain(tuigateway.InputDetectDropResult{}))
	}
	if dropped.IsImage {
		session.Set("attached_images", append(stringList(session.Get("attached_images")), dropped.Path))
		shown := dropped.Remainder
		if shown == "" {
			shown = "[User attached image: " + pypath.Name(dropped.Path) + "]"
		}
		count := int64(len(stringList(session.Get("attached_images"))))
		name, width, height, tokens := imageSize(dropped.Path)
		return ok(rid, tuigateway.Plain(tuigateway.InputDetectDropResult{Matched: true, IsImage: true, Path: dropped.Path, Count: &count, Text: shown,
			Name: name, Width: width, Height: height, TokenEstimate: tokens}))
	}
	shown := "[User attached file: " + dropped.Path + "]"
	if dropped.Remainder != "" {
		shown += "\n" + dropped.Remainder
	}
	return ok(rid, tuigateway.Plain(tuigateway.InputDetectDropResult{Matched: true, Path: dropped.Path, Name: pypath.Name(dropped.Path), Text: shown}))
}
