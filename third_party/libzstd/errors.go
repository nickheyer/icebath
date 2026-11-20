package libzstd

// ErrorCode is a ZSTD_ErrorCode, zero when nothing failed
type ErrorCode int

// ZSTD_ErrorCode values the decoders raise
const (
	ErrGeneric                     ErrorCode = 1
	ErrPrefixUnknown               ErrorCode = 10
	ErrVersionUnsupported          ErrorCode = 12
	ErrFrameParameterUnsupport     ErrorCode = 14
	ErrWindowTooLarge              ErrorCode = 16
	ErrCorruptionDetected          ErrorCode = 20
	ErrChecksumWrong               ErrorCode = 22
	ErrLiteralsHeaderWrong         ErrorCode = 24
	ErrDictionaryCorrupted         ErrorCode = 30
	ErrDictionaryWrong             ErrorCode = 32
	ErrTableLogTooLarge            ErrorCode = 44
	ErrMaxSymbolValueTooLarge      ErrorCode = 46
	ErrMaxSymbolValueTooSmall      ErrorCode = 48
	ErrStageWrong                  ErrorCode = 60
	ErrInitMissing                 ErrorCode = 62
	ErrMemoryAllocation            ErrorCode = 64
	ErrDstSizeTooSmall             ErrorCode = 70
	ErrSrcSizeWrong                ErrorCode = 72
	ErrNoForwardProgressDestFull   ErrorCode = 80
	ErrNoForwardProgressInputEmpty ErrorCode = 82
)

var errorStrings = map[ErrorCode]string{
	0:                              "No error detected",
	ErrGeneric:                     "Error (generic)",
	ErrPrefixUnknown:               "Unknown frame descriptor",
	ErrVersionUnsupported:          "Version not supported",
	ErrFrameParameterUnsupport:     "Unsupported frame parameter",
	ErrWindowTooLarge:              "Frame requires too much memory for decoding",
	ErrCorruptionDetected:          "Data corruption detected",
	ErrChecksumWrong:               "Restored data doesn't match checksum",
	ErrLiteralsHeaderWrong:         "Header of Literals' block doesn't respect format specification",
	ErrDictionaryCorrupted:         "Dictionary is corrupted",
	ErrDictionaryWrong:             "Dictionary mismatch",
	ErrTableLogTooLarge:            "tableLog requires too much memory : unsupported",
	ErrMaxSymbolValueTooLarge:      "Unsupported max Symbol Value : too large",
	ErrMaxSymbolValueTooSmall:      "Specified maxSymbolValue is too small",
	ErrStageWrong:                  "Operation not authorized at current processing stage",
	ErrInitMissing:                 "Context should be init first",
	ErrMemoryAllocation:            "Allocation error : not enough memory",
	ErrDstSizeTooSmall:             "Destination buffer is too small",
	ErrSrcSizeWrong:                "Src size is incorrect",
	ErrNoForwardProgressDestFull:   "Operation made no progress over multiple calls, due to output buffer being full",
	ErrNoForwardProgressInputEmpty: "Operation made no progress over multiple calls, due to input being empty",
}

// ERR_getErrorString text of the code
func (e ErrorCode) Error() string {
	if s, ok := errorStrings[e]; ok {
		return s
	}
	return "Unspecified error code"
}
