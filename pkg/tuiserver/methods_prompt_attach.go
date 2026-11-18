package tuiserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/filedrop"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/tuihost"
)

// Clipboard image helpers set by the binary
var (
	SaveClipboardImage func(dest string) bool
	HasClipboardImage  func() bool
)

var promptAttachRegistry = &handlerRegistry{}

func init() {
	promptAttachRegistry.method("clipboard.paste", clipboardPaste)
	promptAttachRegistry.method("image.attach", imageAttach)
	promptAttachRegistry.method("image.attach_bytes", imageAttachBytes)
	promptAttachRegistry.method("image.detach", imageDetach)
	promptAttachRegistry.method("input.detect_drop", inputDetectDrop)
	promptAttachRegistry.install()
}

// Attached reply after queuing an image with its count and image meta
func attachedImageResult(session *tuihost.Session, imagePath string, extra map[string]any) map[string]any {
	out := map[string]any{"attached": true, "path": imagePath, "count": len(stringList(session.Get("attached_images")))}
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range imageMeta(imagePath) {
		out[k] = v
	}
	return out
}

// Saves the clipboard image into the session images dir and queues it
func clipboardPaste(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	counter := intOr(session.Get("image_counter")) + 1
	session.Set("image_counter", counter)
	imgDir := sessionImagesDir(ctx, session)
	if err := os.MkdirAll(imgDir, 0o777); err != nil {
		return rpcErr(rid, 5027, err.Error(), nil)
	}
	imgPath := filepath.Join(imgDir, fmt.Sprintf("clip_%s_%d.png", time.Now().Format("20060102_150405"), counter))
	if !SaveClipboardImage(imgPath) {
		session.Set("image_counter", max(0, intOr(session.Get("image_counter"))-1))
		message := "No image found in clipboard"
		if HasClipboardImage() {
			message = "Clipboard has image but extraction failed"
		}
		return ok(rid, map[string]any{"attached": false, "message": message})
	}
	session.Set("attached_images", append(stringList(session.Get("attached_images")), imgPath))
	return ok(rid, attachedImageResult(session, imgPath, nil))
}

// Attaches a host image named by a path, a dropped path or a quoted token
func imageAttach(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	raw := paramText(params, "path")
	if raw == "" {
		return rpcErr(rid, 4015, "path required", nil)
	}
	imagePath, remainder := "", ""
	if dropped := filedrop.DetectFileDrop(raw); dropped != nil {
		imagePath, remainder = dropped.Path, dropped.Remainder
	} else {
		var token string
		token, remainder = filedrop.SplitPathInput(raw)
		imagePath = filedrop.ResolveAttachmentPath(token)
		if imagePath == "" {
			return rpcErr(rid, 4016, "image not found: "+token, nil)
		}
	}
	if !filedrop.ImageExtensions[strings.ToLower(pypath.Suffix(imagePath))] {
		return rpcErr(rid, 4016, "unsupported image: "+pypath.Name(imagePath), nil)
	}
	session.Set("attached_images", append(stringList(session.Get("attached_images")), imagePath))
	text := remainder
	if text == "" {
		text = "[User attached image: " + pypath.Name(imagePath) + "]"
	}
	return ok(rid, attachedImageResult(session, imagePath, map[string]any{"remainder": remainder, "text": text}))
}

// Attaches an image sent as base64 bytes by a remote client
func imageAttachBytes(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	rawB64 := paramText(params, "content_base64")
	if rawB64 == "" {
		rawB64 = paramText(params, "data")
	}
	if rawB64 == "" {
		return rpcErr(rid, 4015, "content_base64 required", nil)
	}
	imgBytes, errResp := decodeAttachPayload(rid, rawB64, "image/", attachBytesMaxBytes, "image", "image is empty")
	if errResp != nil {
		return errResp
	}
	filename := ""
	if truthy(params["filename"]) {
		filename = strOr(params["filename"])
	}
	extHint := strings.ToLower(paramText(params, "ext"))
	if extHint != "" && !strings.HasPrefix(extHint, ".") {
		extHint = "." + extHint
	}
	hint := filename
	if hint == "" && extHint != "" {
		hint = "x" + extHint
	}
	ext := sniffImageExt(imgBytes, hint)
	if !allowedImageExtensions()[ext] {
		return rpcErr(rid, 4016, "unsupported image extension: "+ext, nil)
	}
	imgPath, err := queueAttachedImage(ctx, session, imgBytes, ext, "upload")
	if err != nil {
		return rpcErr(rid, 5027, "write failed: "+err.Error(), nil)
	}
	return ok(rid, attachedImageResult(session, imgPath, map[string]any{"remainder": "",
		"text": "[User attached image: " + pypath.Name(imgPath) + "]", "bytes": len(imgBytes)}))
}

// Removes one queued image by path
func imageDetach(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	raw := paramText(params, "path")
	if raw == "" {
		return rpcErr(rid, 4015, "path required", nil)
	}
	images := stringList(session.SetDefault("attached_images", []string{}))
	kept := []string{}
	for _, p := range images {
		if p != raw {
			kept = append(kept, p)
		}
	}
	session.Set("attached_images", kept)
	return ok(rid, map[string]any{"detached": len(kept) != len(images), "count": len(kept)})
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
		return ok(rid, map[string]any{"matched": false})
	}
	if dropped.IsImage {
		session.Set("attached_images", append(stringList(session.Get("attached_images")), dropped.Path))
		shown := dropped.Remainder
		if shown == "" {
			shown = "[User attached image: " + pypath.Name(dropped.Path) + "]"
		}
		result := map[string]any{"matched": true, "is_image": true, "path": dropped.Path,
			"count": len(stringList(session.Get("attached_images"))), "text": shown}
		for k, v := range imageMeta(dropped.Path) {
			result[k] = v
		}
		return ok(rid, result)
	}
	shown := "[User attached file: " + dropped.Path + "]"
	if dropped.Remainder != "" {
		shown += "\n" + dropped.Remainder
	}
	return ok(rid, map[string]any{"matched": true, "is_image": false, "path": dropped.Path, "name": pypath.Name(dropped.Path), "text": shown})
}
