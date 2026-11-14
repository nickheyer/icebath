package wire

import (
	"bytes"
	"encoding/json"
)

// What a content part carries
type PartKind uint8

const (
	PartText PartKind = iota + 1
	PartImage
	PartAudio
	PartVideo
	PartNative
)

// One element of list content, Native holding any part the other fields do not describe
type Part struct {
	Kind      PartKind
	TypeName  string
	Text      string
	Citations json.RawMessage
	URL       string
	Path      string
	Detail    string
	Audio     string
	Format    string
	Native    json.RawMessage
	Cache     *CacheMarker

	src *partSource
}

// Decoded form a part came from, where its media reference sat and the field values it held then
type partSource struct {
	shape     *shape
	sub       *shape
	snap      *Part
	whole     json.RawMessage
	layout    layout
	bare      bool
	textKey   string
	media     string
	flat      bool
	subDetail bool
	subPath   bool
}

// Kind a part type word selects
func partKind(typeName string) PartKind {
	switch typeName {
	case "text", "input_text", "output_text":
		return PartText
	case "image_url", "input_image", "image":
		return PartImage
	case "input_audio", "audio":
		return PartAudio
	case "video_url", "video", "input_video":
		return PartVideo
	}
	return PartNative
}

// Refuses the standard encoder so the byte-exact writers are the only way to encode a part
func (p Part) MarshalJSON() ([]byte, error) {
	return nil, ErrInexactEncoder
}

// Stored text of the part, untouched since decoding coming back byte for byte
func (p *Part) JSON() json.RawMessage {
	return p.appendJSON(nil, 0)
}

// Reads a content part of any shape, a bare string becoming a text part
func (p *Part) UnmarshalJSON(data []byte) error {
	data = trimJSON(data)
	if string(data) == "null" {
		return nil
	}
	if !json.Valid(data) {
		return ErrInvalidJSON
	}
	p.decode(data)
	return nil
}

// Appends the part in its own layout, or the ambient one for a part built in code
func (p *Part) appendJSON(dst []byte, ambient layout) []byte {
	if len(p.Native) > 0 {
		return p.appendNative(dst)
	}
	if p.Kind == PartNative {
		return append(dst, "null"...)
	}
	var sh *shape
	if src := p.src; src != nil {
		if src.bare && p.Kind == PartText && p.TypeName == "" && p.Cache == nil && len(p.Citations) == 0 {
			if src.whole != nil && p.Text == src.snap.Text {
				return append(dst, src.whole...)
			}
			return appendString(dst, p.Text, src.layout)
		}
		sh = src.shape
	}
	return sh.render(dst, p.fields(), ambient)
}

// Appends the verbatim part with its cache_control member brought in line with Cache
func (p *Part) appendNative(dst []byte) []byte {
	if p.Cache == nil && !bytes.Contains(p.Native, []byte(`"cache_control"`)) {
		return append(dst, p.Native...)
	}
	data := trimJSON(p.Native)
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return append(dst, p.Native...)
	}
	existing, _ := objectMember(data, "cache_control")
	if sameCache(parseCache(existing), p.Cache) {
		return append(dst, p.Native...)
	}
	sh, _ := readShape(data)
	sh.whole = nil
	var none *CacheMarker
	return sh.render(dst, withoutSame([]field{cacheField(&p.Cache, &none)}), sh.layout)
}

