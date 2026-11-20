package installidentity

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
)

// Process cache the external cases reset between identities
var ProcessCache = processCache

// Started child process racing for the first identity under root, its stdout collected in out
func StartRace(root, minted, startBarrier, held string) (cmd *exec.Cmd, out *bytes.Buffer, err error) {
	cmd = exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), raceHelperEnv+"="+strings.Join([]string{root, minted, startBarrier, held}, "|"))
	out = &bytes.Buffer{}
	cmd.Stdout = out
	cmd.Stderr = os.Stderr
	return cmd, out, cmd.Start()
}
