package pm

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Single letter ps style state of pid from proc stat on Linux or ps on macOS, false when unknowable
func processState(pid int) (string, bool) {
	switch runtime.GOOS {
	case "linux", "android":
		stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return "", false
		}
		commEnd := bytes.LastIndexByte(stat, ')')
		if commEnd < 0 || commEnd+2 >= len(stat) {
			return "", false
		}
		if c := stat[commEnd+2]; c < utf8.RuneSelf {
			return string(rune(c)), true
		}
		return "\ufffd", true
	case "darwin":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid))
		cmd.Stdin = os.Stdin
		out, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && (ctx.Err() != nil || !errors.As(err, &exit)) {
			return "", false
		}
		text := pystr.Strip(pystr.DecodeUTF8Replace(out))
		if text == "" {
			return "", false
		}
		r, _ := utf8.DecodeRuneInString(text)
		return string(r), true
	}
	return "", false
}
