package cpythonutil

import (
	"errors"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Go value exposed to Python, its attribute lookups answered by GetAttr
type Object interface {
	TypeName() (module, name string)
	GetAttr(g *GIL, name string) (value any, ok bool, err error)
}

// Heap types of handle objects, one per module.name, held for the process lifetime
var (
	typesMu sync.Mutex
	goTypes = map[string]uintptr{}
)

// tp_free every handle type inherits, read once so a dealloc does not look the slot up, and whether some type turned out to differ
var (
	handleFree      atomic.Uintptr
	handleFreeMixed atomic.Bool
)

// Python object whose attribute lookup is answered by impl, a new reference
func (g *GIL) NewObject(impl Object) (uintptr, error) {
	module, name := impl.TypeName()
	return g.newHandleObject(module, name, &handleEntry{obj: impl})
}

// C string that lives for the rest of the process, 0 when the allocation fails
func permanentCString(s string) uintptr {
	p := pyMemRawMalloc(uintptr(len(s) + 1))
	if p == 0 {
		return 0
	}
	b := unsafe.Slice((*byte)(cptr(p)), len(s)+1)
	copy(b, s)
	b[len(s)] = 0
	return p
}

// PyType_Spec followed by its slot array in one C allocation that is never freed
func newSpec(name string, basicSize uintptr, flags uint32, slots [][2]uintptr) (uintptr, error) {
	n := len(slots) + 1
	mem := pyMemRawMalloc(uintptr(32 + 16*n))
	cname := permanentCString(name)
	if mem == 0 || cname == 0 {
		return 0, errors.New("cpython: out of memory for a type spec")
	}
	clear(unsafe.Slice((*byte)(cptr(mem)), 32+16*n))
	*(*uintptr)(cptr(mem)) = cname
	*(*int32)(cptr(mem + 8)) = int32(basicSize)
	*(*int32)(cptr(mem + 12)) = 0
	*(*uint32)(cptr(mem + 16)) = flags
	slotArr := mem + 32
	*(*uintptr)(cptr(mem + 24)) = slotArr
	for i, s := range slots {
		*(*int32)(cptr(slotArr + uintptr(16*i))) = int32(s[0])
		*(*uintptr)(cptr(slotArr + uintptr(16*i) + 8)) = s[1]
	}
	return mem, nil
}

// Heap type for Go handle objects of one Python class name, created once per name
func (g *GIL) goType(module, name string) (uintptr, error) {
	key := module + "." + name
	typesMu.Lock()
	t, ok := goTypes[key]
	typesMu.Unlock()
	if ok {
		return t, nil
	}
	// Built without the lock: creating a type can run the cyclic collector and with it Python code, which may hand the GIL to a thread waiting on typesMu
	spec, err := newSpec(name, objectBasicSize+8, tpflagsDefault|tpflagsDisallowCreate, [][2]uintptr{
		{slotTpGetattro, cbGetattro}, {slotTpDealloc, cbDealloc},
	})
	if err != nil {
		return 0, err
	}
	if t = pyTypeFromSpec(spec); t == 0 {
		return 0, g.fetchError()
	}
	if err := g.SetAttr(t, "__module__", module); err != nil {
		pyDecRef(t)
		return 0, err
	}
	free := pyTypeGetSlot(t, slotTpFree)
	if !handleFree.CompareAndSwap(0, free) && handleFree.Load() != free {
		handleFreeMixed.Store(true)
	}
	typesMu.Lock()
	prev, ok := goTypes[key]
	if !ok {
		goTypes[key] = t
	}
	typesMu.Unlock()
	if ok {
		pyDecRef(t)
		return prev, nil
	}
	return t, nil
}

// New handle object of the module.name handle type registered to e, a new reference
func (g *GIL) newHandleObject(module, name string, e *handleEntry) (uintptr, error) {
	t, err := g.goType(module, name)
	if err != nil {
		return 0, err
	}
	o := pyTypeGenericAlloc(t, 0)
	if o == 0 {
		return 0, g.fetchError()
	}
	id := handleSeq.Add(1)
	handlesMu.Lock()
	handles[id] = e
	handlesMu.Unlock()
	*(*uint64)(cptr(o + objectBasicSize)) = id
	return o, nil
}
