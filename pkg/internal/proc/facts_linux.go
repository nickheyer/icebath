package proc

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Linux reads every process cwd from procfs
const CwdAvailable = true

// Working directory of pid like psutil, a deleted suffix dropped unless that literal path exists
func Cwd(pid int) (string, error) {
	target, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
	if err != nil {
		return "", err
	}
	target, _, _ = strings.Cut(target, "\x00")
	if strings.HasSuffix(target, " (deleted)") {
		if _, err := os.Stat(target); err != nil {
			target = target[:len(target)-len(" (deleted)")]
		}
	}
	return target, nil
}

// Real uid of pid from the Uid line of its status file
func RealUID(pid int) (int, error) {
	data, err := procFile(pid, "status")
	if err != nil {
		return 0, err
	}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if rest, ok := bytes.CutPrefix(line, []byte("Uid:")); ok {
			if fields := bytes.Fields(rest); len(fields) > 0 {
				return strconv.Atoi(string(fields[0]))
			}
		}
	}
	return 0, errors.New("no Uid line in /proc status")
}

// Kernel name of pid between the first open and last close paren of its stat line
func comm(pid int) (string, error) {
	data, err := procFile(pid, "stat")
	if err != nil {
		return "", err
	}
	start, end := bytes.IndexByte(data, '('), bytes.LastIndexByte(data, ')')
	if start < 0 || end < start {
		return "", errors.New("malformed /proc stat line")
	}
	return pystr.SurrogateEscapeDecode(data[start+1 : end]), nil
}