// Fills the part from one valid JSON value
func (p *Part) decode(data []byte) {
	*p = Part{}
	if data[0] == '"' {
		p.Kind, p.Text = PartText, decodeString(data)
		src := &partSource{bare: true, layout: detectLayout(data, false)}
		if !bytes.Equal(appendString(nil, p.Text, src.layout), data) {
			snap := *p
			src.whole, src.snap = bytes.Clone(data), &snap
		}
		p.src = src
		return
	}
	if data[0] != '{' {
		p.Kind, p.Native = PartNative, bytes.Clone(data)
		return
	}
	sh, _ := readShape(data)
	src := &partSource{shape: sh}
	if raw, ok := sh.last("type"); ok {
		p.TypeName, _ = stringValue(raw)
	}
	if raw, ok := sh.last("cache_control"); ok {
		p.Cache = parseCache(raw)
	}
	p.Kind = partKind(p.TypeName)
	described := false
	switch p.Kind {
	case PartText:
		described = true
		src.textKey = "text"
		raw, _ := sh.last("text")
		if s, ok := stringValue(raw); ok {
			p.Text = s
		} else if raw, _ := sh.last("content"); len(raw) > 0 && raw[0] == '"' {
			p.Text, src.textKey = decodeString(raw), "content"
		}
		if raw, ok := sh.last("citations"); ok && string(raw) != "null" {
			p.Citations = bytes.Clone(raw)
		}
	case PartImage:
		described = p.decodeMedia(src, "image_url", "url", "image")
	case PartVideo:
		described = p.decodeMedia(src, "video_url", "url", "video")
	case PartAudio:
		if raw, ok := sh.last("input_audio"); ok && raw[0] == '{' {
			sub, _ := readShape(raw)
			if payload, ok := sub.last("data"); ok && payload[0] == '"' {
				described, src.sub, src.media = true, sub, "input_audio"
				p.Audio = decodeString(payload)
				if format, ok := sub.last("format"); ok {
					p.Format, _ = stringValue(format)
				}
			}
		}
	}
	if !described {
		*p = Part{Kind: p.Kind, TypeName: p.TypeName, Cache: p.Cache, Native: bytes.Clone(data)}
		return
	}
	snap := *p
	if snap.Cache != nil {
		c := *snap.Cache
		snap.Cache = &c
	}
	src.snap = &snap
	p.src = src
	keptSub := src.sub != nil && src.sub.settle(p.subFields())
	if !sh.settle(p.fields()) && !keptSub {
		src.snap = nil
	}
}

// Reads the media reference of an image or video part, false when no member holds a URL or path
func (p *Part) decodeMedia(src *partSource, keys ...string) bool {
	sh := src.shape
	for _, key := range keys {
		raw, ok := sh.last(key)
		if !ok {
			continue
		}
		if s, ok := stringValue(raw); ok && s != "" {
			p.URL, src.media, src.flat = s, key, true
			break
		}
		if raw[0] != '{' {
			continue
		}
		sub, _ := readShape(raw)
		url, _ := sub.last("url")
		path, _ := sub.last("path")
		u, _ := stringValue(url)
		pth, _ := stringValue(path)
		if u == "" && pth == "" {
			continue
		}
		p.URL, src.media, src.sub = u, key, sub
		if pth != "" {
			p.Path, src.subPath = pth, true
		}
		if detail, ok := sub.last("detail"); ok && p.Kind == PartImage {
			p.Detail, _ = stringValue(detail)
			src.subDetail = true
		}
		break
	}
	if !src.subPath {
		if raw, ok := sh.last("path"); ok {
			p.Path, _ = stringValue(raw)
		}
	}
	if !src.subDetail && p.Kind == PartImage {
		raw, ok := sh.last("detail")
		if ok {
			p.Detail, _ = stringValue(raw)
		}
		src.subDetail = !ok && src.sub != nil
	}
	return p.URL != "" || p.Path != ""
}

// Last value a key holds among the members
func (sh *shape) last(key string) (json.RawMessage, bool) {
	for i := len(sh.members) - 1; i >= 0; i-- {
		if sh.members[i].key == key {
			return sh.members[i].raw, true
		}
	}
	return nil, false
}

// Field values the part held when it was read, an empty part when it was built in code
func (p *Part) decoded() (old *Part, fresh bool) {
	if p.src != nil && p.src.snap != nil {
		return p.src.snap, false
	}
	return &Part{}, true
}

