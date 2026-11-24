package acputil

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"

	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
)

// Size of the buffer frames are read through
const readBuffer = 64 << 10

type requestFrame struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type notificationFrame struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type resultFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result"`
}

type errorFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Error   wireError       `json:"error"`
}

// Error object of an error frame, data always written
type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// Members of an incoming frame, each the raw JSON of its value, nil when absent
type inFrame struct {
	id, method, params, result, err []byte
}

// Members of the frame object text data, the last of a repeated key winning
func parseFrame(data []byte) inFrame {
	var f inFrame
	for key, value := range members(data) {
		switch string(key) {
		case "id":
			f.id = value
		case "method":
			f.method = value
		case "params":
			f.params = value
		case "result":
			f.result = value
		case "error":
			f.err = value
		}
	}
	return f
}

// Name a frame's method member gives, and whether the frame calls a method: the member present and not null, a name that is not a string read as empty
func (f inFrame) call() (string, bool) {
	if f.method == nil || isNull(f.method) {
		return "", false
	}
	name, _ := unquote(f.method)
	return name, true
}

// One frame as compact JSON with every character outside ASCII escaped, followed by a line end
func encodeFrame(frame any) ([]byte, error) {
	line, err := jsonutil.Append(nil, frame, jsonutil.ASCII())
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

// Next line of br without its line end, a fresh slice; the last unterminated line comes with io.EOF. A line longer than a positive limit is read to its end without being kept: over is set and only its first bytes come back, enough to name the frame
func readFrame(br *bufio.Reader, limit int) ([]byte, bool, error) {
	var line []byte
	over := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !over {
			line = append(line, chunk...)
			n := len(line)
			if n > 0 && line[n-1] == '\n' {
				n--
			}
			if limit > 0 && n > limit {
				over = true
				line = slices.Clone(line[:min(len(line), readBuffer)])
			}
		}
		switch {
		case err == nil:
			if over {
				return line, true, nil
			}
			return bytes.TrimRight(line, "\r\n"), false, nil
		case errors.Is(err, bufio.ErrBufferFull):
		default:
			return line, over, err
		}
	}
}

// Request id of a reply's id member: an integer, or a number with no fraction
func requestKey(raw []byte) (int64, bool) {
	text := string(bytes.TrimSpace(raw))
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || f != math.Trunc(f) || f < math.MinInt64 || f >= math.MaxInt64 {
		return 0, false
	}
	return int64(f), true
}

// Request error of an error member: its code, message and data, an internal error code when the code is missing; a bare string member is taken as the message
func decodeWireError(raw []byte) *RequestError {
	re := &RequestError{Code: CodeInternalError}
	if s, ok := unquote(bytes.TrimSpace(raw)); ok {
		re.Message = s
	}
	for key, value := range members(raw) {
		switch string(key) {
		case "code":
			if n, ok := requestKey(value); ok && n >= math.MinInt32 && n <= math.MaxInt32 {
				re.Code = int(n)
			}
		case "message":
			if s, ok := unquote(value); ok {
				re.Message = s
			} else if !isNull(value) {
				re.Message = string(value)
			}
		case "data":
			re.Data, _ = dynamic(value)
		}
	}
	if re.Message == "" {
		re.Message = "request failed with code " + strconv.Itoa(re.Code)
	}
	return re
}
