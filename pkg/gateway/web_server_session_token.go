package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/nickheyer/icebath/pkg/internal/pypath"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
)

// Session token from the env var, the file a file prefix names, or a fresh one per server start
func ResolveSessionToken() (string, error) {
	envToken := os.Getenv("ICEBATH_DASHBOARD_SESSION_TOKEN")
	if envToken == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			panic(err)
		}
		return base64.RawURLEncoding.EncodeToString(b), nil
	}
	if !strings.HasPrefix(envToken, "file:") {
		return envToken, nil
	}
	tokenFile := envToken[len("file:"):]
	raw, err := os.ReadFile(tokenFile)
	var text string
	if err == nil {
		text, err = pystr.DecodeUTF8Sig(raw)
	} else {
		err = fmt.Errorf("%s", pypath.OSErrorText(err))
	}
	if err != nil {
		return "", &DashboardExit{Code: 1, Msg: fmt.Sprintf(
			"ICEBATH_DASHBOARD_SESSION_TOKEN names an unreadable token file (%s): %s", tokenFile, err)}
	}
	token := pystr.Strip(text)
	if token == "" {
		return "", &DashboardExit{Code: 1, Msg: fmt.Sprintf(
			"ICEBATH_DASHBOARD_SESSION_TOKEN names an empty token file (%s)", tokenFile)}
	}
	return token, nil
}
