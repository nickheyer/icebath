package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickheyer/icebath/pkg/config"
)

// Runs one icebath command line in process and returns its stdout, failing on an error
func peerGroupRun(t *testing.T, argv ...string) string {
	t.Helper()
	var err error
	out := captureStream(t, &os.Stdout, func() { err = newCLI().Execute(argv) })
	if err != nil {
		t.Fatalf("%v: %v", argv, err)
	}
	return out
}

func TestPeerGroupHandsTheTypedSubcommandToThePeerCommand(t *testing.T) {
	h, _ := leftCoreCLIHome(t)
	t.Setenv("ICEBATH_PEER_SPARK_KEY", "")
	os.Unsetenv("ICEBATH_PEER_SPARK_KEY")
	config.ResetCache()
	if out := peerGroupRun(t, "peer"); out != "No peers registered. Add one: icebath peer add <name> --url http://host:port --key <API_SERVER_KEY>\n" {
		t.Fatalf("bare peer %q", out)
	}
	want := "Peer 'spark' saved (http://spark.lan:8377/). No key given — set the peer's API_SERVER_KEY with:\n" +
		"  icebath peer add spark --url http://spark.lan:8377/ --key <key>\n" +
		"  (or add ICEBATH_PEER_SPARK_KEY=<key> to ~/.icebath/.env)\n"
	if out := peerGroupRun(t, "peer", "set", "Spark", "--url", "http://spark.lan:8377/", "--note", "lab box"); out != want {
		t.Fatalf("set %q", out)
	}
	raw, err := os.ReadFile(filepath.Join(h, "config.yaml"))
	if err != nil || !strings.Contains(string(raw), "bot_peers:") || !strings.Contains(string(raw), "url: http://spark.lan:8377\n") {
		t.Fatalf("config %q %v", raw, err)
	}
	if out := peerGroupRun(t, "peer", "ls"); out != "spark\thttp://spark.lan:8377\t[NO KEY (ICEBATH_PEER_SPARK_KEY unset)] — lab box\n" {
		t.Fatalf("ls %q", out)
	}
	if out := peerGroupRun(t, "peer", "rm", "spark"); out != "Peer 'spark' removed (its ICEBATH_PEER_SPARK_KEY entry in .env is kept; delete it manually if unused).\n" {
		t.Fatalf("rm %q", out)
	}
	if out := peerGroupRun(t, "peer", "list"); !strings.HasPrefix(out, "No peers registered.") {
		t.Fatalf("list after rm %q", out)
	}
}
