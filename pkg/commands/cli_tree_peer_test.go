package commands

import (
	"slices"
	"testing"
)

// Short help, aliases and positional names with help of one peer subcommand
type peerCmdShape struct {
	short   string
	aliases []string
	pos     []posSpec
}

func TestBuildPeerParserMatchesTheArgparseTree(t *testing.T) {
	c := NewCLI()
	peer := c.find("peer")
	if peer == nil {
		t.Fatal("peer missing")
	}
	if peer.Short != "Bot-to-bot DMs across machines (peer Icebath gateways)" ||
		peer.Long != "Register other Icebath gateways as peers and message their agents. 'icebath peer dm <peer>[/<agent>] \"...\"' delivers into the remote agent's canonical Bot Chat over the peer's API server and prints the reply — the cross-machine twin of 'icebath -p <bot> chat'. The peer must run the api_server platform; its API_SERVER_KEY is stored locally as a credential in ~/.icebath/.env." {
		t.Fatalf("peer help %q %q", peer.Short, peer.Long)
	}
	if peer.Annotations["epilog"] != "Examples:\n  icebath peer add spark --url http://spark.lan:8377 --key <API_SERVER_KEY>\n  icebath peer list\n  icebath peer dm spark \"Message from 🤖 dixie (@dixie): disk status?\"\n  icebath peer dm spark/researcher \"...\"   # named profile on a multiplexed peer\n  icebath peer run spark --idempotency-key ticket-123 < long-task.txt\n  icebath peer status spark run_abc123\n  icebath peer stop spark run_abc123\n  icebath peer remove spark\n\nExit codes: 0 ok, 1 delivery/peer error, 2 usage error." {
		t.Fatalf("epilog %q", peer.Annotations["epilog"])
	}
	target := posSpec{Name: "target", Help: "<peer> or <peer>/<agent> (named profile on a multiplexed peer)"}
	message := posSpec{Name: "message", Optional: true, Help: "Message text (or stdin)"}
	runID := posSpec{Name: "run_id", Help: "Run ID returned by 'icebath peer run'"}
	for path, want := range map[string]peerCmdShape{
		"peer add":    {"Register (or update) a peer gateway", []string{"set"}, []posSpec{{Name: "name", Help: "Peer name (lowercase slug, e.g. spark, homelab)"}}},
		"peer list":   {"List registered peers", []string{"ls"}, nil},
		"peer remove": {"Remove a peer", []string{"rm"}, []posSpec{{Name: "name", Help: "Peer name"}}},
		"peer dm":     {"Message an agent on a peer gateway and print its reply", nil, []posSpec{target, message}},
		"peer run":    {"Start a long peer turn asynchronously and return its run ID", nil, []posSpec{target, message}},
		"peer status": {"Read the status and final output of an asynchronous peer run", nil, []posSpec{target, runID}},
		"peer stop":   {"Stop one asynchronous peer run without affecting another turn", nil, []posSpec{target, runID}},
	} {
		cmd := c.find(path)
		if cmd == nil {
			t.Fatalf("%s missing", path)
		}
		if cmd.Short != want.short || !slices.Equal(cmd.Aliases, want.aliases) {
			t.Fatalf("%s short %q aliases %v", path, cmd.Short, cmd.Aliases)
		}
		got := c.positionals[cmd]
		if len(got) != len(want.pos) {
			t.Fatalf("%s positionals %v", path, got)
		}
		for i := range got {
			if got[i].Name != want.pos[i].Name || got[i].Optional != want.pos[i].Optional || got[i].Help != want.pos[i].Help || got[i].Default != "" {
				t.Fatalf("%s positional %d %+v", path, i, got[i])
			}
		}
	}
	add := c.find("peer add")
	for name, help := range map[string]string{
		"url":  "Peer gateway base URL, e.g. http://spark.lan:8377",
		"key":  "The peer's API_SERVER_KEY (stored in ~/.icebath/.env)",
		"note": "Optional description",
	} {
		f := add.Flags().Lookup(name)
		if f == nil || f.Usage != help || f.DefValue != "" || f.Value.Type() != "string" {
			t.Fatalf("add --%s %+v", name, f)
		}
	}
	if req := add.Flags().Lookup("url").Annotations["cobra_annotation_bash_completion_one_required_flag"]; !slices.Equal(req, []string{"true"}) {
		t.Fatalf("--url not required %v", req)
	}
	for _, path := range []string{"peer dm", "peer run", "peer status", "peer stop"} {
		f := c.find(path).Flags().Lookup("json")
		if f == nil || f.Usage != "Emit a JSON result" || f.DefValue != "false" || f.Value.Type() != "bool" {
			t.Fatalf("%s --json %+v", path, f)
		}
		idem := c.find(path).Flags().Lookup("idempotency-key")
		if (path == "peer run") != (idem != nil) {
			t.Fatalf("%s --idempotency-key %+v", path, idem)
		}
	}
	if f := c.find("peer run").Flags().Lookup("idempotency-key"); f.Usage != "Stable retry key (generated when omitted)" || f.DefValue != "" {
		t.Fatalf("--idempotency-key %+v", f)
	}
}

func TestBuildPeerParserNamespacesUseTheArgparseDests(t *testing.T) {
	ns, err := bNamespace(t, "peer add", []string{"peer", "set", "spark", "--url", "http://spark.lan:8377", "--key", "k"})
	if err != nil || ns["name"] != "spark" || ns["url"] != "http://spark.lan:8377" || ns["key"] != "k" || ns["note"] != nil || ns["command"] != "peer" {
		t.Fatalf("add %v %v", ns, err)
	}
	if _, err := bNamespace(t, "peer add", []string{"peer", "add", "spark"}); err == nil {
		t.Fatal("--url is required")
	}
	ns, err = bNamespace(t, "peer dm", []string{"peer", "dm", "spark/researcher", "hi", "--json"})
	if err != nil || ns["target"] != "spark/researcher" || ns["message"] != "hi" || ns["json"] != true {
		t.Fatalf("dm %v %v", ns, err)
	}
	ns, err = bNamespace(t, "peer run", []string{"peer", "run", "spark", "--idempotency-key", "ticket-123"})
	if err != nil || ns["target"] != "spark" || ns["message"] != nil || ns["idempotency_key"] != "ticket-123" || ns["json"] != false {
		t.Fatalf("run %v %v", ns, err)
	}
	if _, present := ns["idempotency-key"]; present {
		t.Fatal("the dashed flag name leaked beside its dest")
	}
	ns, err = bNamespace(t, "peer stop", []string{"peer", "stop", "spark", "run_abc123"})
	if err != nil || ns["target"] != "spark" || ns["run_id"] != "run_abc123" || ns["json"] != false {
		t.Fatalf("stop %v %v", ns, err)
	}
	if _, err := bNamespace(t, "peer status", []string{"peer", "status", "spark"}); err == nil {
		t.Fatal("run_id is required")
	}
	ns, err = bNamespace(t, "peer remove", []string{"peer", "rm", "spark"})
	if err != nil || ns["name"] != "spark" {
		t.Fatalf("remove %v %v", ns, err)
	}
}
