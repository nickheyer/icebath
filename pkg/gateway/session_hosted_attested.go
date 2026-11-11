package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

const (
	maxAttachmentBytes      = 15_000_000
	maxTaskAttachments      = 16
	maxTaskAttachmentBytes  = 50_000_000
	maxAttachmentNameChars  = 255
	maxAttachmentMimeChars  = 127
	maxDriverIdentifierChar = 128
)

var (
	attachmentIDRe       = regexp.MustCompile(`^att_[0-9a-f]{32}$`)
	attachmentMimeRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*/[A-Za-z0-9][A-Za-z0-9!#$&^_.+-]*$`)
	roomIdentifierRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	attestedSHA256Re     = regexp.MustCompile(`^[0-9a-f]{64}$`)
	attachmentKinds      = map[string]bool{"image": true, "pdf": true, "file": true}
	attachmentFieldNames = []string{"attachment_id", "kind", "mime", "name", "size"}
)

// A hosted room attachment contract failure, a ValueError
type AttachmentError struct{ Msg string }

func (e *AttachmentError) Error() string { return e.Msg }

// A driver identifier, clock, TTL or payload that is invalid, a ValueError
type DriverValidationError struct{ Msg string }

func (e *DriverValidationError) Error() string { return e.Msg }

// Stripped bounded room driver identifier
func driverIdentifier(value any, label string) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", &DriverValidationError{label + " must be a string"}
	}
	text = pystr.Strip(text)
	if text == "" || utf8.RuneCountInString(text) > maxDriverIdentifierChar || !roomIdentifierRe.MatchString(text) {
		return "", &DriverValidationError{"invalid " + label}
	}
	return text, nil
}

// One validated attachment manifest entry
func safeManifestEntry(value any) (map[string]any, error) {
	entry, ok := pyjson.Plain(value).(map[string]any)
	if !ok {
		return nil, &AttachmentError{"each attachment manifest entry must be an object"}
	}
	var unknown, missing []string
	for key := range entry {
		if !strings.Contains(" attachment_id kind mime name size ", " "+key+" ") {
			unknown = append(unknown, key)
		}
	}
	for _, key := range attachmentFieldNames {
		if _, has := entry[key]; !has {
			missing = append(missing, key)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		return nil, &AttachmentError{"attachment manifest has unknown fields: " + strings.Join(unknown, ", ")}
	}
	if len(missing) > 0 {
		return nil, &AttachmentError{"attachment manifest is missing fields: " + strings.Join(missing, ", ")}
	}
	size, isInt := pathIndex(entry["size"])
	if _, isBool := entry["size"].(bool); isBool || !isInt || size <= 0 || size > maxAttachmentBytes {
		return nil, &AttachmentError{"attachment size is outside the per-file limit"}
	}
	kind, ok := entry["kind"].(string)
	if !ok || !attachmentKinds[kind] {
		return nil, &AttachmentError{"attachment kind must be image, pdf, or file"}
	}
	rawMime, ok := entry["mime"].(string)
	if !ok {
		return nil, &AttachmentError{"attachment mime must be a string"}
	}
	mime := strings.ToLower(pystr.Strip(rawMime))
	if mime == "" || utf8.RuneCountInString(mime) > maxAttachmentMimeChars || !attachmentMimeRe.MatchString(mime) {
		return nil, &AttachmentError{"attachment mime is invalid"}
	}
	if kind == "image" && !strings.HasPrefix(mime, "image/") {
		return nil, &AttachmentError{"image attachments require an image MIME type"}
	}
	if kind == "pdf" && mime != "application/pdf" {
		return nil, &AttachmentError{"PDF attachments require application/pdf"}
	}
	id, ok := entry["attachment_id"].(string)
	if !ok || !attachmentIDRe.MatchString(id) {
		return nil, &AttachmentError{"invalid attachment_id"}
	}
	rawName, ok := entry["name"].(string)
	if !ok {
		return nil, &AttachmentError{"attachment name must be a string"}
	}
	name := pystr.Strip(rawName)
	if !utf8.ValidString(name) {
		return nil, &AttachmentError{"attachment name must contain valid Unicode"}
	}
	if name == "" || utf8.RuneCountInString(name) > maxAttachmentNameChars || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00\n\r") {
		return nil, &AttachmentError{"attachment name must be a bounded basename"}
	}
	return map[string]any{"attachment_id": id, "kind": kind, "name": name, "size": size, "mime": mime}, nil
}

