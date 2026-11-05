package proc

import (
	"golang.org/x/sys/windows"
)

// Account owning pid as DOMAIN\user from its process token, as psutil reports
func Username(pid int) (string, error) {
	h, err := OpenProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	var tok windows.Token
	if err := windows.OpenProcessToken(h, windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	tu, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	account, domain, _, err := tu.User.Sid.LookupAccount("")
	if err != nil {
		return "", err
	}
	return domain + `\` + account, nil
}
