package libzstd

// ContentSizeError is ZSTD_CONTENTSIZE_ERROR, the decompressed bound of a frame that could not be sized
const ContentSizeError = lContentSizeError

// IsLegacy reports the format version of a v0.5, v0.6 or v0.7 frame start, zero for anything else
func IsLegacy(src []byte) uint32 {
	if len(src) < 4 {
		return 0
	}
	switch readLE32(src, 0) {
	case v05Magic:
		return 5
	case v06Magic:
		return 6
	case v07Magic:
		return 7
	}
	return 0
}

// GetDecompressedSizeLegacy is the content size a legacy frame header declares, zero when it declares none
func GetDecompressedSizeLegacy(src []byte) uint64 {
	switch IsLegacy(src) {
	case 6:
		var fp v06FrameParams
		if need, e := v06GetFrameParams(&fp, src); need != 0 || e != 0 {
			return 0
		}
		return fp.frameContentSize
	case 7:
		var fp v07FrameParams
		if need, e := v07GetFrameParams(&fp, src); need != 0 || e != 0 {
			return 0
		}
		return fp.frameContentSize
	}
	return 0
}

// DecompressLegacy decodes one whole legacy frame with an optional dictionary into dst and returns the bytes written
func DecompressLegacy(dst, src, dict []byte) (int, ErrorCode) {
	switch IsLegacy(src) {
	case 5:
		return v05Decompress(dst, src, dict)
	case 6:
		return v06Decompress(dst, src, dict)
	case 7:
		return v07Decompress(dst, src, dict)
	}
	return 0, ErrPrefixUnknown
}

// FindFrameSizeInfoLegacy returns the compressed size of the legacy frame starting src and the bound of its content
func FindFrameSizeInfoLegacy(src []byte) (compressedSize uint64, decompressedBound uint64, e ErrorCode) {
	var cSize int
	switch IsLegacy(src) {
	case 5:
		cSize, decompressedBound, e = v05FindFrameSizeInfo(src)
	case 6:
		cSize, decompressedBound, e = v06FindFrameSizeInfo(src)
	case 7:
		cSize, decompressedBound, e = v07FindFrameSizeInfo(src)
	default:
		return 0, ContentSizeError, ErrPrefixUnknown
	}
	if e == 0 && cSize > len(src) {
		return 0, ContentSizeError, ErrSrcSizeWrong
	}
	return uint64(cSize), decompressedBound, e
}

// LegacyStream is the buffered decoder ZSTD_initLegacyStream keeps for one legacy format version
type LegacyStream struct {
	v05 *v05ZBuff
	z   *lzbuff
}

// InitLegacyStream readies the stream decoder of newVersion with dict, reusing prev when the version did not change
func InitLegacyStream(prev *LegacyStream, prevVersion, newVersion uint32, dict []byte) *LegacyStream {
	s := prev
	if prevVersion != newVersion {
		s = &LegacyStream{}
		switch newVersion {
		case 5:
			s.v05 = newV05ZBuff()
		case 6:
			s.z = newV06ZBuff()
		case 7:
			s.z = newV07ZBuff()
		}
	}
	switch newVersion {
	case 5:
		s.v05.decompressInit(dict)
	case 6, 7:
		s.z.decompressInit(dict)
	}
	return s
}

// Decompress runs ZSTD_decompressLegacyStream, the bytes written to out and read from in then the next input hint
func (s *LegacyStream) Decompress(version uint32, out, in []byte) (written, read int, hint uint64, e ErrorCode) {
	switch version {
	case 5:
		return s.v05.decompressContinue(out, in)
	case 6, 7:
		return s.z.decompressContinue(out, in)
	}
	return 0, 0, 0, ErrVersionUnsupported
}
