package tools

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/nickheyer/icebath/internal/mcp"
	"github.com/nickheyer/icebath/internal/tools"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// Characters of one result kept, far above what reaches the model, so only floods lose text
	resultCap = 2_000_000
	// Bytes of one embedded payload saved, and images attached to the reply per call
	payloadCap    = 50 << 20
	imagesPerCall = 4
	// Bytes of one image attached for the model to see
	imageCap = 5 << 20
)

// Head and tail of a flood of text around a notice of what was cut
func truncate(text string) string {
	runes := []rune(text)
	if len(runes) <= resultCap {
		return text
	}
	half := resultCap / 2
	return string(runes[:half]) + fmt.Sprintf("\n\n[... MCP RESULT truncated: %d characters omitted ...]\n\n", len(runes)-2*half) + string(runes[len(runes)-half:])
}

// Whether a _meta key is one the protocol reserves: a label modelcontextprotocol or mcp followed by another
func reserved(key string) bool {
	prefix, _, found := strings.Cut(key, "/")
	if !found || prefix == "" {
		return false
	}
	labels := strings.Split(prefix, ".")
	return slices.ContainsFunc(labels[:len(labels)-1], func(label string) bool { return label == "modelcontextprotocol" || label == "mcp" })
}

// Files and images one call's result saved, and what reaches the model
type rendering struct {
	pool   *pool
	server string
	images []*sessionsv1.Media
	paths  []string
}

var nonBase64 = regexp.MustCompile(`[^A-Za-z0-9+/=]`)

// Bytes of a base64 payload, or the notice the model reads in their place
func (r *rendering) decode(data, label string) ([]byte, string) {
	if len(data) > payloadCap*4/3+4 {
		return nil, fmt.Sprintf("[MCP %s too large to cache: ~%d bytes]", label, len(data)*3/4)
	}
	raw, err := base64.StdEncoding.DecodeString(nonBase64.ReplaceAllString(data, ""))
	if err != nil {
		slog.Warn("MCP content decode failed", "server", r.server, "what", label, "error", err)
		return nil, ""
	}
	if len(raw) > payloadCap {
		return nil, fmt.Sprintf("[MCP %s too large to cache: %d bytes]", label, len(raw))
	}
	return raw, ""
}

