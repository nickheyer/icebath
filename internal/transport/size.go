package transport

import (
	"encoding/json"
	"slices"
	"unicode/utf8"
)

// Tokens a picture in a request is counted as
const imageTokens = 1500

// Part types that carry a picture
var imageParts = []string{"image", "image_url", "input_image"}

// Rough tokens of the context a request body holds, about four characters a token: its messages and tools, a Responses body's input,
// instructions and tools, else all of it; each picture at a flat price
func Tokens(body map[string]any) int {
	if messages, ok := body["messages"]; ok {
		return (chars(messages) + chars(body["tools"])) / 4
	}
	if input, ok := body["input"]; ok {
		return (chars(input) + chars(body["instructions"]) + chars(body["tools"])) / 4
	}
	total := 0
	for _, v := range body {
		total += chars(v)
	}
	return total / 4
}

// Characters of a value as the wire holds it, a picture part at the price of its tokens
func chars(v any) int {
	switch c := v.(type) {
	case nil:
		return 0
	case string:
		return utf8.RuneCountInString(c)
	case map[string]any:
		if kind, ok := c["type"].(string); ok && slices.Contains(imageParts, kind) {
			for _, key := range []string{"image_url", "image", "source", "file_id"} {
				if _, has := c[key]; has {
					text, _ := c["text"].(string)
					return imageTokens*4 + utf8.RuneCountInString(text)
				}
			}
		}
		sum := 0
		for k, value := range c {
			sum += utf8.RuneCountInString(k) + 6 + chars(value)
		}
		return sum
	case []any:
		sum := 2 * len(c)
		for _, item := range c {
			sum += chars(item)
		}
		return sum
	case []map[string]any:
		sum := 2 * len(c)
		for _, item := range c {
			sum += chars(item)
		}
		return sum
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	var decoded any
	if json.Unmarshal(raw, &decoded) != nil {
		return len(raw)
	}
	switch d := decoded.(type) {
	case nil:
		return 0
	case string:
		return utf8.RuneCountInString(d)
	case map[string]any, []any:
		return chars(d)
	}
	// A number or true/false, as the wire writes it
	return len(raw)
}
