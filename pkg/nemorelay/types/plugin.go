package types

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DiagnosticLevel is the severity of a configuration diagnostic
type DiagnosticLevel uint8

const (
	DiagnosticLevelWarning DiagnosticLevel = iota + 1
	DiagnosticLevelError
)

var diagnosticLevelNames = []string{"warning", "error"}

// String returns the lowercase wire name
func (l DiagnosticLevel) String() string {
	if l < 1 || int(l) > len(diagnosticLevelNames) {
		return ""
	}
	return diagnosticLevelNames[l-1]
}

func (l DiagnosticLevel) rustDebug(b *strings.Builder) {
	switch l {
	case DiagnosticLevelWarning:
		b.WriteString("Warning")
	case DiagnosticLevelError:
		b.WriteString("Error")
	}
}

func (l DiagnosticLevel) serdeSerialize() (any, error) { return l.String(), nil }

func (l *DiagnosticLevel) serdeDeserialize(v any) error {
	idx, err := decodeUnitEnum(v, diagnosticLevelNames)
	if err != nil {
		return err
	}
	*l = DiagnosticLevel(idx + 1)
	return nil
}

// MarshalJSON writes the serde form
func (l DiagnosticLevel) MarshalJSON() ([]byte, error) { return EncodeJSON(l) }

// UnmarshalJSON reads the serde form
func (l *DiagnosticLevel) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, l) }

// ConfigDiagnostic is one validation finding about plugin configuration
type ConfigDiagnostic struct {
	Level     DiagnosticLevel `json:"level"`
	Code      string          `json:"code"`
	Component *string         `json:"component" serde:"default,skip_none"`
	Field     *string         `json:"field" serde:"default,skip_none"`
	Message   string          `json:"message"`
}

// MarshalJSON writes the serde form
func (d ConfigDiagnostic) MarshalJSON() ([]byte, error) { return EncodeJSON(d) }

// UnmarshalJSON reads the serde form
func (d *ConfigDiagnostic) UnmarshalJSON(b []byte) error { return SerdeUnmarshal(b, d) }

// HTTPErrorMessage formats a plugin HTTP error message for statuses 400 through 599
func HTTPErrorMessage(status uint16, message string) (string, error) {
	if status < 400 || status > 599 {
		return "", errors.New("plugin HTTP error status must be between 400 and 599")
	}
	return fmt.Sprintf("HTTP %d: %s", status, message), nil
}

// ParseHTTPErrorMessage splits a plugin HTTP error message into status and body
func ParseHTTPErrorMessage(message string) (uint16, string, bool) {
	suffix, ok := strings.CutPrefix(message, "HTTP ")
	if !ok {
		return 0, "", false
	}
	code, body, ok := strings.Cut(suffix, ": ")
	if !ok || len(code) != 3 {
		return 0, "", false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return 0, "", false
		}
	}
	status, err := strconv.ParseUint(code, 10, 16)
	if err != nil || status < 400 || status > 599 {
		return 0, "", false
	}
	return uint16(status), body, true
}
