package pyhost

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// C API entry points bound from the loaded libpython, every signature uses only integer and pointer words
var (
	pyPreConfigInitIsolatedConfig func(cfg uintptr)
	pyConfigInitIsolatedConfig    func(cfg uintptr)
	pyConfigClear                 func(cfg uintptr)
	pyFinalizeEx                  func() int32
	pyIsInitialized               func() int32

	pyEvalSaveThread    func() uintptr
	pyEvalRestoreThread func(ts uintptr)
	pyGILStateEnsure    func() int32
	pyGILStateRelease   func(state int32)

	pyIncRef func(o uintptr)
	pyDecRef func(o uintptr)

	pyObjectGetAttrString  func(o uintptr, name string) uintptr
	pyObjectSetAttrString  func(o uintptr, name string, v uintptr) int32
	pyObjectHasAttrString  func(o uintptr, name string) int32
	pyObjectCall           func(callable, args, kwargs uintptr) uintptr
	pyObjectStr            func(o uintptr) uintptr
	pyObjectRepr           func(o uintptr) uintptr
	pyObjectType           func(o uintptr) uintptr
	pyObjectIsInstance     func(o, cls uintptr) int32
	pyObjectIsSubclass     func(o, cls uintptr) int32
	pyObjectGenericGetAttr func(o, name uintptr) uintptr
	pyObjectIsTrue         func(o uintptr) int32
	pyObjectGetIter        func(o uintptr) uintptr
	pyIterNext             func(it uintptr) uintptr
	pyCallableCheck        func(o uintptr) int32
	pyTypeGetFlags         func(t uintptr) uint32
	pyTypeFromSpec         func(spec uintptr) uintptr
	pyTypeGetSlot          func(t uintptr, slot int32) uintptr
	pyTypeGenericAlloc     func(t uintptr, n int) uintptr

	pyImportImportModule func(name string) uintptr
	pySysGetObject       func(name string) uintptr

	pyUnicodeFromStringAndSize func(p unsafe.Pointer, n int) uintptr
	pyUnicodeAsUTF8AndSize     func(o uintptr, size *int) uintptr

	pyLongFromLongLong          func(v int64) uintptr
	pyLongFromUnsignedLongLong  func(v uint64) uintptr
	pyLongAsLongLongAndOverflow func(o uintptr, overflow *int32) int64
	pyLongAsUnsignedLongLong    func(o uintptr) uint64
	pyLongFromString            func(s string, pend uintptr, base int32) uintptr
	pyFloatFromString           func(o uintptr) uintptr

	pyBytesFromStringAndSize func(p unsafe.Pointer, n int) uintptr
	pyBytesAsString          func(o uintptr) uintptr
	pyBytesSize              func(o uintptr) int
	pyByteArrayAsString      func(o uintptr) uintptr
	pyByteArraySize          func(o uintptr) int

	pyDictNew           func() uintptr
	pyDictSetItem       func(d, k, v uintptr) int32
	pyDictNext          func(d uintptr, pos *int, k, v *uintptr) int32
	pyDictDelItemString func(d uintptr, key string) int32

	pyListNew     func(n int) uintptr
	pyListSetItem func(l uintptr, i int, o uintptr) int32
	pyListGetItem func(l uintptr, i int) uintptr
	pyListSize    func(l uintptr) int
	pyListInsert  func(l uintptr, i int, o uintptr) int32

	pyTupleNew     func(n int) uintptr
	pyTupleSetItem func(t uintptr, i int, o uintptr) int32
	pyTupleGetItem func(t uintptr, i int) uintptr
	pyTupleSize    func(t uintptr) int

	pyErrOccurred           func() uintptr
	pyErrClear              func()
	pyErrSetString          func(t uintptr, msg string)
	pyErrFetch              func(t, v, tb *uintptr)
	pyErrNormalizeException func(t, v, tb *uintptr)
	pyErrRestore            func(t, v, tb uintptr)
	pyErrGetRaisedException func() uintptr
	pyErrSetRaisedException func(exc uintptr)
	pyExceptionSetTraceback func(e, tb uintptr) int32
	pyCFunctionNewEx        func(ml, self, module uintptr) uintptr
	pyMemRawMalloc          func(n uintptr) uintptr
	pyMemRawFree            func(p uintptr)
)

// Addresses of exported data objects, read once after binding
var (
	pyNone, pyTrue, pyFalse                  uintptr
	pyBoolType, pyFloatType, pyByteArrayType uintptr
	pyBaseObjectType                         uintptr
	excRuntimeError                          uintptr
)

// Symbols newer than the oldest supported interpreter, bound when present and chosen at call time
var optional = map[string]bool{"PyErr_GetRaisedException": true, "PyErr_SetRaisedException": true}

type symbol struct {
	fptr any
	name string
}

