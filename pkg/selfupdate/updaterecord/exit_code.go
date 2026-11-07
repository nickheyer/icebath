package updaterecord

import (
	"context"
	"math/big"
	"os"
	"path/filepath"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pycodecs"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/pm"
)

const gatewayExitCodeName = ".update_exit_code"

// File the gateway update watcher polls for the finished update's exit status
func GatewayExitCodePath(h string) string { return filepath.Join(h, gatewayExitCodeName) }

// Exit status the update published under the home, an empty file reading as failure exit 1
func ReadGatewayExitCode(h string) (*big.Int, error) {
	data, err := os.ReadFile(GatewayExitCodePath(h))
	if err != nil {
		return nil, err
	}
	text, err := pycodecs.Decode(data, "utf-8-sig")
	if err != nil {
		return nil, err
	}
	text = pystr.Strip(text)
	if text == "" {
		text = "1"
	}
	n, ok := pyint.Parse(text)
	if !ok {
		return nil, &pyint.Error{Msg: "invalid literal for int() with base 10: " + pyrepr.Repr(text)}
	}
	return n, nil
}

// Publishes 0 or 1 atomically for the gateway watcher, a run already committed as success never reads failed
func WriteGatewayUpdateExitCode(ctx context.Context, ok bool) {
	ok = ok || CommittedSuccess(ctx)
	body := []byte("1")
	if ok {
		body = []byte("0")
	}
	_ = pm.DurableWriteBytes(GatewayExitCodePath(home.Dir(ctx)), body)
}
