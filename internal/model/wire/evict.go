package wire

import (
	"bytes"
	"encoding/binary"
	"strings"
)

const (
	outboundImageLimit     = 20
	outboundImageHardLimit = 100
	outboundImageBudget    = 24_000_000
	imageEvictionBatch     = 8
	outboundImageFloor     = 3
	manyImageSideLimit     = 2000
	dimensionProbeChars    = 32 * 1024
)

// Width and height read from the header of PNG or JPEG bytes, false when neither format reveals them
func ImageDimensions(raw []byte) (width, height int, ok bool) {
	if bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) && len(raw) >= 24 {
		return int(binary.BigEndian.Uint32(raw[16:20])), int(binary.BigEndian.Uint32(raw[20:24])), true
	}
	if len(raw) <= 4 || raw[0] != 0xff || raw[1] != 0xd8 {
		return 0, 0, false
	}
	for i := 2; i+9 < len(raw); {
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
		if sofMarker(marker) && segment >= 7 {
			return int(binary.BigEndian.Uint16(raw[i+5 : i+7])), int(binary.BigEndian.Uint16(raw[i+3 : i+5])), true
		}
		i += segment
	}
	return 0, 0, false
}

// Reports whether a JPEG marker starts a frame header that carries the image size
func sofMarker(marker byte) bool {
	switch marker {
	case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
		return true
	}
	return false
}

// Longest side of a base64 image given bare or as a data URL, read from its leading bytes only
func ImageMaxSide(data string) (int, bool) {
	if data == "" {
		return 0, false
	}
	payload := data
	if strings.HasPrefix(data, "data:") {
		header, rest, found := strings.Cut(data, ",")
		if !found || !strings.Contains(header, ";base64") {
			return 0, false
		}
		payload = rest
	}
	payload = trimSpace(payload)
	prefix := payload[:min(len(payload), dimensionProbeChars)]
	prefix = prefix[:len(prefix)-len(prefix)%4]
	raw, ok := decodeBase64Lenient(prefix)
	if !ok || prefix == "" {
		return 0, false
	}
	width, height, ok := ImageDimensions(raw)
	return max(width, height), ok
}

// Value of a character of the standard base64 alphabet
func base64Value(c byte) (byte, bool) {
	switch {
	case c >= 'A' && c <= 'Z':
		return c - 'A', true
	case c >= 'a' && c <= 'z':
		return c - 'a' + 26, true
	case c >= '0' && c <= '9':
		return c - '0' + 52, true
	case c == '+':
		return 62, true
	case c == '/':
		return 63, true
	}
	return 0, false
}

// Decodes base64 skipping characters outside the alphabet and stopping at complete padding, false on a partial quantum
func decodeBase64Lenient(text string) ([]byte, bool) {
	out := make([]byte, 0, len(text)/4*3)
	quad, pads := 0, 0
	var left byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c >= 0x80 {
			return nil, false
		}
		if c == '=' {
			if quad >= 2 {
				if pads++; quad+pads >= 4 {
					return out, true
				}
			}
			continue
		}
		v, ok := base64Value(c)
		if !ok {
			continue
		}
		pads = 0
		switch quad {
		case 0:
			quad, left = 1, v
		case 1:
			quad, out, left = 2, append(out, left<<2|v>>4), v&0x0f
		case 2:
			quad, out, left = 3, append(out, left<<4|v>>2), v&0x03
		case 3:
			quad, out = 0, append(out, left<<6|v)
		}
	}
	return out, quad == 0
}

// Reports whether every image is readable and within the side limit above which a request's cap tightens
func ImagesWithinManyImageLimit(datas []string) bool {
	for _, data := range datas {
		if side, ok := ImageMaxSide(data); !ok || side > manyImageSideLimit {
			return false
		}
	}
	return true
}

// Image load of one request, tool result carriers newest first and the uploads that are never rewritten
type ImageLoad struct {
	CarrierBlocks  []int
	CarrierBytes   []int
	ReservedBlocks int
	ReservedBytes  int
	ManyImageSafe  bool
}

// How many of the oldest image-bearing tool results to retire, a step function of the overshoot
func ImageRetireCount(load ImageLoad) int {
	limit := outboundImageLimit
	if load.ManyImageSafe {
		limit = outboundImageHardLimit
	}
	total, retire := len(load.CarrierBlocks), 0
	for m := 1; m <= total; m++ {
		var sizes []int
		if load.CarrierBytes != nil {
			sizes = load.CarrierBytes[total-m:]
		}
		retire = max(retire, retireCountForRequest(load.CarrierBlocks[total-m:], load.ReservedBlocks, sizes, load.ReservedBytes, limit))
	}
	return retire
}

// Retire count for one request taken alone
func retireCountForRequest(blocks []int, reservedBlocks int, sizes []int, reservedBytes, limit int) int {
	total := len(blocks)
	blocksKept := make([]int, total+1)
	bytesKept := make([]int, total+1)
	for i := range total {
		blocksKept[i+1] = blocksKept[i] + blocks[i]
		if sizes != nil {
			bytesKept[i+1] = bytesKept[i] + sizes[i]
		}
	}
	bytesFit := func(kept int) bool {
		return sizes == nil || reservedBytes+bytesKept[kept] <= outboundImageBudget
	}
	fits := func(kept int) bool {
		return reservedBlocks+blocksKept[kept] <= limit && bytesFit(kept)
	}
	if fits(total) {
		return 0
	}
	floor := min(outboundImageFloor, total)
	if !fits(0) && bytesFit(floor) {
		byteWindow := floor
		for k := floor; k <= total; k++ {
			if bytesFit(k) {
				byteWindow = k
			}
		}
		quantum := max(1, min(imageEvictionBatch, byteWindow-floor+1))
		excess := total - floor
		return excess - excess%quantum
	}
	window := 0
	if fits(0) {
		for k := 0; k <= total; k++ {
			if fits(k) {
				window = k
			}
		}
	}
	quantum := max(1, min(imageEvictionBatch, window-floor))
	retire := 0
	for retire < total {
		retire = min(retire+quantum, total)
		if fits(total - retire) {
			break
		}
	}
	return retire
}

// Tool call ids whose images one session's send path already retired, so a later request never restores them
type ImageLatch struct {
	session string
	retired map[string]bool
}

// Widens a retire count to cover every carrier up to the newest one retired before, then records the retired ids
func (l *ImageLatch) Retire(session string, retire int, carrierIDsNewestFirst []string) int {
	if l == nil {
		return retire
	}
	if l.retired == nil || l.session != session {
		l.session, l.retired = session, map[string]bool{}
	}
	total := len(carrierIDsNewestFirst)
	for n, id := range carrierIDsNewestFirst {
		if id != "" && l.retired[id] {
			retire = max(retire, total-n)
			break
		}
	}
	for _, id := range carrierIDsNewestFirst[total-retire:] {
		if id != "" {
			l.retired[id] = true
		}
	}
	return retire
}
