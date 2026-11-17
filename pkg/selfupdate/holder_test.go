package selfupdate

import "testing"

func TestHolderSubcommandParsesSubcommand(t *testing.T) {
	for cmdline, want := range map[string]string{
		`C:\x\venv\Scripts\python.exe -m icebath_cli.main cron --list`: "cron",
		`C:\x\venv\Scripts\python.exe -m icebath_cli.main chat`:        "chat",
		`python.exe -m icebath_cli.main gateway run`:                   "gateway",
		`python -m icebath_cli.main --profile chat gateway run`:        "gateway",
		`python -m icebath_cli.main -p work cron`:                      "cron",
		`python -m icebath_cli.main kanban --preserve-cache`:           "kanban",
		`python -m icebath_cli.main --reasoning high chat`:             "chat",
		`python -m icebath_cli.main -m cron chat`:                      "chat",
		`python -m icebath_cli.main -t browser,files gateway run`:      "gateway",
		`python -m icebath_cli.main --model=cron chat`:                 "chat",
		`python -m icebath_cli.main -c mysession chat`:                 "chat",
		`C:\bin\icebath.exe update`:                                    "update",
		`/usr/local/bin/icebath chat`:                                  "chat",
		`/opt/icebath/icebath --profile work gateway run --replace`:    "gateway",
		`python -c import time; time.sleep(3)`:                         "",
		`python -m icebath_cli.main`:                                   "",
	} {
		got, ok := hermesHolderSubcommand(cmdline)
		if got != want || ok != (want != "") {
			t.Errorf("%q = %q %v want %q", cmdline, got, ok, want)
		}
	}
}

func TestHolderRuntimeHostMatchesOnlyGateways(t *testing.T) {
	for cmdline, want := range map[string]bool{
		"/usr/local/bin/icebath gateway run":    true,
		"/usr/local/bin/icebath chat":           false,
		"/usr/local/bin/icebath kanban --serve": false,
		"/usr/local/bin/icebath update":         false,
		"/usr/bin/vim notes-about-icebath.md":   false,
	} {
		if got := isRuntimeHost([]string{cmdline}); got != want {
			t.Errorf("%q = %v", cmdline, got)
		}
	}
}
