package pyhost

import (
	"fmt"
	"unsafe"

	"github.com/ebitengine/purego"
)

// PEP 741 entry points, present from 3.14, configure by option name with no struct layout
var (
	pyInitConfigCreate         func() uintptr
	pyInitConfigFree           func(cfg uintptr)
	pyInitConfigGetError       func(cfg uintptr, msg *uintptr) int32
	pyInitConfigSetInt         func(cfg uintptr, name string, v int64) int32
	pyInitConfigSetStr         func(cfg uintptr, name, v string) int32
	pyInitConfigSetStrList     func(cfg uintptr, name string, n uintptr, items unsafe.Pointer) int32
	pyInitializeFromInitConfig func(cfg uintptr) int32
	initConfigSymbols          = []string{"PyInitConfig_Create", "PyInitConfig_Free", "PyInitConfig_GetError", "PyInitConfig_SetInt", "PyInitConfig_SetStr", "PyInitConfig_SetStrList", "Py_InitializeFromInitConfig"}
	hasInitConfig              bool
)

// Binds the PEP 741 functions when every one of them is exported
func bindInitConfig(lookup func(string) (uintptr, error)) {
	ptrs := []any{&pyInitConfigCreate, &pyInitConfigFree, &pyInitConfigGetError, &pyInitConfigSetInt, &pyInitConfigSetStr, &pyInitConfigSetStrList, &pyInitializeFromInitConfig}
	addrs := make([]uintptr, len(ptrs))
	for i, name := range initConfigSymbols {
		addr, err := lookup(name)
		if err != nil || addr == 0 {
			return
		}
		addrs[i] = addr
	}
	for i := range ptrs {
		purego.RegisterFunc(ptrs[i], addrs[i])
	}
	hasInitConfig = true
}

// Settings applied by both initialization paths
type initSettings struct {
	ints    []intSetting
	strs    []strSetting
	argv    []string
	program string
}

type intSetting struct {
	name  string
	value int64
}

type strSetting struct {
	name  string
	value string
}

// The embedding profile, isolated unless Environment honours PYTHON variables, venv aware through executable and home
func settingsFor(opts Options) initSettings {
	in := opts.Interpreter
	s := initSettings{
		ints: []intSetting{
			{"isolated", 1}, {"use_environment", 0}, {"user_site_directory", 0}, {"safe_path", 1},
			{"install_signal_handlers", 0}, {"pathconfig_warnings", 0}, {"parse_argv", 0},
			{"configure_c_stdio", 0}, {"site_import", 1}, {"write_bytecode", 1}, {"utf8_mode", 1},
			{"faulthandler", 0}, {"dev_mode", 0},
		},
		strs: []strSetting{
			{"program_name", in.Executable}, {"executable", in.Executable}, {"home", in.BasePrefix},
		},
		argv: opts.Argv,
	}
	if opts.PycachePrefix != "" {
		s.strs = append(s.strs, strSetting{"pycache_prefix", opts.PycachePrefix})
	}
	if opts.Environment {
		for i, setting := range s.ints {
			switch setting.name {
			case "isolated", "safe_path":
				s.ints[i].value = 0
			case "use_environment", "user_site_directory":
				s.ints[i].value = 1
			}
		}
	}
	if len(s.argv) == 0 {
		s.argv = []string{in.Executable}
	}
	return s
}

// Initializes through PyInitConfig, every option addressed by name
func initFromInitConfig(s initSettings) error {
	cfg := pyInitConfigCreate()
	if cfg == 0 {
		return fmt.Errorf("PyInitConfig_Create returned NULL")
	}
	defer pyInitConfigFree(cfg)
	fail := func(what string) error {
		var msg uintptr
		pyInitConfigGetError(cfg, &msg)
		return fmt.Errorf("%s: %s", what, cString(msg))
	}
	for _, kv := range s.ints {
		if pyInitConfigSetInt(cfg, kv.name, kv.value) != 0 {
			return fail("PyInitConfig_SetInt " + kv.name)
		}
	}
	for _, kv := range s.strs {
		if pyInitConfigSetStr(cfg, kv.name, kv.value) != 0 {
			return fail("PyInitConfig_SetStr " + kv.name)
		}
	}
	items := make([]*byte, len(s.argv))
	keep := make([][]byte, len(s.argv))
	for i, a := range s.argv {
		keep[i] = append([]byte(a), 0)
		items[i] = &keep[i][0]
	}
	if pyInitConfigSetStrList(cfg, "argv", uintptr(len(items)), unsafe.Pointer(&items[0])) != 0 {
		return fail("PyInitConfig_SetStrList argv")
	}
	if pyInitializeFromInitConfig(cfg) != 0 {
		return fail("Py_InitializeFromInitConfig")
	}
	return nil
}

