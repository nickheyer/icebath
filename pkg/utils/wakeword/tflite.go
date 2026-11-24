package wakewordutil

import (
	"path/filepath"
	"unsafe"

	dlutil "github.com/nickheyer/icebath/pkg/utils/dl"
)

// TensorFlow Lite C library with the functions the wake word models call bound, kept loaded for the life of the process
type Runtime struct {
	modelCreateFromFile          func(path *byte) uintptr
	interpreterCreate            func(model, options uintptr) uintptr
	interpreterAllocateTensors   func(interpreter uintptr) int32
	interpreterInvoke            func(interpreter uintptr) int32
	interpreterGetInputTensor    func(interpreter uintptr, index int32) uintptr
	interpreterGetOutputTensor   func(interpreter uintptr, index int32) uintptr
	tensorByteSize               func(tensor uintptr) uintptr
	tensorNumDims                func(tensor uintptr) int32
	tensorDim                    func(tensor uintptr, index int32) int32
	interpreterResizeInputTensor func(interpreter uintptr, index int32, dims *int32, numDims int32) int32
	tensorCopyFromBuffer         func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	tensorCopyToBuffer           func(tensor uintptr, data unsafe.Pointer, size uintptr) int32
	interpreterDelete            func(interpreter uintptr)
	modelDelete                  func(model uintptr)
}

// Runtime over the tensorflowlite_c library file at path, its symbols made global for the delegates it loads
func OpenRuntime(path string) (*Runtime, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lib, err := dlutil.Open(abs, true)
	if err != nil {
		return nil, err
	}
	rt := &Runtime{}
	for _, entry := range []struct {
		fptr any
		name string
	}{
		{&rt.modelCreateFromFile, "TfLiteModelCreateFromFile"}, {&rt.interpreterCreate, "TfLiteInterpreterCreate"},
		{&rt.interpreterAllocateTensors, "TfLiteInterpreterAllocateTensors"}, {&rt.interpreterInvoke, "TfLiteInterpreterInvoke"},
		{&rt.interpreterGetInputTensor, "TfLiteInterpreterGetInputTensor"}, {&rt.interpreterGetOutputTensor, "TfLiteInterpreterGetOutputTensor"},
		{&rt.tensorByteSize, "TfLiteTensorByteSize"}, {&rt.tensorNumDims, "TfLiteTensorNumDims"}, {&rt.tensorDim, "TfLiteTensorDim"},
		{&rt.interpreterResizeInputTensor, "TfLiteInterpreterResizeInputTensor"}, {&rt.tensorCopyFromBuffer, "TfLiteTensorCopyFromBuffer"},
		{&rt.tensorCopyToBuffer, "TfLiteTensorCopyToBuffer"}, {&rt.interpreterDelete, "TfLiteInterpreterDelete"},
		{&rt.modelDelete, "TfLiteModelDelete"},
	} {
		if err := lib.Bind(entry.fptr, entry.name); err != nil {
			return nil, err
		}
	}
	return rt, nil
}
