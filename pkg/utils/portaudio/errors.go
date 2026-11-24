package portaudioutil

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

// Error for a build that has no dynamic library loader for its platform and so cannot load PortAudio
var ErrUnsupported = fmt.Errorf("load portaudio: %w", dlutil.ErrUnsupported)

// Error for any call on a stream once Close has begun, a second Close included
var ErrClosed = errors.New("portaudio stream is closed")

// Error for Playback asked before any Play
var ErrNoPlayback = errors.New("no portaudio playback started")

// Error for audio data that does not match the channel count of its stream
var ErrChannelMismatch = errors.New("audio data does not match the stream's channel count")

var errNoLibrary = errors.New("no library found")

// PortAudio library that could not be found, opened or bound
type LibraryError struct{ Err error }

// What kept the library from loading
func (e *LibraryError) Error() string {
	if e.Err == nil {
		return "load portaudio: library unavailable"
	}
	return "load portaudio: " + e.Err.Error()
}

// The loader's or binder's own error
func (e *LibraryError) Unwrap() error { return e.Err }

// Failed PortAudio call: the operation, PortAudio's error code and text, and the host API's own error when PortAudio reports one
type Error struct {
	Op       string
	Code     int
	Text     string
	HostAPI  string
	HostCode int
	HostText string
}

// Operation, PortAudio's text and code, then the host API's error
func (e *Error) Error() string {
	var b strings.Builder
	if e.Op != "" {
		b.WriteString(e.Op)
		b.WriteString(": ")
	}
	b.WriteString(e.Text)
	if e.Code != 0 {
		b.WriteString(" (PortAudio error ")
		b.WriteString(strconv.Itoa(e.Code))
		b.WriteByte(')')
	}
	if e.HostAPI != "" || e.HostCode != 0 || e.HostText != "" {
		api := e.HostAPI
		if api == "" {
			api = "host API"
		}
		fmt.Fprintf(&b, ": %s error %d: %s", api, e.HostCode, e.HostText)
	}
	return b.String()
}

// Device selector that names no device with channels in a direction, or a name query that matches several
type DeviceError struct {
	Selector  any
	Direction Direction
	Matches   []string
}

// Which selector failed and, for an ambiguous query, the "[index] name, host API" line of each match
func (e *DeviceError) Error() string {
	if len(e.Matches) > 0 {
		return fmt.Sprintf("several %s devices match %q:\n%s", e.Direction, fmt.Sprint(e.Selector), strings.Join(e.Matches, "\n"))
	}
	switch sel := e.Selector.(type) {
	case nil:
		return "no default " + e.Direction.String() + " device"
	case int:
		return fmt.Sprintf("no %s device at index %d", e.Direction, sel)
	}
	return fmt.Sprintf("no %s device matches %q", e.Direction, fmt.Sprint(e.Selector))
}

// Error for a negative PortAudio code, with the host API's details for an unanticipated host error
func check(code int32, op string) error {
	if code >= 0 {
		return nil
	}
	err := &Error{Op: op, Code: int(code), Text: cstr(paGetErrorText(code))}
	if code == paUnanticipatedHostError {
		if info := paGetLastHostErrorInfo(); info != nil {
			err.HostCode, err.HostText = int(info.errorCode), cstr(info.errorText)
			if api, qerr := hostAPI(int(paHostApiTypeIdToHostApiIndex(info.hostAPIType))); qerr == nil {
				err.HostAPI = api.Name
			}
		}
	}
	return err
}
