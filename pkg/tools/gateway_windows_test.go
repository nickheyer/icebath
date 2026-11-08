package tools

import "testing"

func TestDecodeSchtasksBytesReadsUTF8AndUTF16(t *testing.T) {
	if got, err := DecodeSchtasksBytes([]byte("\"\\Icebath_Gateway\",\"N/A\",\"Ready\"\r\n")); err != nil || got != "\"\\Icebath_Gateway\",\"N/A\",\"Ready\"\r\n" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := DecodeSchtasksBytes([]byte{0xff, 0xfe, 'o', 0, 'k', 0}); err != nil || got != "ok" {
		t.Fatalf("%q %v", got, err)
	}
}
