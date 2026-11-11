package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"slices"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/state"
)

// Image attachment staging of the TUI server, assigned from pkg/tuiserver in cmd/icebath/wiring.go
var ImageAttachments struct {
	MaxBytes        int
	DecodeBase64    func(text, mimePrefix string, maxBytes int) ([]byte, error)
	SniffImageExt   func(data []byte, filename string) string
	StageImageBytes func(directory string, data []byte, ext, prefix string) (string, error)
}

// Whether an error carries one of the Python exception classes
func pyTypeNamed(err error, names ...string) bool {
	var fault *PyError
	if errors.As(err, &fault) && slices.Contains(names, fault.Kind) {
		return true
	}
	var typed interface{ PyTypeName() string }
	return errors.As(err, &typed) && slices.Contains(names, typed.PyTypeName())
}

// Refusal of a reason chained to the error that caused it
func refusalFrom(reason string, cause error) error {
	refusal := state.NewRuntimeStoreError(reason, nil, nil)
	refusal.Cause = cause
	return refusal
}

// Stages an uploaded image for one subscribed session in the owner profile's image cache
func attachBytes(ctx context.Context, c *AuthorityConnection, ref SessionRef, params *pyjson.Object) (any, error) {
	invalid := ref.SessionID == "" || paramsOutside(params, "session_id", "profile", "content_base64", "data", "filename", "ext")
	for _, key := range params.Keys {
		if _, isStr := params.GetOr(key, nil).(string); !isStr {
			invalid = true
		}
	}
	if invalid {
		return nil, runtimeRefusal("invalid_params")
	}
	c.mu.Lock()
	_, subscribed := c.subscriptions[ref.SessionID]
	c.mu.Unlock()
	if !subscribed {
		return nil, runtimeRefusal("permission_denied")
	}
	if err := authorizeBusy(ctx, c.authority, c.actor, ref, params, "session:submit"); err != nil {
		return nil, err
	}
	directory := filepath.Join(c.authority.ProfileID, "cache", "images")
	return stageImage(directory, params)
}

// Decodes, sniffs and writes one image, an unknown type refused as invalid_params
func stageImage(directory string, params *pyjson.Object) (*pyjson.Object, error) {
	text := func(key string) string { value, _ := params.GetOr(key, "").(string); return value }
	encoded := text("content_base64")
	if encoded == "" {
		encoded = text("data")
	}
	data, err := ImageAttachments.DecodeBase64(encoded, "image/", ImageAttachments.MaxBytes)
	if err != nil {
		if pyTypeNamed(err, "OverflowError") {
			return nil, refusalFrom("invalid_params", err)
		}
		return nil, err
	}
	if len(data) == 0 {
		return nil, runtimeRefusal("invalid_params")
	}
	hint := pystr.LstripChars(pystr.Lower(pystr.Strip(text("ext"))), ".")
	filename := text("filename")
	if filename == "" && hint != "" {
		filename = "x." + hint
	}
	ext := ImageAttachments.SniffImageExt(data, filename)
	mime, known := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
		".gif": "image/gif", ".webp": "image/webp"}[ext]
	if !known {
		return nil, runtimeRefusal("invalid_params")
	}
	path, err := ImageAttachments.StageImageBytes(directory, data, ext, "upload")
	if err != nil {
		if pypath.IsOSError(err) {
			return nil, refusalFrom("storage_unavailable", err)
		}
		return nil, err
	}
	name := filepath.Base(path)
	return pyjson.ObjectOf("attached", true, "path", path, "mime", mime, "name", name, "count", 1, "remainder", "",
		"text", "[User attached image: "+name+"]", "bytes", len(data)), nil
}
