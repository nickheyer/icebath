package tuigateway

type ImageAttachParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Path      string  `json:"path"`
}

// Image uploaded as base64 by a remote client, data an older name of content_base64
type ImageAttachBytesParams struct {
	SessionID     string  `json:"session_id"`
	Profile       *string `json:"profile,omitzero"`
	ContentBase64 string  `json:"content_base64,omitzero"`
	Data          string  `json:"data,omitzero"`
	Filename      string  `json:"filename,omitzero"`
	Ext           string  `json:"ext,omitzero"`
}

// Queued image with its size and token estimate, or attached false with the message why nothing was queued
type AttachedImageResult struct {
	Attached      bool    `json:"attached"`
	Name          string  `json:"name,omitzero"`
	Width         *int64  `json:"width,omitzero"`
	Height        *int64  `json:"height,omitzero"`
	TokenEstimate *int64  `json:"token_estimate,omitzero"`
	Path          string  `json:"path,omitzero"`
	Count         *int64  `json:"count,omitzero"`
	Remainder     *string `json:"remainder,omitzero"`
	Text          string  `json:"text,omitzero"`
	Bytes         *int64  `json:"bytes,omitzero"`
	Message       string  `json:"message,omitzero"`
}

// Image the session authority queued from uploaded bytes, with the sniffed mime type
type CanonicalAttachedImage struct {
	AttachedImageResult
	Mime string `json:"mime"`
}

type ImageDetachParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Path      string  `json:"path"`
}

type ImageDetachResult struct {
	Detached bool  `json:"detached"`
	Count    int64 `json:"count"`
}

type InputDetectDropParams struct {
	SessionID string  `json:"session_id"`
	Profile   *string `json:"profile,omitzero"`
	Text      string  `json:"text,omitzero"`
}

// A dropped path recognized in typed text, an image queued with its size or a file described in text
type InputDetectDropResult struct {
	Matched       bool   `json:"matched"`
	IsImage       bool   `json:"is_image,omitzero"`
	Name          string `json:"name,omitzero"`
	Width         *int64 `json:"width,omitzero"`
	Height        *int64 `json:"height,omitzero"`
	TokenEstimate *int64 `json:"token_estimate,omitzero"`
	Path          string `json:"path,omitzero"`
	Count         *int64 `json:"count,omitzero"`
	Text          string `json:"text,omitzero"`
}

type PasteCollapseParams struct {
	Text string `json:"text,omitzero"`
}

// Large paste spilled to a file, the placeholder the composer shows for it
type PasteCollapseResult struct {
	Placeholder string `json:"placeholder"`
	Path        string `json:"path"`
	Lines       int64  `json:"lines"`
}

// Saves the host clipboard image into the session and queues it for the next turn
var ClipboardPaste = NewMethod[SessionIDParams, AttachedImageResult]("clipboard.paste")

// Queues an image file the gateway can read for the next turn
var ImageAttach = NewMethod[ImageAttachParams, AttachedImageResult]("image.attach")

// Queues an image uploaded as base64 for the next turn
var ImageAttachBytes = NewMethod[ImageAttachBytesParams, AttachedImageResult]("image.attach_bytes")

// Queues an uploaded image on the session authority, answering its mime type too
var CanonicalImageAttachBytes = NewCanonicalMethod[ImageAttachBytesParams, CanonicalAttachedImage]("image.attach_bytes")

// Drops a queued image before the turn is sent
var ImageDetach = NewMethod[ImageDetachParams, ImageDetachResult]("image.detach")

// Turns a terminal file drop pasted into the composer into an attachment
var InputDetectDrop = NewMethod[InputDetectDropParams, InputDetectDropResult]("input.detect_drop")

// Spills a large paste to a file and answers the inline placeholder
var PasteCollapse = NewMethod[PasteCollapseParams, PasteCollapseResult]("paste.collapse")