// Expected values PyConfig_InitIsolatedConfig leaves in place, checked to prove the parsed layout
var isolatedSentinels = map[string]int32{
	"_config_init": 3, "isolated": 1, "use_environment": 0, "install_signal_handlers": 0,
	"user_site_directory": 0, "configure_c_stdio": 0, "parse_argv": 0, "site_import": 1,
	"write_bytecode": 1, "buffered_stdio": 1, "pathconfig_warnings": 0, "_install_importlib": 1,
	"_init_main": 1, "safe_path": 1,
}

// Initializes through PyPreConfig and PyConfig placed at offsets parsed from the interpreter's header
func initFromLayout(in *Interpreter, s initSettings) error {
	preL, cfgL, err := configLayouts(in)
	if err != nil {
		return err
	}
	pre := pyMemRawMalloc(uintptr(preL.Size))
	cfg := pyMemRawMalloc(uintptr(cfgL.Size))
	if pre == 0 || cfg == 0 {
		return fmt.Errorf("PyMem_RawMalloc failed")
	}
	defer pyMemRawFree(pre)
	defer pyMemRawFree(cfg)
	clear(unsafe.Slice((*byte)(cptr(pre)), preL.Size))
	clear(unsafe.Slice((*byte)(cptr(cfg)), cfgL.Size))
	pyPreConfigInitIsolatedConfig(pre)
	for name, want := range map[string]int32{"_config_init": 3, "isolated": 1, "use_environment": 0} {
		if got := readInt(pre, preL, name); got != want {
			return fmt.Errorf("PyPreConfig layout mismatch: %s is %d, isolated init sets %d", name, got, want)
		}
	}
	for _, kv := range s.ints {
		_, inPre := preL.Fields[kv.name]
		_, inCfg := cfgL.Fields[kv.name]
		if !inPre && !inCfg {
			return fmt.Errorf("neither PyPreConfig nor PyConfig has %s", kv.name)
		}
		if inPre {
			writeInt(pre, preL, kv.name, int32(kv.value))
		}
	}
	if err := statusPreInitialize(pre).err("Py_PreInitialize"); err != nil {
		return err
	}
	pyConfigInitIsolatedConfig(cfg)
	defer pyConfigClear(cfg)
	for name, want := range isolatedSentinels {
		f, ok := cfgL.Fields[name]
		if !ok {
			return fmt.Errorf("PyConfig of %s lacks %s", in.Version(), name)
		}
		if got := *(*int32)(cptr(cfg + uintptr(f.Offset))); got != want {
			return fmt.Errorf("PyConfig layout mismatch: %s at %d is %d, isolated init sets %d", name, f.Offset, got, want)
		}
	}
	for _, kv := range s.ints {
		if _, ok := cfgL.Fields[kv.name]; ok {
			writeInt(cfg, cfgL, kv.name, int32(kv.value))
		}
	}
	for _, kv := range s.strs {
		f, ok := cfgL.Fields[kv.name]
		if !ok || f.Kind != kindWString {
			return fmt.Errorf("PyConfig has no string field %s", kv.name)
		}
		w := wideString(kv.value)
		if err := statusConfigSetString(cfg, cfg+uintptr(f.Offset), unsafe.Pointer(&w[0])).err("PyConfig_SetString " + kv.name); err != nil {
			return err
		}
	}
	wides := make([][]byte, len(s.argv))
	ptrs := make([]unsafe.Pointer, len(s.argv))
	for i, a := range s.argv {
		wides[i] = wideString(a)
		ptrs[i] = unsafe.Pointer(&wides[i][0])
	}
	if err := statusConfigSetArgv(cfg, len(ptrs), unsafe.Pointer(&ptrs[0])).err("PyConfig_SetArgv"); err != nil {
		return err
	}
	if err := statusInitializeFromConfig(cfg).err("Py_InitializeFromConfig"); err != nil {
		return err
	}
	return nil
}

func readInt(base uintptr, l *structLayout, name string) int32 {
	return *(*int32)(cptr(base + uintptr(l.Fields[name].Offset)))
}

func writeInt(base uintptr, l *structLayout, name string, v int32) {
	*(*int32)(cptr(base + uintptr(l.Fields[name].Offset))) = v
}
