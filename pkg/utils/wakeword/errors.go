package wakewordutil

import (
	"errors"
	"runtime"
	"strconv"
)

// Error for a bundle with no tensorflowlite_c library in the places it keeps one
var ErrLibraryNotFound = errors.New("tensorflowlite_c library not found")

// Error for a platform that bundles carry no tensorflowlite_c build for
var ErrUnsupportedPlatform = errors.New("no tensorflowlite_c build for " + runtime.GOOS + "/" + runtime.GOARCH)

// Error for a model whose tensors do not have the sizes the wake word pipeline feeds and reads
var ErrModelShape = errors.New("unexpected wake word model tensor shape")

// Error for input that ends in a partial sample or embedding window
var ErrPartialFrame = errors.New("wake word input ends in a partial frame")

// TensorFlow Lite call on a model file that returned NULL or a failing status
type CallError struct {
	Model    string
	Function string
	Status   int32
}

// Model file, function and what it returned
func (e *CallError) Error() string {
	if e.Status == 0 {
		return e.Model + ": " + e.Function + " returned NULL"
	}
	msg := e.Model + ": " + e.Function + " failed with status " + strconv.Itoa(int(e.Status))
	if name := statusName(e.Status); name != "" {
		msg += " (" + name + ")"
	}
	return msg
}

// Meaning of a TfLiteStatus value, empty for one this package does not know
func statusName(status int32) string {
	switch status {
	case 1:
		return "error"
	case 2:
		return "delegate error"
	case 3:
		return "application error"
	case 4:
		return "delegate data not found"
	case 5:
		return "delegate data write error"
	case 6:
		return "delegate data read error"
	case 7:
		return "unresolved ops"
	case 8:
		return "cancelled"
	case 9:
		return "output shape not known"
	}
	return ""
}
