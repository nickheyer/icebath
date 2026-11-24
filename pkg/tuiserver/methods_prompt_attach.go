package tuiserver

import (
	"context"

	"github.com/nickheyer/icebath/pkg/filedrop"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tuigateway"
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

// Detects a dropped path in typed text and describes its file as a mention the model can open by path, an image with its size
func inputDetectDrop(_ context.Context, rid any, params map[string]any) map[string]any {
	p, err := tuigateway.InputDetectDrop.Params(params)
	if err != nil {
		return rpcErr(rid, 4000, err.Error(), nil)
	}
	dropped := filedrop.DetectFileDrop(p.Text, p.Cwd)
	if dropped == nil {
		return ok(rid, tuigateway.Plain(tuigateway.InputDetectDropResult{}))
	}
	shown := "[User attached file: " + dropped.Path + "]"
	if dropped.Remainder != "" {
		shown += "\n" + dropped.Remainder
	}
	result := tuigateway.InputDetectDropResult{Matched: true, IsImage: dropped.IsImage, Path: dropped.Path, Name: pypath.Name(dropped.Path), Text: shown}
	if dropped.IsImage {
		_, result.Width, result.Height, result.TokenEstimate = imageSize(dropped.Path)
	}
	return ok(rid, tuigateway.Plain(result))
}
