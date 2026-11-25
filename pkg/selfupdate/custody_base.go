package selfupdate

import (
	"os/exec"

	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

func init() {
	pm.SetUpdateLockHooks(pm.UpdateLockHooks{
		BindChild: func(cmd *exec.Cmd) (string, bool) {
			if err := updaterecord.BindChildToUpdateTree(cmd.Process.Pid); err != nil {
				return errchain.OSText(err), true
			}
			return "", false
		},
		ResumeSuspended: func(cmd *exec.Cmd) error {
			if err := updaterecord.ResumeSuspendedChild(cmd.Process); err != nil {
				return &pm.ClassError{Class: "OSError", Message: err.Error()}
			}
			return nil
		},
		Held: updaterecord.CheckoutHeld,
		CheckoutLockFDs: func(root string) ([]int, error) {
			files, err := updaterecord.CheckoutLockFiles(root)
			fds := make([]int, 0, len(files))
			for _, f := range files {
				fds = append(fds, int(f.Fd()))
			}
			return fds, err
		},
	})
}
