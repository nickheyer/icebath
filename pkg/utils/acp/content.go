package acputil

import (
	"encoding/json"
	"errors"
	"fmt"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Content of a prompt, a message chunk or a tool call: *TextContentBlock, *ImageContentBlock, *AudioContentBlock, *ResourceContentBlock or *EmbeddedResourceContentBlock
type ContentBlock interface{ isContentBlock() }

func (*TextContentBlock) isContentBlock()             {}
func (*ImageContentBlock) isContentBlock()            {}
func (*AudioContentBlock) isContentBlock()            {}
func (*ResourceContentBlock) isContentBlock()         {}
func (*EmbeddedResourceContentBlock) isContentBlock() {}

// Contents of an embedded resource: *TextResourceContents or *BlobResourceContents
type ResourceContents interface{ isResourceContents() }

func (*TextResourceContents) isResourceContents() {}
func (*BlobResourceContents) isResourceContents() {}

// Audience, priority and modification time hints of a content block
type Annotations struct {
	Meta *jsonutil.Object `json:"_meta,omitempty"`
	// Roles the content is meant for, assistant or user
	Audience     []string `json:"audience,omitzero"`
	LastModified *string  `json:"lastModified,omitempty"`
	Priority     *float64 `json:"priority,omitempty"`
}

// Plain text
type TextContentBlock struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
	Text        string           `json:"text"`
}

// JSON object of the block with type "text"
func (b TextContentBlock) MarshalJSON() ([]byte, error) {
	type plain TextContentBlock
	return withTag(plain(b), "type", "text")
}

// Fills the block from its JSON object, which must carry text
func (b *TextContentBlock) UnmarshalJSON(data []byte) error {
	type plain TextContentBlock
	return decodeObject(data, (*plain)(b), "TextContentBlock", "text")
}

// Base64 image data with its MIME type
type ImageContentBlock struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
	Data        string           `json:"data"`
	MimeType    string           `json:"mimeType"`
	URI         *string          `json:"uri,omitempty"`
}

// JSON object of the block with type "image"
func (b ImageContentBlock) MarshalJSON() ([]byte, error) {
	type plain ImageContentBlock
	return withTag(plain(b), "type", "image")
}

// Fills the block from its JSON object, which must carry data and mimeType
func (b *ImageContentBlock) UnmarshalJSON(data []byte) error {
	type plain ImageContentBlock
	return decodeObject(data, (*plain)(b), "ImageContentBlock", "data", "mimeType")
}

// Base64 audio data with its MIME type
type AudioContentBlock struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
	Data        string           `json:"data"`
	MimeType    string           `json:"mimeType"`
}

// JSON object of the block with type "audio"
func (b AudioContentBlock) MarshalJSON() ([]byte, error) {
	type plain AudioContentBlock
	return withTag(plain(b), "type", "audio")
}

// Fills the block from its JSON object, which must carry data and mimeType
func (b *AudioContentBlock) UnmarshalJSON(data []byte) error {
	type plain AudioContentBlock
	return decodeObject(data, (*plain)(b), "AudioContentBlock", "data", "mimeType")
}

// Link to a resource the agent can read
type ResourceContentBlock struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
	Description *string          `json:"description,omitempty"`
	MimeType    *string          `json:"mimeType,omitempty"`
	Name        string           `json:"name"`
	// Size in bytes
	Size  *int    `json:"size,omitempty"`
	Title *string `json:"title,omitempty"`
	URI   string  `json:"uri"`
}

// JSON object of the block with type "resource_link"
func (b ResourceContentBlock) MarshalJSON() ([]byte, error) {
	type plain ResourceContentBlock
	return withTag(plain(b), "type", "resource_link")
}

// Fills the block from its JSON object, which must carry name and uri
func (b *ResourceContentBlock) UnmarshalJSON(data []byte) error {
	type plain ResourceContentBlock
	return decodeObject(data, (*plain)(b), "ResourceContentBlock", "name", "uri")
}

// Resource whose contents travel inside the message
type EmbeddedResourceContentBlock struct {
	Meta        *jsonutil.Object `json:"_meta,omitempty"`
	Annotations *Annotations     `json:"annotations,omitempty"`
	Resource    ResourceContents `json:"resource"`
}

// JSON object of the block with type "resource"
func (b EmbeddedResourceContentBlock) MarshalJSON() ([]byte, error) {
	type plain EmbeddedResourceContentBlock
	return withTag(plain(b), "type", "resource")
}

// Fills the block from its JSON object, which must carry resource, decoded by whether it holds text or blob
func (b *EmbeddedResourceContentBlock) UnmarshalJSON(data []byte) error {
	type plain EmbeddedResourceContentBlock
	var w struct {
		*plain
		Resource json.RawMessage `json:"resource"`
	}
	w.plain = (*plain)(b)
	if err := decodeObject(data, &w, "EmbeddedResourceContentBlock", "resource"); err != nil {
		return err
	}
	resource, err := decodeResourceContents(w.Resource)
	if err != nil {
		return fmt.Errorf("resource: %w", err)
	}
	b.Resource = resource
	return nil
}

// Text contents of a resource
type TextResourceContents struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	MimeType *string          `json:"mimeType,omitempty"`
	Text     string           `json:"text"`
	URI      string           `json:"uri"`
}

// Fills the contents from their JSON object, which must carry text and uri
func (r *TextResourceContents) UnmarshalJSON(data []byte) error {
	type plain TextResourceContents
	return decodeObject(data, (*plain)(r), "TextResourceContents", "text", "uri")
}

// Base64 binary contents of a resource
type BlobResourceContents struct {
	Meta     *jsonutil.Object `json:"_meta,omitempty"`
	Blob     string           `json:"blob"`
	MimeType *string          `json:"mimeType,omitempty"`
	URI      string           `json:"uri"`
}

// Fills the contents from their JSON object, which must carry blob and uri
func (r *BlobResourceContents) UnmarshalJSON(data []byte) error {
	type plain BlobResourceContents
	return decodeObject(data, (*plain)(r), "BlobResourceContents", "blob", "uri")
}

// Content block of a JSON object, the variant chosen by its type
func decodeContentBlock(data []byte) (ContentBlock, error) {
	kind, ok := tagOf(data, "type")
	if !ok {
		return nil, &MissingFieldError{Type: "ContentBlock", Field: "type"}
	}
	var block ContentBlock
	switch kind {
	case "text":
		block = new(TextContentBlock)
	case "image":
		block = new(ImageContentBlock)
	case "audio":
		block = new(AudioContentBlock)
	case "resource_link":
		block = new(ResourceContentBlock)
	case "resource":
		block = new(EmbeddedResourceContentBlock)
	default:
		return nil, &UnknownVariantError{Union: "ContentBlock", Key: "type", Value: kind}
	}
	if err := json.Unmarshal(data, block); err != nil {
		return nil, err
	}
	return block, nil
}

// Resource contents of a JSON object: text contents when it carries text, else blob contents
func decodeResourceContents(data []byte) (ResourceContents, error) {
	var contents ResourceContents
	switch {
	case hasMember(data, "text"):
		contents = new(TextResourceContents)
	case hasMember(data, "blob"):
		contents = new(BlobResourceContents)
	default:
		return nil, errors.New("resource contents carry neither text nor blob")
	}
	if err := json.Unmarshal(data, contents); err != nil {
		return nil, err
	}
	return contents, nil
}
