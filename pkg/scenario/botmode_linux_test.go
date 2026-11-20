package scenario

import "testing"

func TestScenarioBotModeDMRunnerRunsTransports(t *testing.T) {
	s := Start(t, Setup{Home: map[string]string{
		"dm/piped.txt":   "line one\r\nline two\n",
		"dm/kept.txt":    "kept\n",
		"dm/status.txt":  "status\n",
		"dm/missing.txt": "missing\n",
		"dm/local.txt":   "local\n",
	}})
	entry := "__icebath_bot_mode_dm__"

	s.Run(entry, "--deliver").Exit(2)
	s.Run(entry, "--run-delivery", "inline", "${ICEBATH_HOME}/dm/kept.txt", "cat").Exit(2)
	s.Run(entry, "--run-delivery", "--author", "not json", "stdin", "${ICEBATH_HOME}/dm/kept.txt", "cat").Exit(2)
	if !s.Exists("dm/kept.txt") {
		s.Failf("a malformed runner argv removed the DM file")
	}

	if out := s.Run(entry, "--run-delivery", "stdin", "${ICEBATH_HOME}/dm/piped.txt", "cat").Exit(0).Stdout; out != "line one\nline two\n" {
		s.Failf("transport input %q", out)
	}
	if s.Exists("dm/piped.txt") {
		s.Failf("the DM file outlived its stdin transport")
	}
	s.Run(entry, "--run-delivery", "stdin", "${ICEBATH_HOME}/dm/status.txt", "sh", "-c", "cat >/dev/null; exit 7").Exit(7)
	absent := s.Run(entry, "--run-delivery", "stdin", "${ICEBATH_HOME}/dm/missing.txt", "icebath-no-such-transport").Exit(1)
	if Dig(absent.JSON(), "error") == "" || Dig(absent.JSON(), "reason") == nil {
		s.Failf("missing transport answer %q", absent.Stdout)
	}
	if s.Exists("dm/missing.txt") {
		s.Failf("the DM file outlived a transport that could not start")
	}

	local := s.Run(entry, "--run-delivery", "query-file", "${ICEBATH_HOME}/dm/local.txt", "icebath", "-p", "ghost").Exit(1)
	s.SameJSON(local.JSON(), `{"reason": "runtime_unavailable", "error": "No canonical Bot Chat authority; payload retained."}`)
	if !s.Exists("dm/local.txt") {
		s.Failf("a local delivery with no authority dropped its payload")
	}
}
