package gatewayrun

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/nickheyer/icebath/pkg/gateway"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyshutil"
	shellutil "github.com/nickheyer/icebath/pkg/utils/shell"
)

// Spawns icebath update for the gateway detached so it outlives the restart, output and exit code in files
func spawnDetachedUpdate(icebathCmd []string, outputPath, exitCodePath string) error {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(exe, gateway.GatewayUpdateHelperEntry, outputPath, exitCodePath, exe, "update", "--gateway")
		cmd.SysProcAttr = home.DetachAttr(true)
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	quoted := make([]string, len(icebathCmd))
	for i, part := range icebathCmd {
		quoted[i] = shellutil.Quote(part)
	}
	updateCmd := fmt.Sprintf("PYTHONUNBUFFERED=1 %s update --gateway > %s 2>&1; "+
		"rc=$?; printf '%%s' \"$rc\" > %s && mv -f %s %s", strings.Join(quoted, " "), shellutil.Quote(outputPath),
		shellutil.Quote(exitCodePath+".tmp"), shellutil.Quote(exitCodePath+".tmp"), shellutil.Quote(exitCodePath))
	argv := []string{"bash", "-c", updateCmd}
	if setsid, ok := pyshutil.Which("setsid", nil); ok && setsid != "" {
		argv = append([]string{setsid}, argv...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = home.DetachAttr(false)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
