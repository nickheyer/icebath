//go:build windows

package gateway

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Records live under the user's own state root on Windows, where mode bits set no ACLs
func hostRecordIsOwn(string) bool { return true }

func tightenHostStateDir(string) {}

const (
	openReparsePoint           = 0x00200000
	accessAllowedObjectAceType = 5
)

// Owner and SYSTEM only protected security attributes for a private credential file
func privateSecurityAttributes() (*windows.SecurityAttributes, string, error) {
	owner, err := currentUserSID()
	if err != nil {
		return nil, "", err
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("O:%sD:P(A;;FA;;;%s)(A;;FA;;;SY)", owner, owner))
	if err != nil {
		return nil, "", err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	return sa, owner, nil
}

func currentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

// Refuses a handle whose owner or allow entries name anyone but this user and SYSTEM
func verifyPrivateSecurity(handle windows.Handle, owner string) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	allowed := map[string]bool{owner: true, "S-1-5-18": true}
	sid, _, err := sd.Owner()
	if err != nil || sid == nil || !allowed[sid.String()] {
		return errors.New("Windows SSH runtime object has the wrong owner")
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return errors.New("Windows SSH runtime object has a null DACL")
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return err
		}
		switch ace.Header.AceType {
		case windows.ACCESS_ALLOWED_ACE_TYPE, accessAllowedObjectAceType, 9, 11:
		default:
			continue
		}
		if ace.Mask == 0 {
			continue
		}
		aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !allowed[aceSID.String()] {
			return errors.New("Windows SSH runtime object has a permissive DACL")
		}
	}
	return nil
}

// Replaces path through the SSH runtime writer with an owner and SYSTEM only protected DACL file
func writePrivateText(path, text string) error {
	return WritePrivateFile(path, []byte(text))
}

var hostSignalInstalled bool

// Clears every registered role on a console close, logoff or shutdown before the OS ends the process
func installHostSignalCleanup() {
	if hostSignalInstalled {
		return
	}
	hostSignalInstalled = true
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		HostRendezvousAtExit()
		signal.Stop(ch)
	}()
}
