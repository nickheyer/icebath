package mediacache

import (
	"bytes"
	"strings"
)

// Container id to canonical file extension
var ContainerToExt = map[string]string{
	"m4a": ".m4a", "mp4": ".mp4", "ogg": ".ogg", "flac": ".flac", "wav": ".wav", "mp3": ".mp3", "aac": ".aac", "webm": ".webm",
}

// MP4 ftyp brands that mean audio, the iOS voice note and audiobook brands
var mp4AudioBrands = [][]byte{[]byte("m4a "), []byte("m4b ")}

// Unambiguous fixed prefixes checked after ftyp and RIFF
var prefixContainers = []struct {
	prefix    []byte
	container string
}{{[]byte("OggS"), "ogg"}, {[]byte("fLaC"), "flac"}, {[]byte("ID3"), "mp3"}}

// Container key from magic bytes, false when the bytes match no audio or AV container
func SniffContainer(data []byte) (string, bool) {
	if len(data) >= 8 && string(data[4:8]) == "ftyp" {
		if len(data) >= 12 {
			brand := bytes.ToLower(data[8:12])
			for _, b := range mp4AudioBrands {
				if bytes.Equal(brand, b) {
					return "m4a", true
				}
			}
		}
		return "mp4", true
	}
	for _, p := range prefixContainers {
		if bytes.HasPrefix(data, p.prefix) {
			return p.container, true
		}
	}
	if len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WAVE" {
		return "wav", true
	}
	if len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0 {
		if data[1]&0xF6 == 0xF0 {
			return "aac", true
		}
		return "mp3", true
	}
	if bytes.HasPrefix(data, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "webm", true
	}
	return "", false
}

// Container matching extension with generic MP4 claimed as m4a, the dotted fallback when unknown
func SniffAudioExt(data []byte, fallbackExt string) string {
	fallback := fallbackExt
	if !strings.HasPrefix(fallback, ".") {
		fallback = "." + fallback
	}
	container, ok := SniffContainer(data)
	if !ok {
		return fallback
	}
	if container == "mp4" {
		return ".m4a"
	}
	return ContainerToExt[container]
}
