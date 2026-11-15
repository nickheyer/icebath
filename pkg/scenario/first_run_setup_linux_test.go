package scenario

import (
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Terminal modes of the scenario terminal as the program on it left them
func (tm *Term) modes() unix.Termios {
	tm.s.t.Helper()
	state, err := unix.IoctlGetTermios(int(tm.master.Fd()), unix.TCGETS)
	if err != nil {
		tm.s.t.Fatal(err)
	}
	return *state
}

// Reports the line discipline a shell expects, canonical input with echo, signals and output processing
func cookedModes(m unix.Termios) bool {
	return m.Lflag&unix.ICANON != 0 && m.Lflag&unix.ECHO != 0 && m.Lflag&unix.ISIG != 0 && m.Oflag&unix.OPOST != 0
}

// A first launch opens setup at once, the left arrow steps back with the terminal usable, leaving keeps saved answers
func TestScenarioFirstRunSetupBackStepKeepsTheTerminalUsable(t *testing.T) {
	s := Start(t, Setup{Bare: true})
	tm := s.Terminal()
	tm.Await(`● How would you like to set up Icebath\?`)
	if strings.Contains(tm.Text(), "Run setup now?") {
		s.Failf("the first launch still asks before opening setup\n%s", clip(tm.Text()))
	}
	tm.Send("\x1b[B")
	tm.Await(`❯ Full setup`)
	tm.Send("\r")
	tm.Await(`1 / 4  Model & Provider`)
	tm.Await(`● Select provider`)
	tm.Send("/anthropic")
	tm.Await(`❯ Anthropic`)
	tm.Send("\r")
	tm.Await(`● Choose authentication method`)
	tm.Await(`← back`)

	tm.Send("\x1b[D")
	tm.Await(`Returning to the previous choice in Model & Provider\.\.\.`)
	tm.Await(`● Select provider`)
	if cookedModes(tm.modes()) {
		s.Failf("the provider list after the back step is not reading keys itself\n%s", clip(tm.Text()))
	}
	tm.Send("/anthropic")
	tm.Await(`❯ Anthropic`)
	tm.Send("\r")
	tm.Await(`● Choose authentication method`)
	tm.Send("\x1b[B")
	tm.Await(`❯ Anthropic API key`)
	tm.Send("\r")
	tm.Await(`● API key \(sk-ant-\.\.\.\)`)
	tm.Send("sk-ant-api03-scenario-key")
	tm.Await(`25 characters`)
	if strings.Contains(tm.Text(), "scenario-key") {
		s.Failf("the typed key is readable on the terminal\n%s", clip(tm.Text()))
	}
	tm.Send("\r")
	tm.Await(`API key saved\.`)
	tm.Await(`● Select default model`)
	tm.Send("\r")
	tm.Await(`Default model set to: \S+ \(via Anthropic\)`)
	tm.Await(`● Select reasoning effort`)

	tm.Send("\x1b")
	tm.Await(`press esc again to cancel`)
	tm.Send("\x1b")
	tm.Await(`Setup cancelled\. Remaining sections were not changed\.`)
	if code := tm.Wait(); code != 0 {
		s.Failf("leaving setup with a provider saved exited %d\n%s", code, clip(tm.Text()))
	}
	if !cookedModes(tm.modes()) {
		s.Failf("icebath left the terminal out of its normal modes: %+v\n%s", tm.modes(), clip(tm.Text()))
	}
	if !strings.Contains(s.Read(".env"), "ANTHROPIC_API_KEY=sk-ant-api03-scenario-key") {
		s.Failf("the key entered before leaving was not kept\n%s", s.Read(".env"))
	}
	s.HasJSON(s.YAML("config.yaml"), `{"model": {"provider": "anthropic"}}`)
}

// Leaving the first launch setup before any provider is saved ends with the hint and a failing exit
func TestScenarioFirstRunSetupLeftEarlyExitsWithTheHint(t *testing.T) {
	s := Start(t, Setup{Bare: true})
	tm := s.Terminal()
	tm.Await(`● How would you like to set up Icebath\?`)
	tm.Send("\x1b")
	tm.Await(`press esc again to cancel`)
	tm.Send("\x1b")
	tm.Await(`You can run 'icebath setup' at any time to configure\.`)
	if code := tm.Wait(); code != 1 {
		s.Failf("leaving setup with nothing configured exited %d\n%s", code, clip(tm.Text()))
	}
	if !cookedModes(tm.modes()) {
		s.Failf("icebath left the terminal out of its normal modes: %+v", tm.modes())
	}
}