// Saves bytes in the profile's MCP cache under a name of their content, its path
func (r *rendering) save(data []byte, name string) (string, error) {
	sum := sha256.Sum256(data)
	dir := filepath.Join(r.pool.dir(), "cache", "mcp")
	file := filepath.Join(dir, hex.EncodeToString(sum[:])[:16]+"-"+name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return file, os.WriteFile(file, data, 0o600)
}

// Extension of a MIME type, the fallback when the system knows none
func extension(mimeType, fallback string) string {
	base := mcp.BaseMime(mimeType)
	switch base {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "audio/wav", "audio/x-wav", "audio/wave":
		return ".wav"
	}
	if exts, err := mime.ExtensionsByType(base); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return fallback
}

// Image or audio of a block saved, its MEDIA tag; an image also attached for a model that sees images; empty when the block is not that
// media or could not be saved
func (r *rendering) media(block *mcp.ContentBlock, kind string, attach bool) string {
	base := mcp.BaseMime(block.MimeType)
	if block.Type != kind && block.Data == "" || !strings.HasPrefix(base, kind+"/") {
		return ""
	}
	raw, notice := r.decode(block.Data, kind+" block")
	if raw == nil {
		return notice
	}
	fallback := ".bin"
	if kind == "image" {
		fallback = ".png"
	}
	file, err := r.save(raw, kind+extension(base, fallback))
	if err != nil {
		slog.Warn("MCP content could not be saved", "server", r.server, "error", err)
		return ""
	}
	if kind == "image" {
		r.paths = append(r.paths, file)
		if attach && len(r.images) < imagesPerCall && len(raw) <= imageCap {
			r.images = append(r.images, &sessionsv1.Media{MimeType: base, Data: raw})
		}
	}
	return "MEDIA:" + file
}

// Name a resource is saved under: its URI's last part without control characters and at most 150 characters long, else resource and the
// extension of its type
func resourceName(uri, mimeType string) string {
	name := ""
	if u, err := url.Parse(uri); err == nil {
		if p, err := url.PathUnescape(u.EscapedPath()); err == nil {
			name = path.Base(strings.TrimRight(p, "/"))
		}
	}
	name = strings.TrimSpace(regexp.MustCompile(`[\x00-\x1f\x7f/\\]`).ReplaceAllString(name, ""))
	if runes := []rune(name); len(runes) > 150 {
		ext := filepath.Ext(name)
		if len([]rune(ext)) > 0 && len([]rune(ext)) <= 13 {
			stem := []rune(strings.TrimSuffix(name, ext))
			name = string(stem[:max(0, 150-len([]rune(ext)))]) + ext
		} else {
			name = string(runes[:150])
		}
	}
	if name == "" || name == "." || name == ".." {
		name = "resource" + extension(mimeType, ".bin")
	}
	return name
}

// Type, MIME type, URI, size and name of a block, for a notice about it
func details(block *mcp.ContentBlock, kind string) string {
	parts := []string{"type=" + kind}
	if block.MimeType != "" {
		parts = append(parts, "mimeType="+block.MimeType)
	}
	uri := block.URI
	if uri == "" && block.Resource != nil {
		uri = block.Resource.URI
	}
	if uri != "" {
		parts = append(parts, "uri="+uri)
	}
	if block.Size != nil {
		parts = append(parts, fmt.Sprintf("size=%d", *block.Size))
	}
	if block.Name != "" {
		parts = append(parts, "name="+block.Name)
	}
	return strings.Join(parts, ", ")
}

// A resource link as the text pointing at the tool that reads it, an embedded resource as its text or the file its bytes were saved to
func (r *rendering) resource(block *mcp.ContentBlock) string {
	if block.Type == "resource_link" || block.URI != "" && block.Resource == nil && block.Type != "text" {
		if block.URI == "" {
			return ""
		}
		detail := "uri=" + block.URI
		if block.Name != "" {
			detail += ", name=" + block.Name
		}
		if block.MimeType != "" {
			detail += ", mimeType=" + block.MimeType
		}
		return fmt.Sprintf("[MCP resource link: %s — fetch it with %s]", detail, toolName(r.server, "read_resource"))
	}
	res := block.Resource
	if res == nil {
		return ""
	}
	if res.Text != nil {
		return stripTags(*res.Text)
	}
	if res.Blob == nil {
		return ""
	}
	label := cmp.Or(res.MimeType, res.URI)
	raw, notice := r.decode(*res.Blob, "embedded resource")
	if raw == nil {
		if notice != "" {
			return strings.TrimSuffix(notice, "]") + ", uri=" + res.URI + "]"
		}
		return "[MCP embedded resource could not be decoded: " + label + "]"
	}
	file, err := r.save(raw, resourceName(res.URI, res.MimeType))
	if err != nil {
		slog.Warn("MCP embedded resource could not be saved", "server", r.server, "error", err)
		return "[MCP embedded resource could not be cached: " + label + "]"
	}
	return fmt.Sprintf("[MCP resource saved to %s (%s, %d bytes) — read it with read_file or terminal tools]", file, cmp.Or(res.MimeType, "unknown type"), len(raw))
}

// Text of the blocks, how many held something real, a notice in place of each block of a type not known
func (r *rendering) blocks(content []*mcp.ContentBlock, attach bool) (string, int) {
	var parts []string
	usable := 0
	for _, block := range content {
		if block == nil {
			continue
		}
		if block.Text != "" {
			parts = append(parts, stripTags(block.Text))
			if strings.TrimSpace(block.Text) != "" {
				usable++
			}
			continue
		}
		rendered := r.media(block, "image", attach)
		if rendered == "" {
			rendered = r.media(block, "audio", false)
		}
		if rendered == "" {
			rendered = r.resource(block)
		}
		if rendered != "" {
			parts = append(parts, rendered)
			usable++
			continue
		}
		kind := cmp.Or(block.Type, "ContentBlock")
		switch kind {
		case "text", "resource", "audio", "image", "resource_link":
			parts = append(parts, "[MCP content unreadable: the block could not be decoded or saved ("+details(block, kind)+")]")
		default:
			slog.Warn("MCP content block type unsupported, dropped", "server", r.server, "type", kind)
			parts = append(parts, "[MCP content dropped: unsupported block ("+details(block, kind)+")]")
		}
	}
	return truncate(strings.Join(parts, "\n")), usable
}

// Result a tool call hands the model: the text of its content, its structured content unless a text block already is it, and its _meta of
// keys the protocol does not reserve; an error result as the error
func (r *rendering) result(ctx context.Context, env *tools.Env, result *mcp.CallToolResult) string {
	if result.IsError {
		var b strings.Builder
		for _, block := range result.Content {
			switch {
			case block == nil:
			case block.Text != "":
				b.WriteString(block.Text)
			case block.Resource != nil && block.Resource.Text != nil:
				b.WriteString(*block.Resource.Text)
			}
		}
		return tools.ErrorResult(errors.New(mcp.Redact(truncate(cmp.Or(b.String(), "MCP tool returned an error")))))
	}
	attach := env != nil && env.Aux != nil && env.Aux.NativeImages(ctx)
	text, usable := r.blocks(result.Content, attach)
	if len(r.images) > 0 {
		text += "\n\nThe image(s) from this call are attached — inspect them with your native vision."
	}
	structured := result.StructuredContent
	if structured != nil {
		if encoded, err := json.Marshal(structured); err == nil && len([]rune(string(encoded))) > resultCap {
			structured = truncate(string(encoded))
		}
	}
	if _, isText := structured.(string); structured != nil && usable > 0 && (isText || dualEmitted(result.Content, structured)) {
		structured = nil
	}
	meta := map[string]any{}
	for k, v := range result.Meta {
		if !reserved(k) {
			meta[k] = v
		}
	}
	if structured == nil && len(meta) == 0 {
		return tools.JSON(map[string]any{"result": text})
	}
	payload := map[string]any{}
	switch {
	case structured == nil:
		payload["result"] = text
	case text != "":
		payload["result"], payload["structuredContent"] = text, structured
	default:
		payload["result"] = structured
	}
	if len(meta) > 0 {
		payload["_meta"] = meta
	}
	if _, err := json.Marshal(payload); err != nil {
		return tools.JSON(map[string]any{"result": text})
	}
	return tools.JSON(payload)
}

// Whether some text block is the structured content written as JSON
func dualEmitted(content []*mcp.ContentBlock, structured any) bool {
	want, err := json.Marshal(structured)
	if err != nil {
		return false
	}
	var normalized any
	json.Unmarshal(want, &normalized)
	for _, block := range content {
		if block == nil || block.Text == "" {
			continue
		}
		var parsed any
		if json.Unmarshal([]byte(block.Text), &parsed) == nil && reflect.DeepEqual(parsed, normalized) {
			return true
		}
	}
	return false
}

// Text a prompt message's content shows the model: text as it is, media saved as MEDIA tags, resources as tool results show them, a notice
// for a block of no known use
func (r *rendering) promptContent(raw json.RawMessage) (string, error) {
	var block mcp.ContentBlock
	if err := json.Unmarshal(raw, &block); err != nil || !strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		return "", errors.New("Input should be a content block object")
	}
	switch block.Type {
	case "text":
		return block.Text, nil
	case "image", "audio":
		if tag := r.media(&block, block.Type, false); tag != "" {
			return tag, nil
		}
	case "resource", "resource_link":
		if text := r.resource(&block); text != "" || block.Resource != nil && block.Resource.Text != nil {
			return text, nil
		}
	default:
		kind := cmp.Or(block.Type, "ContentBlock")
		slog.Warn("MCP prompt content block type unsupported", "server", r.server, "type", kind)
		return "[MCP content dropped: unsupported block (" + details(&block, kind) + ")]", nil
	}
	return "[MCP content unreadable: the block could not be decoded or saved (" + details(&block, block.Type) + ")]", nil
}
