package hostinfo

import (
	"math"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hardware part of the machine summary, a field left out of its JSON when not measured
type Machine struct {
	OSFamily   Family   `json:"os_family,omitempty"`
	OSRelease  string   `json:"os_release,omitempty"`
	NativeArch Arch     `json:"native_arch,omitempty"`
	CPUModel   string   `json:"cpu_model,omitempty"`
	RAMGB      *int     `json:"ram_gb,omitempty"`
	GPUClass   GPUClass `json:"gpu_class,omitempty"`
	Vendor     string   `json:"vendor,omitempty"`
	WSL        bool     `json:"wsl"`
	Container  bool     `json:"container"`
}

// This machine and its OS account for the desktop first run, empty strings where the OS gives none
type Summary struct {
	Machine        Machine
	MachineKind    string
	HasNvidiaGPU   bool
	NvidiaPlatform NvidiaPlatform
	Locale         string
	FullName       string
}

var nonNames = map[string]bool{
	"admin": true, "administrator": true, "default": true, "guest": true, "me": true, "owner": true, "root": true, "test": true, "user": true,
}

// This machine and its OS account, measured once
func (h *Host) Summary() Summary { return h.summary.value(func() Summary { return summarize(h) }) }

// Measures every summary field, a field the host could not answer left empty
func summarize(h *Host) Summary {
	nvidia := h.nvidiaPlatform()
	class := h.GPUClass()
	m := Machine{OSFamily: OSFamily, NativeArch: h.NativeArch(), CPUModel: h.processorModel(), Vendor: h.processorVendor(),
		WSL: h.WSL(), Container: h.Container().Inside()}
	if o, err := h.OS(); err == nil {
		m.OSRelease = o.releaseName()
	}
	if ram, err := h.RAMTotal(); err == nil {
		gb := int(math.RoundToEven(float64(ram) / (1 << 30)))
		m.RAMGB = &gb
	}
	if class != GPUUnknown {
		m.GPUClass = class
	}
	login, full := account()
	return Summary{Machine: m, MachineKind: machineKind(nvidia), HasNvidiaGPU: class == GPUNvidia, NvidiaPlatform: nvidia,
		Locale: hostLocale(), FullName: fullNameFrom(login, full)}
}

// Spark for an NVIDIA Spark, Mac, PC, or computer
func machineKind(nvidia NvidiaPlatform) string {
	switch {
	case nvidia == RTXSpark || nvidia == DGXSpark:
		return "Spark"
	case OSFamily == FamilyDarwin:
		return "Mac"
	case OSFamily == FamilyWindows:
		return "PC"
	}
	return "computer"
}

// Real full name only, never a login handle or a generic account name
func fullNameFrom(login, full string) string {
	name := strings.Join(strings.Fields(full), " ")
	lower := strings.ToLower(name)
	if n := utf8.RuneCountInString(name); n < 2 || n > 40 || nonNames[lower] || lower == strings.ToLower(login) {
		return ""
	}
	if strings.ContainsFunc(name, func(r rune) bool { return unicode.IsDigit(r) || strings.ContainsRune(`_@/\`, r) }) {
		return ""
	}
	if name == lower && name != strings.ToUpper(name) {
		return ""
	}
	return name
}
