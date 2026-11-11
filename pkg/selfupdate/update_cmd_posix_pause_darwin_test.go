//go:build darwin

package selfupdate

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickheyer/icebath/pkg/gateway"
)

func TestALaunchdJobIsPausedThroughLaunchdAndRestartedThroughIt(t *testing.T) {
	ctx := context.Background()
	label := fmt.Sprintf("ai.icebath.p2probe-%d", os.Getpid())
	plist := filepath.Join(t.TempDir(), label+".plist")
	body := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n" +
		"<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n" +
		"<plist version=\"1.0\">\n<dict>\n\t<key>KeepAlive</key>\n\t<true/>\n\t<key>Label</key>\n\t<string>" + label + "</string>\n" +
		"\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/bin/sleep</string>\n\t\t<string>600</string>\n\t</array>\n" +
		"\t<key>RunAtLoad</key>\n\t<true/>\n</dict>\n</plist>\n"
	if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	if exec.Command("launchctl", "print", domain).Run() != nil {
		domain = fmt.Sprintf("user/%d", os.Getuid())
	}
	pid := func() int {
		_, running, ok, _ := gateway.LaunchdPrintServicePID(ctx, domain, label)
		if !ok {
			return 0
		}
		return running
	}
	if err := exec.Command("launchctl", "bootstrap", domain, plist).Run(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exec.Command("launchctl", "bootout", domain+"/"+label).Run() }()
	deadline := time.Now().Add(15 * time.Second)
	for pid() == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	first := pid()
	if first == 0 {
		t.Fatal("premise: launchd never started the probe job")
	}
	job := map[string]any{"kind": "launchd", "label": label, "domain": domain, "plist": plist, "pid": first}
	if err := stopJob(ctx, job, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)
	if alive(first, nil) || pid() != 0 {
		t.Fatal("the paused job is running again under launchd")
	}
	if err := startJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	if now := pid(); now == 0 || now == first {
		t.Fatal("the restart did not bring the job back under launchd")
	}
}
