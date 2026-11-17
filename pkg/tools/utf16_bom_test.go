package tools

import "testing"

func TestDecodeUTF16BOMFollowsThePythonUTF16Codec(t *testing.T) {
	for _, c := range []struct {
		name string
		data []byte
		want string
		ok   bool
	}{
		{"little endian", []byte{0xff, 0xfe, 'H', 0, 'e', 0, 'r', 0, 'm', 0, 0xe9, 0, 's', 0}, "Herm\u00e9s", true},
		{"big endian", []byte{0xfe, 0xff, 0, 'H', 0, 0xe9, 0x4e, 0x2d}, "H\u00e9\u4e2d", true},
		{"bom alone", []byte{0xff, 0xfe}, "", true},
		{"second bom kept", []byte{0xff, 0xfe, 0xff, 0xfe, 'A', 0}, "\ufeffA", true},
		{"pair little endian", []byte{0xff, 0xfe, 0x3d, 0xd8, 0x00, 0xde, '!', 0}, "\U0001f600!", true},
		{"pair big endian", []byte{0xfe, 0xff, 0xd8, 0x3d, 0xde, 0x00}, "\U0001f600", true},
		{"no bom", []byte{'o', 0, 'k', 0}, "", false},
		{"utf8 text", []byte("ok"), "", false},
		{"empty", nil, "", false},
		{"odd trailing byte", []byte{0xff, 0xfe, '<', 0, '?'}, "", false},
		{"odd trailing byte big endian", []byte{0xfe, 0xff, 0, '<', 0}, "", false},
		{"lone low surrogate", []byte{0xff, 0xfe, 0x00, 0xdc, 'A', 0}, "", false},
		{"high surrogate then letter", []byte{0xff, 0xfe, 0x3d, 0xd8, 'A', 0}, "", false},
		{"high surrogate then high surrogate", []byte{0xfe, 0xff, 0xd8, 0x3d, 0xd8, 0x3d}, "", false},
		{"high surrogate at the end", []byte{0xff, 0xfe, 'A', 0, 0x3d, 0xd8}, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := decodeUTF16BOM(c.data)
			if got != c.want || ok != c.ok {
				t.Fatalf("got %q %v, want %q %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSchtasksOutputDecodesABOMAndFallsBackOnMalformedUTF16(t *testing.T) {
	noPages := func(int) codePageDecoder { return nil }
	if got, err := decodeSchtasksOutput([]byte{0xff, 0xfe, 'H', 0, 0xe9, 0}, nil, noPages); err != nil || got != "H\u00e9" {
		t.Fatalf("%q %v", got, err)
	}
	malformed := []byte{0xff, 0xfe, '<', 0, '?'}
	if got, err := decodeSchtasksOutput(malformed, nil, noPages); err != nil || got != decodeUTF8Replace(malformed) || got == "" {
		t.Fatalf("%q %v", got, err)
	}
}
