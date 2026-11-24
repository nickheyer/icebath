package textutil

import (
	"strconv"
	"unicode/utf8"
)

// Error for text that is not valid UTF-8, located at its first invalid byte
type InvalidUTF8Error struct {
	// Byte offset of the first byte that starts no valid UTF-8 sequence
	Offset int
}

// Message naming the byte offset where the valid UTF-8 ends
func (e *InvalidUTF8Error) Error() string {
	return "invalid UTF-8 at byte " + strconv.Itoa(e.Offset)
}

// Nil when data is valid UTF-8, otherwise an *InvalidUTF8Error at its first invalid byte
func ValidateUTF8[T ~string | ~[]byte](data T) error {
	if i := invalidUTF8Offset(data); i >= 0 {
		return &InvalidUTF8Error{Offset: i}
	}
	return nil
}

// Text of data when it is valid UTF-8, a leading byte order mark kept, otherwise no text and an *InvalidUTF8Error
func DecodeUTF8(data []byte) (string, error) {
	if i := invalidUTF8Offset(data); i >= 0 {
		return "", &InvalidUTF8Error{Offset: i}
	}
	return string(data), nil
}

// Data without one leading UTF-8 byte order mark, unchanged when it starts with none
func TrimBOM[T ~string | ~[]byte](data T) T {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data[3:]
	}
	return data
}

// Offset of the first byte of data that starts no valid UTF-8 sequence, -1 when there is none
func invalidUTF8Offset[T ~string | ~[]byte](data T) int {
	switch d := any(data).(type) {
	case string:
		if utf8.ValidString(d) {
			return -1
		}
	case []byte:
		if utf8.Valid(d) {
			return -1
		}
	}
	for i := 0; i < len(data); {
		if data[i] < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(string(data[i:min(i+utf8.UTFMax, len(data))]))
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return -1
}
