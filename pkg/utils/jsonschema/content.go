package jsonschemautil

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"mime"
	"strings"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// contentEncoding, contentMediaType and contentSchema of a schema as one check: the string decodes, the decoded bytes parse as the media type, and the parsed document passes contentSchema; nil when nothing it names is checkable
func (c *compiler) content(l loc, o *jsonutil.Object) (checkFn, error) {
	encoding, _ := o.Value("contentEncoding").(string)
	mediaType, _ := o.Value("contentMediaType").(string)
	decode := contentDecoder(encoding)
	isJSON := jsonMediaType(mediaType)
	var schema *node
	if sv, ok := o.Get("contentSchema"); ok && isJSON && l.dialect.draft >= Draft201909 {
		n, err := c.sub(sv, l, "/contentSchema")
		if err != nil {
			return nil, err
		}
		schema = n
	}
	if decode == nil && !isJSON {
		return nil, nil
	}
	encodingText, mediaText := quote(encoding, schemaQuoteLimit), quote(mediaType, schemaQuoteLimit)
	return func(st *state, v any, _ *evaluated) bool {
		s, ok := v.(string)
		if !ok {
			return true
		}
		data := []byte(s)
		if decode != nil {
			out, err := decode(s)
			if err != nil {
				if !st.quick {
					old := st.rename("contentEncoding")
					st.fail("contentEncoding", quoteValue(v)+" is not valid "+encodingText+" content: "+err.Error(), v, nil)
					st.rename(old)
				}
				return false
			}
			data = out
		}
		if !isJSON {
			return true
		}
		doc, err := jsonutil.Decode(data)
		if err != nil {
			if !st.quick {
				old := st.rename("contentMediaType")
				st.fail("contentMediaType", quoteValue(v)+" is not valid "+mediaText+" content: "+err.Error(), v, nil)
				st.rename(old)
			}
			return false
		}
		if schema == nil {
			return true
		}
		old := st.rename("contentSchema")
		ok = st.derived(schema, doc)
		st.rename(old)
		return ok
	}, nil
}

// Decoder of a content encoding, nil for one the package does not decode
func contentDecoder(name string) func(string) ([]byte, error) {
	switch strings.ToLower(name) {
	case "base64":
		return base64.StdEncoding.DecodeString
	case "base64url":
		return func(s string) ([]byte, error) {
			if strings.HasSuffix(s, "=") {
				return base64.URLEncoding.DecodeString(s)
			}
			return base64.RawURLEncoding.DecodeString(s)
		}
	case "base32":
		return base32.StdEncoding.DecodeString
	case "base32hex":
		return base32.HexEncoding.DecodeString
	case "base16":
		return hex.DecodeString
	}
	return nil
}

// Whether a media type is JSON: application/json or any type with a +json suffix
func jsonMediaType(mediaType string) bool {
	if mediaType == "" {
		return false
	}
	t, _, err := mime.ParseMediaType(mediaType)
	if err != nil {
		return false
	}
	return t == "application/json" || strings.HasSuffix(t, "+json")
}
