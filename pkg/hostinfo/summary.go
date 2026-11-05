package hostinfo

import (
	"regexp"
	"strings"
	"sync"
)

var nonNames = map[string]bool{
	"admin": true, "administrator": true, "default": true, "guest": true, "me": true, "owner": true, "root": true, "test": true, "user": true,
}

var handleChars = regexp.MustCompile(`[\d_@/\\]`)

// FullNameFrom keeps a real full name only, never a login handle
func FullNameFrom(login, full string) string {
	name := strings.Join(strings.Fields(full), " ")
	n := len([]rune(name))
	if n < 2 || n > 40 || nonNames[strings.ToLower(name)] || strings.EqualFold(name, login) {
		return ""
	}
	if handleChars.MatchString(name) || (name == strings.ToLower(name) && name != strings.ToUpper(name)) {
		return ""
	}
	return name
}

// LinuxLocaleFrom reads LANG from locale config text as a hyphenated language tag without encoding
func LinuxLocaleFrom(text string) string {
	for _, line := range strings.Split(text, "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == "LANG" && value != "" {
			value = strings.Trim(value, "\"'")
			if i := strings.Index(value, "."); i >= 0 {
				value = value[:i]
			}
			return strings.ReplaceAll(value, "_", "-")
		}
	}
	return ""
}

// System locale from its config file, the shell LANG deliberately not read
func linuxLocale() string {
	for _, path := range []string{"/etc/locale.conf", "/etc/default/locale"} {
		if text := readText(path, 65536); text != "" {
			if value := LinuxLocaleFrom(text); value != "" {
				return value
			}
		}
	}
	return ""
}

// MachineKind is Spark for an NVIDIA Spark, Mac, PC, or computer
func MachineKind(osFamily, nvidia string) string {
	if nvidia == "rtx_spark" || nvidia == "dgx_spark" {
		return "Spark"
	}
	switch osFamily {
	case "darwin":
		return "Mac"
	case "win32":
		return "PC"
	}
	return "computer"
}

// Summary is this machine and account as measured
type Summary struct {
	Machine        map[string]any
	MachineKind    string
	HasNvidiaGPU   bool
	NvidiaPlatform string
	Locale         string
	FullName       string
}

// AsMap is the summary in the wire shape the desktop reads
func (s Summary) AsMap() map[string]any {
	return map[string]any{"machine": s.Machine, "machine_kind": s.MachineKind, "has_nvidia_gpu": s.HasNvidiaGPU,
		"nvidia_platform": emptyAsNil(s.NvidiaPlatform), "locale": emptyAsNil(s.Locale), "full_name": emptyAsNil(s.FullName)}
}

// Nil for an empty string
func emptyAsNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

var (
	summaryOnce  sync.Once
	summaryValue Summary
)

// HostSummary measures the machine once, leaving out machine keys that were not measured
func HostSummary() Summary {
	summaryOnce.Do(func() { summaryValue = computeSummary() })
	return summaryValue
}

// Measures every summary field
func computeSummary() Summary {
	osFamily, arch, gpu, cpu := OSFamily(), NativeArch(), GPUClass(), CPUModel()
	ram := RAMTotalBytes()
	nvidia := NvidiaPlatform()
	machine := map[string]any{}
	put := func(key string, value any) {
		switch v := value.(type) {
		case string:
			if v != "" {
				machine[key] = v
			}
		case nil:
		default:
			machine[key] = value
		}
	}
	put("os_family", osFamily)
	put("os_release", osRelease())
	put("native_arch", arch)
	put("cpu_model", cpu)
	if ram > 0 {
		machine["ram_gb"] = int((float64(ram)/(1<<30))+0.5) + roundHalfEvenAdjust(float64(ram)/(1<<30))
	}
	if gpu != "unknown" {
		put("gpu_class", gpu)
	}
	put("vendor", CPUVendor())
	machine["wsl"] = IsWSL()
	machine["container"] = IsContainer()
	login, full := account()
	return Summary{Machine: machine, MachineKind: MachineKind(osFamily, nvidia), HasNvidiaGPU: gpu == "nvidia",
		NvidiaPlatform: nvidia, Locale: osLocale(), FullName: FullNameFrom(login, full)}
}

// Python round() is half to even, so an exact half above an odd integer rounds down by one
func roundHalfEvenAdjust(v float64) int {
	whole := float64(int(v))
	if v-whole == 0.5 && int(whole)%2 == 0 {
		return -1
	}
	return 0
}
