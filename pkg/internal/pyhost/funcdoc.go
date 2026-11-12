package pyhost

// Method definitions of documented callables, one per name and doc text
var docMethodDefs = map[[2]string]uintptr{}

// Native callable named name with ml_doc set to doc, whose leading name(params) line becomes its text signature
func (g *GIL) NewDocFunc(name, doc string, fn RawFunc) (uintptr, error) {
	self, err := g.newHandleObject("icebath", "GoFunctionHandle", &handleEntry{raw: fn})
	if err != nil {
		return 0, err
	}
	defer pyDecRef(self)
	typesMu.Lock()
	key := [2]string{name, doc}
	def, ok := docMethodDefs[key]
	if !ok {
		def = pyMemRawMalloc(32)
		*(*uintptr)(cptr(def)) = permanentCString(name)
		*(*uintptr)(cptr(def + 8)) = cbMethod
		*(*int32)(cptr(def + 16)) = methVarargsKeywords
		*(*int32)(cptr(def + 20)) = 0
		*(*uintptr)(cptr(def + 24)) = permanentCString(doc)
		docMethodDefs[key] = def
	}
	typesMu.Unlock()
	f := pyCFunctionNewEx(def, self, 0)
	if f == 0 {
		return 0, g.fetchError()
	}
	return f, nil
}
