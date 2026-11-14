package media

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
)

// ImportError of a managed environment that has no pilk module
var ErrPilkMissing = errors.New("No module named 'pilk'")

const silkToWAVRateScript = `import json, sys
try:
    import pilk
    pilk.silk_to_wav(sys.argv[1], sys.argv[2], rate=int(sys.argv[3]))
except BaseException as exc:
    sys.stdout.write(json.dumps({"error": str(exc)}))
    sys.exit(1)
`

// Decodes a SILK file to WAV at the sample rate with pilk in the managed interpreter, ErrPilkMissing without pilk
func SilkToWAVAtRate(ctx context.Context, src, dst string, rate int) error {
	if !safeFindSpec("pilk") {
		return ErrPilkMissing
	}
	python, err := managedPythonCommand(ctx)
	if err != nil {
		return err
	}
	stdout, _, err := sttRunQuiet(append(append([]string(nil), python...), "-c", silkToWAVRateScript, src, dst, strconv.Itoa(rate)), 300, nil)
	if err == nil {
		return nil
	}
	var pe *sttProcessError
	if errors.As(err, &pe) {
		var reply struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(stdout), &reply) == nil {
			return &sttPythonError{msg: reply.Error}
		}
	}
	return err
}
