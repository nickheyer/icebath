package imaging

import (
	"bytes"
	"encoding/binary"
	"strings"
)

// Base64 characters of an embedded image decoded when only its header is read
const headerProbeChars = 32 * 1024

// JPEG start of frame markers that carry the frame dimensions
var jpegSOFMarkers = [256]bool{0xC0: true, 0xC1: true, 0xC2: true, 0xC3: true, 0xC5: true, 0xC6: true, 0xC7: true,
	0xC9: true, 0xCA: true, 0xCB: true, 0xCD: true, 0xCE: true, 0xCF: true}

// Container and dimensions read from a PNG, JPEG, GIF or WebP header without decoding pixels
func HeaderSize(raw []byte) (Format, int, int, bool) {
	if bytes.HasPrefix(raw, pngSignature) && len(raw) >= 24 {
		return PNG, int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24])), true
	}
	if bytes.HasPrefix(raw, []byte{0xff, 0xd8}) && len(raw) > 4 {
		w, h, ok := jpegHeaderSize(raw)
		return JPEG, w, h, ok
	}
	if len(raw) >= 10 && (string(raw[:6]) == "GIF87a" || string(raw[:6]) == "GIF89a") {
		return GIF, int(binary.LittleEndian.Uint16(raw[6:8])), int(binary.LittleEndian.Uint16(raw[8:10])), true
	}
	if len(raw) >= 16 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP" {
		switch string(raw[12:16]) {
		case "VP8 ":
			if len(raw) >= 30 && bytes.Equal(raw[23:26], []byte{0x9d, 0x01, 0x2a}) {
				return WebP, int(binary.LittleEndian.Uint16(raw[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(raw[28:30]) & 0x3fff), true
			}
		case "VP8L":
			if len(raw) >= 25 && raw[20] == 0x2f {
				bits := binary.LittleEndian.Uint32(raw[21:25])
				return WebP, int(bits&0x3fff) + 1, int(bits>>14&0x3fff) + 1, true
			}
		case "VP8X":
			if len(raw) >= 30 {
				return WebP, int(le24(raw[24:27])) + 1, int(le24(raw[27:30])) + 1, true
			}
		}
		return WebP, 0, 0, false
	}
	return UnknownFormat, 0, 0, false
}

// Dimensions of the first JPEG frame header, skipping fill bytes and stopping at the scan
func jpegHeaderSize(raw []byte) (int, int, bool) {
	i := 2
	for i+9 < len(raw) {
		if raw[i] != 0xff {
			i++
			continue
		}
		marker := raw[i+1]
		i += 2
		for marker == 0xff && i < len(raw) {
			marker = raw[i]
			i++
		}
		if marker == 0xd8 || marker == 0xd9 {
			continue
		}
		if marker == 0xda || i+2 > len(raw) {
			break
		}
		segment := int(binary.BigEndian.Uint16(raw[i : i+2]))
		if segment < 2 || i+segment > len(raw) {
			break
		}
		if jpegSOFMarkers[marker] && segment >= 7 {
			return int(binary.BigEndian.Uint16(raw[i+5 : i+7])), int(binary.BigEndian.Uint16(raw[i+3 : i+5])), true
		}
		i += segment
	}
	return 0, 0, false
}

// Longest PNG or JPEG side in a base64 data URL or bare base64, decoding only the leading characters
func MaxSideFromBase64(data string) (int, bool) {
	if data == "" {
		return 0, false
	}
	payload := data
	if strings.HasPrefix(payload, "data:") {
		head, rest, found := strings.Cut(payload, ",")
		if !found || !strings.Contains(head, ";base64") {
			return 0, false
		}
		payload = rest
	}
	payload = strings.TrimSpace(payload)
	payload = payload[:min(len(payload), headerProbeChars)]
	payload = payload[:len(payload)-len(payload)%4]
	if payload == "" {
		return 0, false
	}
	raw, err := decodeBase64Lenient(payload)
	if err != nil {
		return 0, false
	}
	f, w, h, ok := HeaderSize(raw)
	if !ok || (f != PNG && f != JPEG) {
		return 0, false
	}
	return max(w, h), true
}

// Little endian 24 bit value
func le24(b []byte) uint32 { return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 }
