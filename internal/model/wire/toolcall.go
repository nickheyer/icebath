package wire

import (
	"bytes"
	"encoding/json"
)

// One model-issued function call, Arguments holding the JSON text as produced
type ToolCall struct {
	ID                string
	CallID            string
	ResponseItemID    string
	Name              string
	Arguments         string
	ExtraContent      json.RawMessage
	FunctionSignature string
	Repaired          bool

	src *toolCallSource
}

// Decoded dictionary a tool call came from, its function object and the field values it held then
type toolCallSource struct {
	shape *shape
	fn    *shape
	snap  *ToolCall
}

// Refuses the standard encoder so the byte-exact writers are the only way to encode a tool call
func (c ToolCall) MarshalJSON() ([]byte, error) {
	return nil, ErrInexactEncoder
}

// Stored tool call dictionary as text, members untouched since decoding coming back byte for byte
func (c *ToolCall) JSON() json.RawMessage {
	return c.appendJSON(nil, 0)
}

// Reads a stored tool call dictionary with its member order and unknown members
func (c *ToolCall) UnmarshalJSON(data []byte) error {
	data = trimJSON(data)
	if string(data) == "null" {
		return nil
	}
	if len(data) == 0 || data[0] != '{' || !json.Valid(data) {
		return ErrInvalidJSON
	}
	*c = ToolCall{}
	c.decode(data)
	return nil
}

// Appends the dictionary in its own layout, or the ambient one for a call built in code
func (c *ToolCall) appendJSON(dst []byte, ambient layout) []byte {
	var sh *shape
	if c.src != nil {
		sh = c.src.shape
	}
	return sh.render(dst, c.fields(), ambient)
}

// Fills the fields from a valid object and settles which member values must be kept verbatim
func (c *ToolCall) decode(data []byte) {
	sh, _ := readShape(data)
	var fnRaw []byte
	for _, mb := range sh.members {
		switch mb.key {
		case "id":
			c.ID, _ = stringValue(mb.raw)
		case "call_id":
			c.CallID, _ = stringValue(mb.raw)
		case "response_item_id":
			c.ResponseItemID, _ = stringValue(mb.raw)
		case "function":
			fnRaw = mb.raw
		case "extra_content":
			c.ExtraContent = nil
			if string(mb.raw) != "null" {
				c.ExtraContent = bytes.Clone(mb.raw)
			}
		}
	}
	var fn *shape
	if len(fnRaw) > 0 && fnRaw[0] == '{' {
		fn, _ = readShape(fnRaw)
		for _, mb := range fn.members {
			switch mb.key {
			case "name":
				c.Name, _ = stringValue(mb.raw)
			case "arguments":
				if s, ok := stringValue(mb.raw); ok {
					c.Arguments = s
				} else if string(mb.raw) != "null" {
					c.Arguments = string(mb.raw)
				}
			case "thought_signature":
				c.FunctionSignature, _ = stringValue(mb.raw)
			}
		}
	}
	snap := *c
	c.src = &toolCallSource{shape: sh, fn: fn, snap: &snap}
	keptFn := fn != nil && fn.settle(c.functionFields())
	if !sh.settle(c.fields()) && !keptFn {
		c.src.snap = nil
	}
}

// Field values the call held when it was read, an empty call when it was built in code
func (c *ToolCall) decoded() (old *ToolCall, fresh bool) {
	if c.src != nil && c.src.snap != nil {
		return c.src.snap, false
	}
	return &ToolCall{}, true
}

// Field table of the call in the order a call built in code is written
func (c *ToolCall) fields() []field {
	old, fresh := c.decoded()
	id := textField("id", &c.ID, &old.ID)
	id.zero = `""`
	fields := []field{
		id,
		textField("call_id", &c.CallID, &old.CallID),
		textField("response_item_id", &c.ResponseItemID, &old.ResponseItemID),
		{
			key:  "type",
			zero: `"function"`,
			enc:  func(dst []byte, l layout) ([]byte, bool) { return dst, false },
			same: func() bool { return true },
		},
		{
			key: "function",
			enc: func(dst []byte, l layout) ([]byte, bool) {
				var fn *shape
				if c.src != nil {
					fn = c.src.fn
				}
				if c.src != nil && fn == nil && c.Name == "" && c.Arguments == "" && c.FunctionSignature == "" {
					return dst, false
				}
				return fn.render(dst, c.functionFields(), l), true
			},
			same: func() bool {
				return c.Name == old.Name && c.Arguments == old.Arguments && c.FunctionSignature == old.FunctionSignature
			},
		},
		rawField("extra_content", &c.ExtraContent, &old.ExtraContent),
	}
	if fresh {
		return withoutSame(fields)
	}
	return fields
}

// Field table of the function object inside the call
func (c *ToolCall) functionFields() []field {
	old, fresh := c.decoded()
	name := textField("name", &c.Name, &old.Name)
	name.zero = `""`
	args := textField("arguments", &c.Arguments, &old.Arguments)
	args.zero = `""`
	fields := []field{name, args, textField("thought_signature", &c.FunctionSignature, &old.FunctionSignature)}
	if fresh {
		return withoutSame(fields)
	}
	return fields
}

// Reports whether two calls would be written identically
func (c *ToolCall) equal(o *ToolCall) bool {
	return c.src == o.src && c.ID == o.ID && c.CallID == o.CallID && c.ResponseItemID == o.ResponseItemID &&
		c.Name == o.Name && c.Arguments == o.Arguments && c.FunctionSignature == o.FunctionSignature &&
		bytes.Equal(c.ExtraContent, o.ExtraContent)
}