// Bounded aggregate of several valid message manifests
func validateTaskManifest(value []any) ([]map[string]any, error) {
	if len(value) > maxTaskAttachments {
		return nil, &AttachmentError{fmt.Sprintf("task attachments must contain at most %d entries", maxTaskAttachments)}
	}
	normalized := make([]map[string]any, 0, len(value))
	ids, total := map[string]bool{}, 0
	for _, entry := range value {
		item, err := safeManifestEntry(entry)
		if err != nil {
			return nil, err
		}
		normalized = append(normalized, item)
		ids[item["attachment_id"].(string)] = true
		total += item["size"].(int)
	}
	if len(ids) != len(normalized) {
		return nil, &AttachmentError{"task attachment ids must be unique"}
	}
	if total > maxTaskAttachmentBytes {
		return nil, &AttachmentError{"task attachments exceed the aggregate byte limit"}
	}
	return normalized, nil
}

// Store metadata plus the exact source event of each bound task input
func validateBoundTaskManifest(value any) ([]map[string]any, error) {
	list, ok := pyjson.Plain(value).([]any)
	if !ok || len(list) == 0 {
		return nil, &DriverValidationError{"attachments must be a non-empty list"}
	}
	stripped := make([]any, len(list))
	events := make([]string, len(list))
	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, &DriverValidationError{"task attachment requires event_id"}
		}
		if _, has := fields["event_id"]; !has {
			return nil, &DriverValidationError{"task attachment requires event_id"}
		}
	}
	for i, item := range list {
		fields := item.(map[string]any)
		event, err := driverIdentifier(fields["event_id"], "attachment event_id")
		if err != nil {
			return nil, err
		}
		events[i] = event
		rest := map[string]any{}
		for k, v := range fields {
			if k != "event_id" {
				rest[k] = v
			}
		}
		stripped[i] = rest
	}
	manifest, err := validateTaskManifest(stripped)
	if err != nil {
		return nil, &DriverValidationError{err.Error()}
	}
	for i := range manifest {
		manifest[i]["event_id"] = events[i]
	}
	return manifest, nil
}

// One bound input and its content addressed retained reference
type attestedInput struct {
	item      map[string]any
	reference *pyjson.Object
}

// Manifest items with their retained references derived from source attested digests alone
func attestedInputs(ctx context.Context, attachments, digests any) ([]attestedInput, error) {
	manifest, err := validateBoundTaskManifest(attachments)
	if err != nil {
		return nil, err
	}
	list, ok := pyjson.Plain(digests).([]any)
	if !ok || len(list) != len(manifest) {
		return nil, runtimeRefusal("permission_denied")
	}
	for _, d := range list {
		if text, ok := d.(string); !ok || !attestedSHA256Re.MatchString(text) {
			return nil, runtimeRefusal("permission_denied")
		}
	}
	root, err := nativeInputsRoot(ctx)
	if err != nil {
		return nil, err
	}
	inputs := make([]attestedInput, len(manifest))
	for i, item := range manifest {
		digest := list[i].(string)
		name := item["name"].(string)
		if attachmentMimes[item["mime"].(string)] {
			name = digest + pathSuffix(name)
		}
		inputs[i] = attestedInput{item: item, reference: pyjson.ObjectOf("path", filepath.Join(root, digest, name), "sha256", digest, "size", item["size"])}
	}
	return inputs, nil
}

// Payload the committed submission carries for these inputs, derived from source attested digests alone
func attestedSubmissionPayload(ctx context.Context, prompt string, attachments, digests any) (*pyjson.Object, error) {
	if !pyTruthy(attachments) {
		return pyjson.ObjectOf("text", prompt), nil
	}
	inputs, err := attestedInputs(ctx, attachments, digests)
	if err != nil {
		return nil, err
	}
	var documents []string
	var media, mediaTypes []any
	for _, input := range inputs {
		mime := input.item["mime"].(string)
		if attachmentMimes[mime] {
			media = append(media, input.reference)
			mediaTypes = append(mediaTypes, mime)
		} else {
			path, _ := input.reference.Get("path")
			documents = append(documents, path.(string))
		}
	}
	text := prompt
	for _, path := range documents {
		text += HostedDocumentLine + path + "\n"
	}
	out := pyjson.ObjectOf("text", text)
	if len(media) > 0 {
		out.Set("attachments_v1", pyjson.ObjectOf("media", media, "media_types", mediaTypes))
	}
	return out, nil
}

// Refuses storage_unavailable unless every retained document still hashes to its source attested digest
func verifyAttestedDocuments(ctx context.Context, attachments, digests any) error {
	if !pyTruthy(attachments) {
		return nil
	}
	inputs, err := attestedInputs(ctx, attachments, digests)
	if err != nil {
		return err
	}
	var references []any
	for _, input := range inputs {
		if !attachmentMimes[input.item["mime"].(string)] {
			references = append(references, input.reference)
		}
	}
	_, err = restoreNativeMedia(ctx, references)
	return err
}
