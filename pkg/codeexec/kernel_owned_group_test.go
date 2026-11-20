package codeexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
)

// Cell source spawning a subprocess that stays in the kernel's group and writing both pids to receipt
func groupCellCode(childArgv []string, receipt string) string {
	argv, _ := json.Marshal(childArgv)
	path, _ := json.Marshal(receipt)
	return fmt.Sprintf("import json, os, subprocess, sys, time\n"+
		"argv = [sys.executable if a == '@python' else a for a in json.loads(%q)]\n"+
		"c = subprocess.Popen(argv)\n"+
		"receipt = json.loads(%q)\n"+
		"open(receipt + '.tmp', 'w').write(json.dumps({'kernel': os.getpid(), 'cell_child': c.pid}))\n"+
		"os.replace(receipt + '.tmp', receipt)\n"+
		"time.sleep(120)\n", string(argv), string(path))
}

// Child mode owner that spawns a real kernel running a cell with a subprocess, then waits to be killed
func kernelOwnerEntry() {
	role := os.Getenv("CODEEXEC_KERNEL_OWNER")
	if role == "" {
		return
	}
	homeDir := os.Getenv("ICEBATH_HOME")
	rt := interpreter{Python: os.Getenv("CODEEXEC_KERNEL_PYTHON"), Prefix: os.Getenv("CODEEXEC_KERNEL_PREFIX")}
	userPython = func(context.Context, *tools.Env) (string, error) { return rt.Python, nil }
	env := &tools.Env{Home: homeDir, Config: config.Tree{}, Values: &sync.Map{}, Cwd: homeDir}
	k := newSessionKernel(kernelKey{owner: role})
	if err := k.spawn(context.Background(), env, rt.Python, "", nil, 1); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	receipt := filepath.Join(os.Getenv("CODEEXEC_OWNER_ROOT"), role+".json")
	cell, _ := json.Marshal(map[string]string{"id": "x",
		"code": groupCellCode([]string{"@python", "-c", "import time; time.sleep(120)"}, receipt)})
	_, _ = k.stdin.Write(append(cell, '\n'))
	time.Sleep(600 * time.Second)
	os.Exit(0)
}