// Member and form the media reference is written in
func (p *Part) mediaForm() (key string, flat, subDetail, subPath bool) {
	if src := p.src; src != nil && !src.bare {
		return src.media, src.flat, src.subDetail, src.subPath
	}
	switch {
	case p.Kind == PartVideo:
		return "video_url", false, false, false
	case p.TypeName == "input_image":
		return "image_url", true, false, false
	case p.TypeName == "image":
		return "url", true, false, false
	}
	return "image_url", false, true, false
}

// Field table of the object form in the order a part built in code is written
func (p *Part) fields() []field {
	old, fresh := p.decoded()
	typ := textField("type", &p.TypeName, &old.TypeName)
	fields := []field{typ}
	switch p.Kind {
	case PartText, 0:
		fields[0].zero = `"text"`
		key := "text"
		if p.src != nil && p.src.textKey != "" {
			key = p.src.textKey
		}
		text := textField(key, &p.Text, &old.Text)
		text.zero = `""`
		fields = append(fields, text, rawField("citations", &p.Citations, &old.Citations))
	case PartImage, PartVideo:
		fields[0].zero = `"image_url"`
		if p.Kind == PartVideo {
			fields[0].zero = `"video_url"`
		}
		key, flat, subDetail, subPath := p.mediaForm()
		switch {
		case key == "":
		case flat:
			fields = append(fields, textField(key, &p.URL, &old.URL))
		default:
			fields = append(fields, field{
				key: key,
				enc: func(dst []byte, l layout) ([]byte, bool) {
					if p.src == nil && p.URL == "" && p.Path != "" {
						return dst, false
					}
					return p.subShape().render(dst, p.subFields(), l), true
				},
				same: func() bool {
					return p.URL == old.URL && (!subDetail || p.Detail == old.Detail) && (!subPath || p.Path == old.Path)
				},
			})
		}
		if !subDetail && p.Kind == PartImage {
			fields = append(fields, textField("detail", &p.Detail, &old.Detail))
		}
		if !subPath {
			fields = append(fields, textField("path", &p.Path, &old.Path))
		}
	case PartAudio:
		fields[0].zero = `"input_audio"`
		fields = append(fields, field{
			key: "input_audio",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				return p.subShape().render(dst, p.subFields(), l), true
			},
			same: func() bool { return p.Audio == old.Audio && p.Format == old.Format },
		})
	}
	fields = append(fields, cacheField(&p.Cache, &old.Cache))
	if fresh {
		return withoutSame(fields)
	}
	return fields
}

// Decoded nested media object, nil when the part has none
func (p *Part) subShape() *shape {
	if p.src == nil {
		return nil
	}
	return p.src.sub
}

// Field table of the nested media object
func (p *Part) subFields() []field {
	old, fresh := p.decoded()
	var fields []field
	if p.Kind == PartAudio {
		data := textField("data", &p.Audio, &old.Audio)
		data.zero = `""`
		fields = []field{data, textField("format", &p.Format, &old.Format)}
	} else {
		_, _, subDetail, subPath := p.mediaForm()
		url := textField("url", &p.URL, &old.URL)
		if !subPath {
			url.zero = `""`
		}
		fields = []field{url}
		if subDetail {
			fields = append(fields, textField("detail", &p.Detail, &old.Detail))
		}
		if subPath {
			fields = append(fields, textField("path", &p.Path, &old.Path))
		}
	}
	if fresh {
		return withoutSame(fields)
	}
	return fields
}

// Reports whether two parts would be written identically
func (p *Part) equal(o *Part) bool {
	return p.src == o.src && p.Kind == o.Kind && p.TypeName == o.TypeName && p.Text == o.Text &&
		p.URL == o.URL && p.Path == o.Path && p.Detail == o.Detail && p.Audio == o.Audio && p.Format == o.Format &&
		bytes.Equal(p.Citations, o.Citations) && bytes.Equal(p.Native, o.Native) && sameCache(p.Cache, o.Cache)
}
