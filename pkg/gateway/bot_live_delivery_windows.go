package gateway

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// Owner account of a path for the log line, never failing
func dirOwner(path string) string {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	name, domain, _, err := owner.LookupAccount("")
	if err != nil {
		return fmt.Sprintf("unknown (%s)", err)
	}
	if domain != "" {
		return domain + `\` + name
	}
	return name
}

// A foreign Windows ACL denying attribute writes never refuses a mailbox this process can still use
func chmodRefusesMailbox(root string) bool { return false }
