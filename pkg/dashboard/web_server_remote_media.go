package dashboard

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
)

var singleRangeRE = regexp.MustCompile(`(?i)^[\s\x0b\x1c-\x1f\x{85}\x{a0}]*bytes[\s\x0b\x1c-\x1f\x{85}\x{a0}]*=[\s\x0b\x1c-\x1f\x{85}\x{a0}]*(\d*)` +
	`[\s\x0b\x1c-\x1f\x{85}\x{a0}]*-[\s\x0b\x1c-\x1f\x{85}\x{a0}]*(\d*)[\s\x0b\x1c-\x1f\x{85}\x{a0}]*\n?$`)

const maxPositionDigits = 19

// One past every file, ten to the nineteenth
const pastAnyFile uint64 = 10000000000000000000

// Remote workspace that streams a file, or one byte range of it, without buffering
type mediaStreamWorkspace interface {
	StreamFile(path string, chunkSize int, offset int64, length *int64) func(ctx context.Context, yield func([]byte) error) error
}

// Range byte position, or one past every file for a number too long to be in any file
func rangePosition(digits string) uint64 {
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		digits = "0"
	}
	if len(digits) > maxPositionDigits {
		return pastAnyFile
	}
	n, _ := strconv.ParseUint(digits, 10, 64)
	return n
}

// 416 naming the file size in Content-Range
func rangeUnsatisfiable(size int64) *HTTPException {
	return NewHTTPException(416, "Requested range not satisfiable", "Content-Range", fmt.Sprintf("bytes */%d", size))
}

// Inclusive first and last byte one bytes range asks of a file, false to serve it whole, 416 when unsatisfiable
func byteRange(header string, size int64) (int64, int64, bool, error) {
	m := singleRangeRE.FindStringSubmatch(header)
	if m == nil || (m[1] == "" && m[2] == "") {
		return 0, 0, false, nil
	}
	firstDigits, lastDigits := m[1], m[2]
	usize := uint64(size)
	if firstDigits == "" {
		suffix := rangePosition(lastDigits)
		if suffix == 0 || size == 0 {
			return 0, 0, false, rangeUnsatisfiable(size)
		}
		first := int64(0)
		if suffix < usize {
			first = int64(usize - suffix)
		}
		return first, size - 1, true, nil
	}
	first := rangePosition(firstDigits)
	if lastDigits != "" {
		if last := rangePosition(lastDigits); last < first {
			return 0, 0, false, nil
		}
	}
	if first >= usize {
		return 0, 0, false, rangeUnsatisfiable(size)
	}
	last := size - 1
	if lastDigits != "" {
		if l := rangePosition(lastDigits); l < uint64(size-1) {
			last = int64(l)
		}
	}
	return int64(first), last, true, nil
}

// Target on the SSH workspace as inline media, 200 whole, 206 for the asked range, 416 when unsatisfiable
func remoteMediaResponse(req *Request, workspace mediaStreamWorkspace, target string, size int64, mediaType string) (ASGIResponse, error) {
	first, last, ranged, err := byteRange(req.Headers().Get("range"), size)
	if err != nil {
		return nil, err
	}
	if !ranged {
		first, last = 0, size-1
	}
	length := max(last-first+1, 0)
	posix := pypath.PosixPurePathStr(target)
	name := pyurl.Quote(posix[strings.LastIndex(posix, "/")+1:], "/")
	headers := []string{
		"Accept-Ranges", "bytes",
		"Content-Disposition", "inline; filename*=UTF-8''" + name,
		"Content-Length", strconv.FormatInt(length, 10),
		"X-Content-Type-Options", "nosniff",
	}
	status := 200
	if ranged {
		status = 206
		headers = append(headers, "Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, size))
	}
	if req.Method() == "HEAD" || length == 0 {
		return NewResponse(nil, status, mediaType, headers...), nil
	}
	var body func(ctx context.Context, yield func([]byte) error) error
	if ranged {
		body = workspace.StreamFile(target, 64*1024, first, &length)
	} else {
		body = workspace.StreamFile(target, 64*1024, 0, nil)
	}
	return NewStreamingResponse(body, status, mediaType, headers...), nil
}
