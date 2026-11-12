package tuiserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/filedrop"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
	"github.com/nickheyer/icebath/pkg/tuigateway"
)

// Clipboard image helpers of the classic CLI, set by the binary
var (
	SaveClipboardImage func(dest string) bool
	HasClipboardImage  func() bool
)

var promptAttachRegistry = &handlerRegistry{}

func init() {
	promptAttachRegistry.method("clipboard.paste", clipboardPaste)
	promptAttachRegistry.method("image.attach", imageAttach)
	promptAttachRegistry.method("image.attach_bytes", imageAttachBytes)
	promptAttachRegistry.method("pdf.attach", pdfAttach)
	promptAttachRegistry.method("file.attach", fileAttach)
	promptAttachRegistry.method("image.detach", imageDetach)
	promptAttachRegistry.method("input.detect_drop", inputDetectDrop)
	promptAttachRegistry.install()
}

// Attached reply after queuing an image with its count and image meta
func attachedImageResult(session *tuigateway.Session, imagePath string, extra map[string]any) map[string]any {
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

// PDF to render as its path and display name, or the error response
func pdfAttachSource(rid any, params map[string]any, tdPath, rawPath, rawB64 string) (string, string, map[string]any) {
	if rawB64 != "" {
		pdfBytes, errResp := decodeAttachPayload(rid, rawB64, "application/pdf", pdfAttachMaxBytes, "PDF", "decoded PDF is empty")
		if errResp != nil {
			return "", "", errResp
		}
		if !bytes.HasPrefix(pdfBytes, []byte("%PDF-")) {
			return "", "", rpcErr(rid, 4017, "payload is not a PDF (missing %PDF- magic bytes)", nil)
		}
		pdfPath := filepath.Join(tdPath, "input.pdf")
		if err := os.WriteFile(pdfPath, pdfBytes, 0o666); err != nil {
			panic(err)
		}
		name := "uploaded.pdf"
		if truthy(params["filename"]) {
			name = strOr(params["filename"])
		}
		return pdfPath, name, nil
	}
	resolved := filedrop.ResolveAttachmentPath(rawPath)
	info, statErr := os.Stat(resolved)
	if resolved == "" || statErr != nil || !info.Mode().IsRegular() {
		return "", "", rpcErr(rid, 4016, "PDF not found: "+rawPath, nil)
	}
	if strings.ToLower(pypath.Suffix(resolved)) != ".pdf" {
		return "", "", rpcErr(rid, 4016, "not a PDF: "+pypath.Name(resolved), nil)
	}
	if info.Size() > pdfAttachMaxBytes {
		return "", "", rpcErr(rid, 4018, fmt.Sprintf("PDF too large; cap is %d MB", pdfAttachMaxBytes/(1024*1024)), nil)
	}
	return resolved, pypath.Name(resolved), nil
}

// First and last page validated against the per call cap, or the error response
func pdfPageRange(rid any, params map[string]any) (int64, int64, map[string]any) {
	bad := rpcErr(rid, 4015, "first_page/last_page must be integers", nil)
	first := int64(1)
	if raw := params["first_page"]; truthy(raw) {
		n, err := pyint.Int64(raw)
		if err != nil {
			return 0, 0, bad
		}
		first = n
	}
	var last int64
	hasLast := params["last_page"] != nil
	if hasLast {
		n, err := pyint.Int64(params["last_page"])
		if err != nil {
			return 0, 0, bad
		}
		last = n
	}
	if first < 1 {
		return 0, 0, rpcErr(rid, 4015, "first_page must be >= 1", nil)
	}
	if !hasLast {
		last = first + pdfAttachMaxPages - 1
	}
	if last < first {
		return 0, 0, rpcErr(rid, 4015, "last_page must be >= first_page", nil)
	}
	if last-first+1 > pdfAttachMaxPages {
		return 0, 0, rpcErr(rid, 4019, fmt.Sprintf("page range exceeds cap of %d pages per attach call", pdfAttachMaxPages), nil)
	}
	return first, last, nil
}

// Renders each PDF page to PNG with pdftoppm and queues the pages as images
func pdfAttach(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	pdftoppm, lookErr := exec.LookPath("pdftoppm")
	if lookErr != nil {
		return rpcErr(rid, 5028, "pdftoppm not installed (poppler-utils package required)", nil)
	}
	rawPath, rawB64 := paramText(params, "path"), paramText(params, "content_base64")
	if rawB64 == "" {
		rawB64 = paramText(params, "data")
	}
	if rawPath == "" && rawB64 == "" {
		return rpcErr(rid, 4015, "path or content_base64 required", nil)
	}
	td, err := os.MkdirTemp("", "pdf_attach_")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(td)
	pdfPath, displayName, errResp := pdfAttachSource(rid, params, td, rawPath, rawB64)
	if errResp != nil {
		return errResp
	}
	first, last, errResp := pdfPageRange(rid, params)
	if errResp != nil {
		return errResp
	}
	runCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, pdftoppm, "-png", "-r", "150", "-f", strconv.FormatInt(first, 10), "-l", strconv.FormatInt(last, 10), pdfPath, filepath.Join(td, "page"))
	cmd.SysProcAttr = tools.WindowsHideAttr()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return rpcErr(rid, 5028, "pdftoppm timed out (>120s)", nil)
	}
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		panic(runErr)
	}
	if exitErr != nil {
		output := strings.ToValidUTF8(stderr.String(), "�")
		if pystr.Strip(output) == "" {
			output = strings.ToValidUTF8(stdout.String(), "�")
		}
		lines := pystr.Splitlines(pystr.Strip(output), false)
		tail := lines[max(0, len(lines)-3):]
		return rpcErr(rid, 5028, "pdftoppm failed: "+strings.Join(tail, " | "), nil)
	}
	rendered, _ := filepath.Glob(filepath.Join(td, "page-*.png"))
	sort.Strings(rendered)
	if len(rendered) == 0 {
		return rpcErr(rid, 5028, "pdftoppm produced no pages (corrupt PDF?)", nil)
	}
	var pages []any
	for _, src := range rendered {
		stem := strings.TrimSuffix(pypath.Name(src), pypath.Suffix(src))
		pageNum := stem
		if i := strings.Index(stem, "-"); i >= 0 {
			pageNum = stem[i+1:]
		}
		pageInt := first + int64(len(pages))
		if n, convErr := strconv.ParseInt(pystr.Strip(pageNum), 10, 64); convErr == nil {
			pageInt = n
		}
		data, readErr := os.ReadFile(src)
		if readErr != nil {
			panic(readErr)
		}
		dst, queueErr := queueAttachedImage(ctx, session, data, ".png", "pdf_p"+pageNum)
		if queueErr != nil {
			panic(queueErr)
		}
		page := map[string]any{"path": dst, "page": pageInt}
		for k, v := range imageMeta(dst) {
			page[k] = v
		}
		pages = append(pages, page)
	}
	return ok(rid, map[string]any{"attached": true, "filename": displayName, "pages_attached": len(pages), "pages": pages,
		"count": len(stringList(session.Get("attached_images"))),
		"text":  fmt.Sprintf("[User attached PDF: %s (%d page(s))]", displayName, len(pages))})
}

// Stages a non image file for the agent and returns its workspace relative file reference
func fileAttach(ctx context.Context, rid any, params map[string]any) map[string]any {
	session, errResp := sessBuilding(ctx, params, rid)
	if errResp != nil {
		return errResp
	}
	raw, dataURL, name := paramText(params, "path"), paramText(params, "data_url"), paramText(params, "name")
	if raw == "" && dataURL == "" {
		return rpcErr(rid, 4015, "path or data_url required", nil)
	}
	var resp map[string]any
	if failure := catchPanic(func() {
		stored, uploaded, err := stageSessionFileAttachment(ctx, session, raw, dataURL, name, nil, false)
		panicIf(err)
		refPath := attachmentRefPath(ctx, session, stored)
		resp = ok(rid, map[string]any{"attached": true, "name": pypath.Name(stored), "path": stored, "ref_path": refPath,
			"ref_text": "@file:" + formatRefValue(refPath), "uploaded": uploaded})
	}); failure != nil {
		return rpcErr(rid, 5028, errorMessage(failure), nil)
	}
	return resp
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
