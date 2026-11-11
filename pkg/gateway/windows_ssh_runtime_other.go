//go:build !windows

package gateway

// Win32 calls that all refuse off Windows, where the runtime has no trust boundary to offer
func newSSHWin32() *sshWin32 {
	return &sshWin32{
		available: func() error { return errWindowsSSHOnly },
		open: func(string, uint32, uint32, uint32, uint32) (sshHandle, error) {
			return 0, errWindowsSSHOnly
		},
		read:            func(sshHandle, int) ([]byte, error) { return nil, errWindowsSSHOnly },
		write:           func(sshHandle, []byte) error { return errWindowsSSHOnly },
		flush:           func(sshHandle) error { return errWindowsSSHOnly },
		close:           func(sshHandle) {},
		createDirectory: func(string) error { return errWindowsSSHOnly },
		moveFileEx:      func(string, string, uint32) error { return errWindowsSSHOnly },
		winerror:        func(error) (int, bool) { return 0, false },
		spawnDetached:   func([]string, []string, sshHandle, string) (int, error) { return 0, errWindowsSSHOnly },
	}
}
