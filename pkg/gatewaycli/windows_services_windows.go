package gatewaycli

import (
	"errors"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
)

func init() { scmEnumerate = enumerateSCMServices }

// SCM query error under the exception class psutil raises for it
func scmQueryFailure(err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case windows.ERROR_PRIVILEGE_NOT_HELD:
			return &scmQueryError{Class: "AccessDenied", Err: err}
		case windows.ERROR_INVALID_NAME, windows.ERROR_SERVICE_DOES_NOT_EXIST:
			return &scmQueryError{Class: "NoSuchProcess", Err: err}
		}
		if pypath.OSErrorClass(err) == "PermissionError" {
			return &scmQueryError{Class: "AccessDenied", Err: err}
		}
	}
	return err
}

// One Win32 service whose configuration and status are read through query only handles
type windowsSCMService struct{ name string }

func (s windowsSCMService) Name() (string, error) { return s.name, nil }

// Runs fn with a handle to the service opened with only the given access right
func (s windowsSCMService) withHandle(access uint32, fn func(windows.Handle) error) error {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return scmQueryFailure(err)
	}
	defer windows.CloseServiceHandle(manager)
	name, err := windows.UTF16PtrFromString(s.name)
	if err != nil {
		return err
	}
	handle, err := windows.OpenService(manager, name, access)
	if err != nil {
		return scmQueryFailure(err)
	}
	defer windows.CloseServiceHandle(handle)
	if err := fn(handle); err != nil {
		return scmQueryFailure(err)
	}
	return nil
}

// Binary path of the service from its configuration
func (s windowsSCMService) BinPath() (string, error) {
	binpath := ""
	err := s.withHandle(windows.SERVICE_QUERY_CONFIG, func(handle windows.Handle) error {
		var needed uint32
		err := windows.QueryServiceConfig(handle, nil, 0, &needed)
		if err != nil && !errors.Is(err, windows.ERROR_INSUFFICIENT_BUFFER) {
			return err
		}
		buf := make([]byte, needed)
		config := (*windows.QUERY_SERVICE_CONFIG)(unsafe.Pointer(&buf[0]))
		if err := windows.QueryServiceConfig(handle, config, needed, &needed); err != nil {
			return err
		}
		binpath = windows.UTF16PtrToString(config.BinaryPathName)
		return nil
	})
	return binpath, err
}

// Process status record of the service
func (s windowsSCMService) queryStatus() (windows.SERVICE_STATUS_PROCESS, error) {
	var status windows.SERVICE_STATUS_PROCESS
	err := s.withHandle(windows.SERVICE_QUERY_STATUS, func(handle windows.Handle) error {
		var needed uint32
		return windows.QueryServiceStatusEx(handle, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed)
	})
	return status, err
}

// State of the service in the words psutil reports
func (s windowsSCMService) Status() (string, error) {
	status, err := s.queryStatus()
	if err != nil {
		return "", err
	}
	switch status.CurrentState {
	case windows.SERVICE_CONTINUE_PENDING:
		return "continue_pending", nil
	case windows.SERVICE_PAUSE_PENDING:
		return "pause_pending", nil
	case windows.SERVICE_PAUSED:
		return "paused", nil
	case windows.SERVICE_RUNNING:
		return "running", nil
	case windows.SERVICE_START_PENDING:
		return "start_pending", nil
	case windows.SERVICE_STOP_PENDING:
		return "stop_pending", nil
	case windows.SERVICE_STOPPED:
		return "stopped", nil
	}
	return "unknown", nil
}

// Process id of the service, zero when it has none
func (s windowsSCMService) PID() (int, error) {
	status, err := s.queryStatus()
	if err != nil {
		return 0, err
	}
	return int(status.ProcessId), nil
}

// Every Win32 service the SCM knows in any state
func enumerateSCMServices() ([]scmService, error) {
	manager, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return nil, scmQueryFailure(err)
	}
	defer windows.CloseServiceHandle(manager)
	var buf []byte
	var needed, returned uint32
	for {
		var first *byte
		if len(buf) > 0 {
			first = &buf[0]
		}
		err := windows.EnumServicesStatusEx(manager, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL,
			first, uint32(len(buf)), &needed, &returned, nil, nil)
		if err == nil {
			break
		}
		if !errors.Is(err, windows.ERROR_MORE_DATA) || needed == 0 {
			return nil, scmQueryFailure(err)
		}
		buf = make([]byte, uint32(len(buf))+needed)
	}
	if returned == 0 {
		return nil, nil
	}
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buf[0])), returned)
	services := make([]scmService, 0, returned)
	for _, entry := range entries {
		services = append(services, windowsSCMService{name: windows.UTF16PtrToString(entry.ServiceName)})
	}
	return services, nil
}
