//go:build windows

package tools

import (
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
)

// File version of the fixed file info resource as major.minor.build.revision
func platformPEVersion(path string) (observation[string], error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil || size == 0 {
		return observation[string]{State: checkUnavailable, Detail: "no version resource"}, nil
	}
	buf := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&buf[0])); err != nil {
		return observation[string]{State: checkError, Detail: "GetFileVersionInfoW failed"}, nil
	}
	var ptr unsafe.Pointer
	var length uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&buf[0]), `\`, unsafe.Pointer(&ptr), &length); err != nil || ptr == nil {
		return observation[string]{State: checkUnavailable, Detail: "no fixed file info"}, nil
	}
	info := (*windows.VS_FIXEDFILEINFO)(ptr)
	ms, ls := info.FileVersionMS, info.FileVersionLS
	v := fmt.Sprintf("%d.%d.%d.%d", ms>>16, ms&0xFFFF, ls>>16, ls&0xFFFF)
	return observation[string]{State: checkPresent, Value: &v}, nil
}

// str() of a registry value as winreg.QueryValueEx returns it
func platformRegistryValueStr(k registry.Key, name string) (string, bool) {
	_, valType, err := k.GetValue(name, nil)
	if err != nil {
		return "", false
	}
	switch valType {
	case registry.SZ, registry.EXPAND_SZ:
		s, _, err := k.GetStringValue(name)
		return s, err == nil
	case registry.DWORD, registry.QWORD:
		n, _, err := k.GetIntegerValue(name)
		return strconv.FormatUint(n, 10), err == nil
	case registry.MULTI_SZ:
		items, _, err := k.GetStringsValue(name)
		if err != nil {
			return "", false
		}
		parts := make([]string, len(items))
		for i, s := range items {
			parts[i] = pyrepr.StrRepr(s)
		}
		return "[" + strings.Join(parts, ", ") + "]", true
	}
	raw, _, err := k.GetBinaryValue(name)
	if err != nil {
		return "", false
	}
	if len(raw) == 0 {
		return "None", true
	}
	return pyrepr.BytesRepr(raw), true
}

// Uninstall entries whose DisplayName starts with the prefix, machine wide before per user
func platformUninstallEntries(prefix string) []map[string]string {
	roots := []struct {
		hive registry.Key
		path string
	}{
		{registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.LOCAL_MACHINE, `SOFTWARE\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall`},
		{registry.CURRENT_USER, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall`},
	}
	var out []map[string]string
	for _, root := range roots {
		key, err := registry.OpenKey(root.hive, root.path, registry.READ)
		if err != nil {
			continue
		}
		names, _ := key.ReadSubKeyNames(-1)
		for _, name := range names {
			values := map[string]string{}
			if entry, err := registry.OpenKey(key, name, registry.QUERY_VALUE); err == nil {
				for _, valueName := range []string{"DisplayName", "DisplayVersion", "InstallLocation"} {
					if s, ok := platformRegistryValueStr(entry, valueName); ok {
						values[valueName] = s
					}
				}
				entry.Close()
			}
			if strings.HasPrefix(values["DisplayName"], prefix) {
				out = append(out, values)
			}
		}
		key.Close()
	}
	return out
}

// Liveness of a runtime file pid through a limited query handle, the DWORD masking larger ints
func platformPIDAlive(pid *big.Int) (observation[bool], error) {
	if pid == nil || pid.Sign() <= 0 {
		return observation[bool]{State: checkUnavailable, Detail: "no pid in runtime file"}, nil
	}
	masked := uint32(new(big.Int).And(pid, big.NewInt(0xFFFFFFFF)).Uint64())
	t, f := true, false
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, masked)
	if err != nil || handle == 0 {
		return observation[bool]{State: checkAbsent, Value: &f}, nil
	}
	windows.CloseHandle(handle)
	return observation[bool]{State: checkPresent, Value: &t}, nil
}
