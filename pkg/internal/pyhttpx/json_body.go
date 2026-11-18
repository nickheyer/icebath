package pyhttpx

import (
	"bytes"
	"encoding/json"
)

// Body bytes httpx writes for a json= request, compact and with every non ASCII character and markup character raw
func JSONBody(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return rawLineSeparators(bytes.TrimSuffix(b.Bytes(), []byte("\n"))), nil
}

// Turns the escaped U+2028 and U+2029 Go writes inside strings back into the raw characters Python writes
func rawLineSeparators(in []byte) []byte {
	if !bytes.Contains(in, []byte(`\u202`)) {
		return in
	}
	out := make([]byte, 0, len(in))
	inString := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		switch {
		case !inString:
			inString = c == '"'
		case c == '"':
			inString = false
		case c == '\\' && i+5 < len(in) && in[i+1] == 'u' && string(in[i+2:i+5]) == "202" && (in[i+5] == '8' || in[i+5] == '9'):
			out = append(out, " "[:2]...)
			out = append(out, 0xa8+(in[i+5]-'8'))
			i += 5
			continue
		case c == '\\':
			out = append(out, c, in[i+1])
			i++
			continue
		}
		out = append(out, c)
	}
	return out
}
