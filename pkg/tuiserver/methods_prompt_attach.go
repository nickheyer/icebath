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
	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/tuihost"
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

// Attached reply after queuing an image with its count and image meta
func attachedImageResult(session *tuihost.Session, imagePath string) tuigateway.AttachedImageResult {
	count := int64(len(stringList(session.Get("attached_images"))))
	name, width, height, tokens := imageSize(imagePath)
	return tuigateway.AttachedImageResult{Attached: true, Path: imagePath, Count: &count, Name: name, Width: width, Height: height, TokenEstimate: tokens}
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

// Answer of an image method
func imageAnswer(rid any, result tuigateway.AttachedImageResult) map[string]any {
	return ok(rid, tuigateway.Plain(result))
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
		missing, noDesktop := ClipboardImageBlocker()
		switch {
		case missing != "":
			message = "pasting an image needs " + missing + " on this desktop; install it, or paste the file's path into the prompt"
		case noDesktop:
			message = "no desktop clipboard is reachable from this gateway; paste or drop the image file's path into the prompt"
		case HasClipboardImage():
			message = "the clipboard holds an image that could not be written as PNG; copy it again or paste the file's path into the prompt"
		}
		return imageAnswer(rid, tuigateway.AttachedImageResult{Message: message})
	}
	session.Set("attached_images", append(stringList(session.Get("attached_images")), imgPath))
	return imageAnswer(rid, attachedImageResult(session, imgPath))
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
	result := attachedImageResult(session, imagePath)
	result.Remainder, result.Text = &remainder, text
	return imageAnswer(rid, result)
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
	result := attachedImageResult(session, imgPath)
	empty, size := "", int64(len(imgBytes))
	result.Remainder, result.Text, result.Bytes = &empty, "[User attached image: "+pypath.Name(imgPath)+"]", &size
	return imageAnswer(rid, result)
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
	return ok(rid, tuigateway.Plain(tuigateway.ImageDetachResult{Detached: len(kept) != len(images), Count: int64(len(kept))}))
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
