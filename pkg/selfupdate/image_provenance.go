package selfupdate

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

// Marker an image bakes outside the home and the checkout, a var so tests and other image builders repoint it
var ImageProvenancePath = "/etc/icebath/image-provenance.json"

const imageProvenanceSchema = 1

// Validated image provenance, or the description of an invalid marker that still means image managed
type ImageProvenance struct {
	Schema         int
	DeploymentKind string
	Manager        string
	Image          *string
	Version        *string
	Revision       *string
	MarkerPath     string
	Valid          bool
	Error          *string
}

// Invalid provenance naming why the marker at path could not be trusted
func invalidProvenance(path, reason string) *ImageProvenance {
	return &ImageProvenance{Schema: imageProvenanceSchema, DeploymentKind: "image", Manager: "unknown", MarkerPath: path, Valid: false, Error: &reason}
}

// Stripped text of an optional marker field, nil when absent or blank, not ok when it is not text
func optionalProvenanceString(payload *pyjson.Object, name string) (*string, bool) {
	value, _ := payload.Get(name)
	if value == nil {
		return nil, true
	}
	text, isStr := value.(string)
	if !isStr {
		return nil, false
	}
	if stripped := strings.TrimSpace(text); stripped != "" {
		return &stripped, true
	}
	return nil, true
}

// Baked image marker at markerPath or the default path, nil when absent, invalid provenance for any defect
func ReadImageProvenance(markerPath string) *ImageProvenance {
	path := ImageProvenancePath
	if markerPath != "" {
		path = markerPath
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && pypath.OSErrorClass(err) == "FileNotFoundError" {
			return nil
		}
		return invalidProvenance(path, "marker_presence_unreadable:"+pypath.OSErrorClass(err))
	}
	if !info.Mode().IsRegular() {
		return invalidProvenance(path, "marker_not_regular_file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return invalidProvenance(path, "marker_unreadable:"+pypath.OSErrorClass(err))
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return invalidProvenance(path, "marker_unreadable:UnicodeDecodeError")
	}
	doc, err := pyjson.Loads(string(raw))
	if err != nil {
		return invalidProvenance(path, "marker_unreadable:JSONDecodeError")
	}
	payload, isDict := doc.(*pyjson.Object)
	if !isDict || payload == nil {
		return invalidProvenance(path, "marker_not_object")
	}
	if schema, _ := payload.Get("schema"); schema != int64(imageProvenanceSchema) {
		return invalidProvenance(path, "unsupported_marker_schema")
	}
	if kind, _ := payload.Get("deployment_kind"); kind != "image" {
		return invalidProvenance(path, "invalid_deployment_kind")
	}
	rawManager, _ := payload.Get("manager")
	manager, isStr := rawManager.(string)
	if !isStr || strings.TrimSpace(manager) == "" {
		return invalidProvenance(path, "missing_manager")
	}
	out := &ImageProvenance{Schema: imageProvenanceSchema, DeploymentKind: "image", Manager: strings.TrimSpace(manager), MarkerPath: path, Valid: true}
	for name, field := range map[string]**string{"image": &out.Image, "version": &out.Version, "revision": &out.Revision} {
		value, ok := optionalProvenanceString(payload, name)
		if !ok {
			return invalidProvenance(path, "invalid_"+firstInvalidProvenanceField(payload))
		}
		*field = value
	}
	return out
}

// First optional marker field in reading order whose value is not text
func firstInvalidProvenanceField(payload *pyjson.Object) string {
	for _, name := range []string{"image", "version", "revision"} {
		if _, ok := optionalProvenanceString(payload, name); !ok {
			return name
		}
	}
	return ""
}
