package cpythonutil

import "unsafe"

// C API entry points bound once from the loaded libpython, every signature using integer, pointer and double words, which purego passes in registers on every supported system
var (
	pyPreConfigInitIsolatedConfig func(cfg uintptr)
	pyConfigInitIsolatedConfig    func(cfg uintptr)
	pyConfigClear                 func(cfg uintptr)
	pyFinalizeEx                  func() int32
	pyIsInitialized               func() int32

	pyEvalSaveThread             func() uintptr
	pyEvalRestoreThread          func(ts uintptr)
	pyGILStateEnsure             func() int32
	pyGILStateRelease            func(state int32)
	pyGILStateGetThisThreadState func() uintptr
	pyThreadStateGetUnchecked    func() uintptr

	pyIncRef func(o uintptr)
	pyDecRef func(o uintptr)

	pyObjectGetAttr        func(o, name uintptr) uintptr
	pyObjectSetAttr        func(o, name, v uintptr) int32
	pyObjectHasAttr        func(o, name uintptr) int32
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
	pyTypeIsSubtype        func(a, b uintptr) int32
	pyTypeFromSpec         func(spec uintptr) uintptr
	pyTypeGetSlot          func(t uintptr, slot int32) uintptr
	pyTypeGenericAlloc     func(t uintptr, n int) uintptr

	pyImportImport func(name uintptr) uintptr
	pySysGetObject func(name string) uintptr

	pyUnicodeFromStringAndSize func(p unsafe.Pointer, n int) uintptr
	pyUnicodeDecodeUTF8        func(p unsafe.Pointer, n int, errors string) uintptr
	pyUnicodeAsUTF8AndSize     func(o uintptr, size *int) uintptr
	pyUnicodeAsEncodedString   func(o uintptr, encoding, errors string) uintptr
	pyUnicodeInternInPlace     func(p *uintptr)

	pyLongFromLongLong          func(v int64) uintptr
	pyLongFromUnsignedLongLong  func(v uint64) uintptr
	pyLongAsLongLongAndOverflow func(o uintptr, overflow *int32) int64
	pyLongAsUnsignedLongLong    func(o uintptr) uint64
	pyLongFromString            func(s string, pend uintptr, base int32) uintptr
	pyNumberToBase              func(o uintptr, base int32) uintptr
	pyFloatFromDouble           func(v float64) uintptr
	pyFloatAsDouble             func(o uintptr) float64

	pyBytesFromStringAndSize func(p unsafe.Pointer, n int) uintptr
	pyBytesFromObject        func(o uintptr) uintptr
	pyBytesAsString          func(o uintptr) uintptr
	pyBytesSize              func(o uintptr) int
	pyByteArrayAsString      func(o uintptr) uintptr
	pyByteArraySize          func(o uintptr) int

	pyDictNew           func() uintptr
	pyDictItems         func(d uintptr) uintptr
	pyDictSize          func(d uintptr) int
	pyDictSetItem       func(d, k, v uintptr) int32
	pyDictNext          func(d uintptr, pos *int, k, v *uintptr) int32
	pyDictDelItemString func(d uintptr, key string) int32

	pyListNew     func(n int) uintptr
	pyListSetItem func(l uintptr, i int, o uintptr) int32
	pyListGetItem func(l uintptr, i int) uintptr
	pyListSize    func(l uintptr) int
	pyListInsert  func(l uintptr, i int, o uintptr) int32
	pyListAsTuple func(l uintptr) uintptr

	pyTupleNew     func(n int) uintptr
	pyTupleSetItem func(t uintptr, i int, o uintptr) int32
	pyTupleGetItem func(t uintptr, i int) uintptr
	pyTupleSize    func(t uintptr) int

	pyErrOccurred           func() uintptr
	pyErrClear              func()
	pyErrSetObject          func(t, v uintptr)
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

// Addresses of exported data objects: the object itself for the singletons and builtin types, the pointer variable for the PyExc names until resolveExceptionVariables reads it
var (
	pyNone, pyTrue, pyFalse                              uintptr
	pyBoolType, pyLongType, pyFloatType, pyUnicodeType   uintptr
	pyBytesType, pyByteArrayType, pyDictType, pyListType uintptr
	pyTupleType, pyBaseObjectType                        uintptr
	excRuntimeError, excAttributeError, excSystemError   uintptr
	hasRaisedException                                   bool
)

// Function symbol the host binds, optional ones left nil when absent and fallback naming an older spelling of the same function
type symbol struct {
	fptr     any
	name     string
	fallback string
	optional bool
}

// Every function symbol the host calls, resolved by bind
func functionTable() []symbol {
	return []symbol{
		{fptr: &pyPreConfigInitIsolatedConfig, name: "PyPreConfig_InitIsolatedConfig"},
		{fptr: &pyConfigInitIsolatedConfig, name: "PyConfig_InitIsolatedConfig"},
		{fptr: &pyConfigClear, name: "PyConfig_Clear"},
		{fptr: &pyFinalizeEx, name: "Py_FinalizeEx"},
		{fptr: &pyIsInitialized, name: "Py_IsInitialized"},
		{fptr: &pyEvalSaveThread, name: "PyEval_SaveThread"},
		{fptr: &pyEvalRestoreThread, name: "PyEval_RestoreThread"},
		{fptr: &pyGILStateEnsure, name: "PyGILState_Ensure"},
		{fptr: &pyGILStateRelease, name: "PyGILState_Release"},
		{fptr: &pyGILStateGetThisThreadState, name: "PyGILState_GetThisThreadState"},
		{fptr: &pyThreadStateGetUnchecked, name: "PyThreadState_GetUnchecked", fallback: "_PyThreadState_UncheckedGet", optional: true},
		{fptr: &pyIncRef, name: "Py_IncRef"},
		{fptr: &pyDecRef, name: "Py_DecRef"},
		{fptr: &pyObjectGetAttr, name: "PyObject_GetAttr"},
		{fptr: &pyObjectSetAttr, name: "PyObject_SetAttr"},
		{fptr: &pyObjectHasAttr, name: "PyObject_HasAttr"},
		{fptr: &pyObjectCall, name: "PyObject_Call"},
		{fptr: &pyObjectStr, name: "PyObject_Str"},
		{fptr: &pyObjectRepr, name: "PyObject_Repr"},
		{fptr: &pyObjectType, name: "PyObject_Type"},
		{fptr: &pyObjectIsInstance, name: "PyObject_IsInstance"},
		{fptr: &pyObjectIsSubclass, name: "PyObject_IsSubclass"},
		{fptr: &pyObjectGenericGetAttr, name: "PyObject_GenericGetAttr"},
		{fptr: &pyObjectIsTrue, name: "PyObject_IsTrue"},
		{fptr: &pyObjectGetIter, name: "PyObject_GetIter"},
		{fptr: &pyIterNext, name: "PyIter_Next"},
		{fptr: &pyCallableCheck, name: "PyCallable_Check"},
		{fptr: &pyTypeGetFlags, name: "PyType_GetFlags"},
		{fptr: &pyTypeIsSubtype, name: "PyType_IsSubtype"},
		{fptr: &pyTypeFromSpec, name: "PyType_FromSpec"},
		{fptr: &pyTypeGetSlot, name: "PyType_GetSlot"},
		{fptr: &pyTypeGenericAlloc, name: "PyType_GenericAlloc"},
		{fptr: &pyImportImport, name: "PyImport_Import"},
		{fptr: &pySysGetObject, name: "PySys_GetObject"},
		{fptr: &pyUnicodeFromStringAndSize, name: "PyUnicode_FromStringAndSize"},
		{fptr: &pyUnicodeDecodeUTF8, name: "PyUnicode_DecodeUTF8"},
		{fptr: &pyUnicodeAsUTF8AndSize, name: "PyUnicode_AsUTF8AndSize"},
		{fptr: &pyUnicodeAsEncodedString, name: "PyUnicode_AsEncodedString"},
		{fptr: &pyUnicodeInternInPlace, name: "PyUnicode_InternInPlace"},
		{fptr: &pyLongFromLongLong, name: "PyLong_FromLongLong"},
		{fptr: &pyLongFromUnsignedLongLong, name: "PyLong_FromUnsignedLongLong"},
		{fptr: &pyLongAsLongLongAndOverflow, name: "PyLong_AsLongLongAndOverflow"},
		{fptr: &pyLongAsUnsignedLongLong, name: "PyLong_AsUnsignedLongLong"},
		{fptr: &pyLongFromString, name: "PyLong_FromString"},
		{fptr: &pyNumberToBase, name: "PyNumber_ToBase"},
		{fptr: &pyFloatFromDouble, name: "PyFloat_FromDouble"},
		{fptr: &pyFloatAsDouble, name: "PyFloat_AsDouble"},
		{fptr: &pyBytesFromStringAndSize, name: "PyBytes_FromStringAndSize"},
		{fptr: &pyBytesFromObject, name: "PyBytes_FromObject"},
		{fptr: &pyBytesAsString, name: "PyBytes_AsString"},
		{fptr: &pyBytesSize, name: "PyBytes_Size"},
		{fptr: &pyByteArrayAsString, name: "PyByteArray_AsString"},
		{fptr: &pyByteArraySize, name: "PyByteArray_Size"},
		{fptr: &pyDictNew, name: "PyDict_New"},
		{fptr: &pyDictItems, name: "PyDict_Items"},
		{fptr: &pyDictSize, name: "PyDict_Size"},
		{fptr: &pyDictSetItem, name: "PyDict_SetItem"},
		{fptr: &pyDictNext, name: "PyDict_Next"},
		{fptr: &pyDictDelItemString, name: "PyDict_DelItemString"},
		{fptr: &pyListNew, name: "PyList_New"},
		{fptr: &pyListSetItem, name: "PyList_SetItem"},
		{fptr: &pyListGetItem, name: "PyList_GetItem"},
		{fptr: &pyListSize, name: "PyList_Size"},
		{fptr: &pyListInsert, name: "PyList_Insert"},
		{fptr: &pyListAsTuple, name: "PyList_AsTuple"},
		{fptr: &pyTupleNew, name: "PyTuple_New"},
		{fptr: &pyTupleSetItem, name: "PyTuple_SetItem"},
		{fptr: &pyTupleGetItem, name: "PyTuple_GetItem"},
		{fptr: &pyTupleSize, name: "PyTuple_Size"},
		{fptr: &pyErrOccurred, name: "PyErr_Occurred"},
		{fptr: &pyErrClear, name: "PyErr_Clear"},
		{fptr: &pyErrSetObject, name: "PyErr_SetObject"},
		{fptr: &pyErrFetch, name: "PyErr_Fetch"},
		{fptr: &pyErrNormalizeException, name: "PyErr_NormalizeException"},
		{fptr: &pyErrRestore, name: "PyErr_Restore"},
		{fptr: &pyErrGetRaisedException, name: "PyErr_GetRaisedException", optional: true},
		{fptr: &pyErrSetRaisedException, name: "PyErr_SetRaisedException", optional: true},
		{fptr: &pyExceptionSetTraceback, name: "PyException_SetTraceback"},
		{fptr: &pyCFunctionNewEx, name: "PyCFunction_NewEx"},
		{fptr: &pyMemRawMalloc, name: "PyMem_RawMalloc"},
		{fptr: &pyMemRawFree, name: "PyMem_RawFree"},
	}
}

// Exported data object, deref marking a PyExc variable that holds a pointer to the class
type dataSymbol struct {
	dst   *uintptr
	name  string
	deref bool
}

// Exported data objects the host reads
func dataTable() []dataSymbol {
	return []dataSymbol{
		{&pyNone, "_Py_NoneStruct", false}, {&pyTrue, "_Py_TrueStruct", false}, {&pyFalse, "_Py_FalseStruct", false},
		{&pyBoolType, "PyBool_Type", false}, {&pyLongType, "PyLong_Type", false}, {&pyFloatType, "PyFloat_Type", false},
		{&pyUnicodeType, "PyUnicode_Type", false}, {&pyBytesType, "PyBytes_Type", false},
		{&pyByteArrayType, "PyByteArray_Type", false}, {&pyDictType, "PyDict_Type", false},
		{&pyListType, "PyList_Type", false}, {&pyTupleType, "PyTuple_Type", false},
		{&pyBaseObjectType, "PyBaseObject_Type", false},
		{&excRuntimeError, "PyExc_RuntimeError", true}, {&excAttributeError, "PyExc_AttributeError", true},
		{&excSystemError, "PyExc_SystemError", true},
	}
}

// Resolves every symbol through lookup once and registers the Go function values, so no call looks a symbol up again
func bind(lookup func(string) (uintptr, error)) error {
	for _, s := range functionTable() {
		addr, err := lookup(s.name)
		if (err != nil || addr == 0) && s.fallback != "" {
			addr, err = lookup(s.fallback)
		}
		if err != nil || addr == 0 {
			if s.optional {
				continue
			}
			return &SymbolError{Name: s.name, Err: err}
		}
		registerFunc(s.fptr, addr)
	}
	hasRaisedException = pyErrGetRaisedException != nil && pyErrSetRaisedException != nil
	for _, d := range dataTable() {
		addr, err := lookup(d.name)
		if err != nil || addr == 0 {
			return &SymbolError{Name: d.name, Err: err}
		}
		*d.dst = addr
	}
	return bindStatus(lookup)
}

// Reads the class pointer stored in each PyExc variable, valid only after initialization
func resolveExceptionVariables() {
	for _, d := range dataTable() {
		if d.deref {
			*d.dst = *(*uintptr)(cptr(*d.dst))
		}
	}
}
