package tools

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/nickheyer/icebath/pkg/internal/pyrequests"
)

// requests call with timeout=, every transport failure raised as the requests exception it maps to
func requestsSend(ctx context.Context, tm pyrequests.Timeout, method, target string, body []byte, header http.Header) (*sdkResponse, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = header.Clone()
	resp, err := pyrequests.Send(ctx, webHTTPTransport, tm, req)
	if err != nil {
		return nil, err
	}
	return &sdkResponse{status: resp.Status, header: resp.Header, body: resp.Body}, nil
}

// exa_py reporting_failover_connect_timeouts then typed_timeouts over one requests call
func exaTypedTimeouts(err error, tm pyrequests.Timeout, elapsed time.Duration) error {
	var re *pyrequests.Error
	if !errors.As(err, &re) {
		return err
	}
	if re.Class == "ConnectionError" && elapsed >= tm.Connect && re.Reason == "NewConnectionError" && !re.DNS {
		re = &pyrequests.Error{Class: "ConnectTimeout", Text: "Connect timed out. (connect timeout=" + tm.ConnectText + ") The last address tried failed with: " + re.Text, Reason: re.Reason, Err: re}
	}
	switch {
	case re.Class == "ConnectTimeout" || re.Class == "ReadTimeout", re.Class == "ConnectionError" && re.Reason == "ReadTimeoutError":
		return &pyrequests.Error{Class: "ExaTimeoutError", Text: re.Text, Reason: re.Reason, DNS: re.DNS, Err: re}
	}
	return re
}
