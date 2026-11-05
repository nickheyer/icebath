//go:build !windows

package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func lr3EnvEchoCmd(name string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", `printf %s "$`+name+`"`)
}

// Spawns the router as an owned child and idles until killed
func lr3OwnerRole(root string) {
	if _, err := SpawnServer(lr3HelperCmd(root, "router"), true); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Second)
	}
}

func TestOwnerSigkillReapsRouterTreeNotAnExternalControl(t *testing.T) {
	root := t.TempDir()
	log, err := os.Create(filepath.Join(root, "children.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var launchers []*exec.Cmd
	for _, role := range []string{"control", "owner"} {
		cmd := lr3HelperCmd(root, role)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		launchers = append(launchers, cmd)
	}
	ids := map[string]lr3Identity{}
	defer func() {
		for _, id := range ids {
			lr3Kill(id)
		}
		for _, l := range launchers {
			_ = l.Process.Kill()
			_ = l.Wait()
		}
	}()
	for _, role := range []string{"owner", "router", "grandchild", "control"} {
		ids[role] = lr3Read(t, filepath.Join(root, role+".json"))
	}
	if pgid, err := syscall.Getpgid(ids["grandchild"].PID); err != nil || pgid != ids["router"].PID {
		t.Fatalf("model child left the router's group: pgid %d err %v router %d", pgid, err, ids["router"].PID)
	}
	_ = launchers[1].Process.Kill()
	_ = launchers[1].Wait()
	if !lr3Wait(func() bool { return !lr3Alive(ids["router"]) && !lr3Alive(ids["grandchild"]) }, 15*time.Second) {
		t.Fatalf("router tree to die after kill -9 of its owner: %v", ids)
	}
	if !lr3Alive(ids["control"]) {
		t.Fatal("an unrelated process was killed")
	}
}