// Every function symbol the host calls, resolved by bind
func functionTable() []symbol {
	return []symbol{
		{&pyPreConfigInitIsolatedConfig, "PyPreConfig_InitIsolatedConfig"},
		{&pyConfigInitIsolatedConfig, "PyConfig_InitIsolatedConfig"},
		{&pyConfigClear, "PyConfig_Clear"},
		{&pyFinalizeEx, "Py_FinalizeEx"},
		{&pyIsInitialized, "Py_IsInitialized"},
		{&pyEvalSaveThread, "PyEval_SaveThread"},
		{&pyEvalRestoreThread, "PyEval_RestoreThread"},
		{&pyGILStateEnsure, "PyGILState_Ensure"},
		{&pyGILStateRelease, "PyGILState_Release"},
		{&pyIncRef, "Py_IncRef"},
		{&pyDecRef, "Py_DecRef"},
		{&pyObjectGetAttrString, "PyObject_GetAttrString"},
		{&pyObjectSetAttrString, "PyObject_SetAttrString"},
		{&pyObjectHasAttrString, "PyObject_HasAttrString"},
		{&pyObjectCall, "PyObject_Call"},
		{&pyObjectStr, "PyObject_Str"},
		{&pyObjectRepr, "PyObject_Repr"},
		{&pyObjectType, "PyObject_Type"},
		{&pyObjectIsInstance, "PyObject_IsInstance"},
		{&pyObjectIsSubclass, "PyObject_IsSubclass"},
		{&pyObjectGenericGetAttr, "PyObject_GenericGetAttr"},
		{&pyObjectIsTrue, "PyObject_IsTrue"},
		{&pyObjectGetIter, "PyObject_GetIter"},
		{&pyIterNext, "PyIter_Next"},
		{&pyCallableCheck, "PyCallable_Check"},
		{&pyTypeGetFlags, "PyType_GetFlags"},
		{&pyTypeFromSpec, "PyType_FromSpec"},
		{&pyTypeGetSlot, "PyType_GetSlot"},
		{&pyTypeGenericAlloc, "PyType_GenericAlloc"},
		{&pyImportImportModule, "PyImport_ImportModule"},
		{&pySysGetObject, "PySys_GetObject"},
		{&pyUnicodeFromStringAndSize, "PyUnicode_FromStringAndSize"},
		{&pyUnicodeAsUTF8AndSize, "PyUnicode_AsUTF8AndSize"},
		{&pyLongFromLongLong, "PyLong_FromLongLong"},
		{&pyLongFromUnsignedLongLong, "PyLong_FromUnsignedLongLong"},
		{&pyLongAsLongLongAndOverflow, "PyLong_AsLongLongAndOverflow"},
		{&pyLongAsUnsignedLongLong, "PyLong_AsUnsignedLongLong"},
		{&pyLongFromString, "PyLong_FromString"},
		{&pyFloatFromString, "PyFloat_FromString"},
		{&pyBytesFromStringAndSize, "PyBytes_FromStringAndSize"},
		{&pyBytesAsString, "PyBytes_AsString"},
		{&pyBytesSize, "PyBytes_Size"},
		{&pyByteArrayAsString, "PyByteArray_AsString"},
		{&pyByteArraySize, "PyByteArray_Size"},
		{&pyDictNew, "PyDict_New"},
		{&pyDictSetItem, "PyDict_SetItem"},
		{&pyDictNext, "PyDict_Next"},
		{&pyDictDelItemString, "PyDict_DelItemString"},
		{&pyListNew, "PyList_New"},
		{&pyListSetItem, "PyList_SetItem"},
		{&pyListGetItem, "PyList_GetItem"},
		{&pyListSize, "PyList_Size"},
		{&pyListInsert, "PyList_Insert"},
		{&pyTupleNew, "PyTuple_New"},
		{&pyTupleSetItem, "PyTuple_SetItem"},
		{&pyTupleGetItem, "PyTuple_GetItem"},
		{&pyTupleSize, "PyTuple_Size"},
		{&pyErrOccurred, "PyErr_Occurred"},
		{&pyErrClear, "PyErr_Clear"},
		{&pyErrSetString, "PyErr_SetString"},
		{&pyErrFetch, "PyErr_Fetch"},
		{&pyErrNormalizeException, "PyErr_NormalizeException"},
		{&pyErrRestore, "PyErr_Restore"},
		{&pyErrGetRaisedException, "PyErr_GetRaisedException"},
		{&pyErrSetRaisedException, "PyErr_SetRaisedException"},
		{&pyExceptionSetTraceback, "PyException_SetTraceback"},
		{&pyCFunctionNewEx, "PyCFunction_NewEx"},
		{&pyMemRawMalloc, "PyMem_RawMalloc"},
		{&pyMemRawFree, "PyMem_RawFree"},
	}
}

type dataSymbol struct {
	dst   *uintptr
	name  string
	deref bool
}

// Exported data objects, the PyObject itself for structs and the pointer variable for PyExc names
func dataTable() []dataSymbol {
	return []dataSymbol{
		{&pyNone, "_Py_NoneStruct", false}, {&pyTrue, "_Py_TrueStruct", false}, {&pyFalse, "_Py_FalseStruct", false},
		{&pyBoolType, "PyBool_Type", false}, {&pyFloatType, "PyFloat_Type", false},
		{&pyByteArrayType, "PyByteArray_Type", false}, {&pyBaseObjectType, "PyBaseObject_Type", false},
		{&excRuntimeError, "PyExc_RuntimeError", true},
	}
}

var hasRaisedException bool

// Resolves every symbol through lookup and registers the Go function values
func bind(lookup func(string) (uintptr, error)) error {
	for _, s := range functionTable() {
		addr, err := lookup(s.name)
		if err != nil || addr == 0 {
			if optional[s.name] {
				continue
			}
			return fmt.Errorf("libpython lacks %s: %v", s.name, err)
		}
		purego.RegisterFunc(s.fptr, addr)
	}
	hasRaisedException = pyErrGetRaisedException != nil && pyErrSetRaisedException != nil
	for _, d := range dataTable() {
		addr, err := lookup(d.name)
		if err != nil || addr == 0 {
			return fmt.Errorf("libpython lacks data %s: %v", d.name, err)
		}
		*d.dst = addr
	}
	return bindStatus(lookup)
}

// Reads the PyObject pointer stored in each PyExc variable, valid only after initialization
func resolveExceptionVariables() {
	for _, d := range dataTable() {
		if d.deref {
			*d.dst = *(*uintptr)(cptr(*d.dst))
		}
	}
}
