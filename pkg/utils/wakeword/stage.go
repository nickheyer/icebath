package wakewordutil

import (
	"fmt"
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

// One model file in its own interpreter with the input and output tensors the pipeline feeds and reads
type stage struct {
	rt          *Runtime
	path        string
	model       uintptr
	interpreter uintptr
	input       uintptr
	output      uintptr
	out         []float32
}

// Model loaded into a fresh interpreter, its first input resized to shape when one is given, its tensors allocated
func newStage(rt *Runtime, path string, shape []int32) (*stage, error) {
	s := &stage{rt: rt, path: path}
	if err := s.load(shape); err != nil {
		s.release()
		return nil, err
	}
	return s, nil
}

func (s *stage) load(shape []int32) error {
	rt := s.rt
	cpath := dlutil.CString(s.path)
	if s.model = rt.modelCreateFromFile(&cpath[0]); s.model == 0 {
		return s.null("TfLiteModelCreateFromFile")
	}
	if s.interpreter = rt.interpreterCreate(s.model, 0); s.interpreter == 0 {
		return s.null("TfLiteInterpreterCreate")
	}
	if len(shape) > 0 {
		if status := rt.interpreterResizeInputTensor(s.interpreter, 0, unsafe.SliceData(shape), int32(len(shape))); status != 0 {
			return s.failed("TfLiteInterpreterResizeInputTensor", status)
		}
	}
	if status := rt.interpreterAllocateTensors(s.interpreter); status != 0 {
		return s.failed("TfLiteInterpreterAllocateTensors", status)
	}
	if s.input = rt.interpreterGetInputTensor(s.interpreter, 0); s.input == 0 {
		return s.null("TfLiteInterpreterGetInputTensor")
	}
	if s.output = rt.interpreterGetOutputTensor(s.interpreter, 0); s.output == 0 {
		return s.null("TfLiteInterpreterGetOutputTensor")
	}
	return nil
}

// Whether the stage still holds its interpreter and model
func (s *stage) open() bool { return s != nil && s.interpreter != 0 && s.model != 0 }

// Deletes the interpreter, then the model, each at most once
func (s *stage) release() {
	if s == nil {
		return
	}
	if s.interpreter != 0 {
		s.rt.interpreterDelete(s.interpreter)
		s.interpreter = 0
	}
	if s.model != 0 {
		s.rt.modelDelete(s.model)
		s.model = 0
	}
	s.input, s.output = 0, 0
}

// ErrModelShape unless the input tensor holds exactly that many float32 values
func (s *stage) expectInput(values int) error {
	if size := s.rt.tensorByteSize(s.input); size != uintptr(values)*4 {
		return s.shapeError(fmt.Sprintf("input tensor holds %d bytes, want %d float32 values", size, values))
	}
	return nil
}

// Embedding windows a classifier scores at once, the second dimension of its input
func (s *stage) classifierWindows() (int, error) {
	dims := s.rt.tensorNumDims(s.input)
	if dims < 2 {
		return 0, s.shapeError(fmt.Sprintf("classifier input has %d dimensions, want at least 2", dims))
	}
	windows := int(s.rt.tensorDim(s.input, 1))
	if windows < 1 {
		return 0, s.shapeError(fmt.Sprintf("classifier input takes %d embedding windows", windows))
	}
	if err := s.expectInput(windows * wwFeatures); err != nil {
		return 0, err
	}
	return windows, nil
}

// Copies in to the input tensor, runs the interpreter and returns the output values in a buffer the next run overwrites
func (s *stage) run(in []float32) ([]float32, error) {
	rt := s.rt
	if status := rt.tensorCopyFromBuffer(s.input, unsafe.Pointer(unsafe.SliceData(in)), uintptr(len(in))*4); status != 0 {
		return nil, s.failed("TfLiteTensorCopyFromBuffer", status)
	}
	if status := rt.interpreterInvoke(s.interpreter); status != 0 {
		return nil, s.failed("TfLiteInterpreterInvoke", status)
	}
	size := rt.tensorByteSize(s.output)
	if size == 0 || size%4 != 0 {
		return nil, s.shapeError(fmt.Sprintf("output tensor holds %d bytes, not whole float32 values", size))
	}
	n := int(size / 4)
	if cap(s.out) < n {
		s.out = make([]float32, n)
	}
	out := s.out[:n]
	if status := rt.tensorCopyToBuffer(s.output, unsafe.Pointer(&out[0]), size); status != 0 {
		return nil, s.failed("TfLiteTensorCopyToBuffer", status)
	}
	return out, nil
}

func (s *stage) null(function string) error { return &CallError{Model: s.path, Function: function} }

func (s *stage) failed(function string, status int32) error {
	return &CallError{Model: s.path, Function: function, Status: status}
}

func (s *stage) shapeError(detail string) error {
	return fmt.Errorf("%s: %w: %s", s.path, ErrModelShape, detail)
}

// Shifts a rolling buffer left by the values appended at its end, keeping only the newest values when more arrive than it holds
func shiftAppend(buf, values []float32) {
	if len(values) >= len(buf) {
		copy(buf, values[len(values)-len(buf):])
		return
	}
	copy(buf, buf[len(values):])
	copy(buf[len(buf)-len(values):], values)
}
