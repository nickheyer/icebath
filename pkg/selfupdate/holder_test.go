package selfupdate

import "testing"

func TestHolderSubcommandParsesSubcommand(t *testing.T) {
	for cmdline, want := range map[string]string{
		`C:\x\venv\Scripts\python.exe -m icebath_cli.main serve --host 127.0.0.1`: "serve",
		`C:\x\venv\Scripts\python.exe -m icebath_cli.main dashboard`:              "dashboard",
		`python.exe -m icebath_cli.main gateway run`:                              "gateway",
		`python -m icebath_cli.main --profile serve gateway run`:                  "gateway",
		`python -m icebath_cli.main -p work serve`:                                "serve",
		`python -m icebath_cli.main kanban --preserve-cache`:                      "kanban",
		`python -m icebath_cli.main --reasoning high serve`:                       "serve",
		`python -m icebath_cli.main -m dashboard serve`:                           "serve",
		`python -m icebath_cli.main -t browser,files gateway run`:                 "gateway",
		`python -m icebath_cli.main --model=dashboard serve`:                      "serve",
		`python -m icebath_cli.main -c mysession serve`:                           "serve",
		`C:\bin\icebath.exe dashboard`:                                            "dashboard",
		`/usr/local/bin/icebath serve`:                                            "serve",
		`/opt/icebath/icebath --profile work dashboard --port 9119`:               "dashboard",
		`python -c import time; time.sleep(3)`:                                    "",
		`python -m icebath_cli.main`:                                              "",
	} {
		got, ok := hermesHolderSubcommand(cmdline)
		if got != want || ok != (want != "") {
			t.Errorf("%q = %q %v want %q", cmdline, got, ok, want)
		}
	}
}

func TestHolderRuntimeHostMatchesServeDashboardAndGateways(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"/usr/local/bin/icebath serve":              true,
		"/usr/local/bin/icebath dashboard":          true,
		"/usr/local/bin/icebath gateway run":        true,
		"/usr/local/bin/icebath kanban --serve":     false,
		"/usr/local/bin/icebath update":             false,
		"/usr/bin/vim notes-about-icebath-serve.md": false,
	} {
		if got := isRuntimeHost([]string{cmdline}); got != want {
			t.Errorf("%q = %v", cmdline, got)
		}
	}
}
