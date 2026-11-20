package tuiserver

// Byte cap of an attached payload the owner's image staging enforces
const AttachBytesMaxBytes = attachBytesMaxBytes

// Decoded data URL or bare base64 payload under the cap, nil when invalid
func DecodeAttachBase64(raw, mimePrefix string, maxBytes int) ([]byte, error) {
	return decodeAttachBase64(raw, mimePrefix, maxBytes, "attachment")
}

// Image extension from the filename hint, else magic bytes, else png
func SniffImageExt(data []byte, filename string) string { return sniffImageExt(data, filename) }

// Private collision free image staging shared by local and canonical byte uploads
func StageImageBytes(directory string, data []byte, ext, prefix string) (string, error) {
	return stageImageBytes(directory, data, ext, prefix)
}
