package provider

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

const blackholeURL = "http://10.0.0.9:30080/v1"

// Transport whose every request fails with the error, the dial timeout standing for a blackholed host
func failingProbeTransport(t *testing.T, err error) (context.Context, *fakeTransport) {
	t.Helper()
	ctx, ft, _ := metaTest(t)
	ft.handle = func(*http.Request) (*http.Response, error) { return nil, err }
	return ctx, ft
}

func readTimeoutErr() error { return &net.OpError{Op: "read", Net: "tcp", Err: &timeoutErr{}} }

// Records a settled server type verdict so detection answers without a request, as patching detect_local_server_type to None
func settleDetection(url, serverType string) {
	endpointProbePathMu.Lock()
	endpointProbePathCache[probeServerRoot(localhostToIPv4(normalizeBaseURL(url)))] = serverTypeVerdict{serverType: serverType, known: true, at: monotonicNow()}
	endpointProbePathMu.Unlock()
}

func TestBlackholeCacheKeyedOnHostPortNotPath(t *testing.T) {
	metaTest(t)
	noteEndpointBlackholed("http://10.0.0.9:30080")
	if !endpointBlackholed("http://10.0.0.9:30080/v1") || !endpointBlackholed("http://10.0.0.9:30080/api/v1") {
		t.Fatal("every probe path of one server shares the entry")
	}
}

func TestBlackholeCacheDifferentPortIsIndependent(t *testing.T) {
	metaTest(t)
	noteEndpointBlackholed("http://10.0.0.9:30080/v1")
	if endpointBlackholed("http://10.0.0.9:11434/v1") {
		t.Fatal("another port is a different server")
	}
}

func TestBlackholeCacheEntryExpiresAfterTtl(t *testing.T) {
	metaTest(t)
	noteEndpointBlackholed("http://10.0.0.9:30080/v1")
	endpointBlackholeMu.Lock()
	endpointBlackholeCache["10.0.0.9:30080"] = endpointBlackholeCache["10.0.0.9:30080"].Add(-endpointBlackholeTTL - time.Second)
	endpointBlackholeMu.Unlock()
	if endpointBlackholed("http://10.0.0.9:30080/v1") {
		t.Fatal("a recovered endpoint is probed again")
	}
}

func TestBlackholeCacheTtlZeroDisablesShortCircuit(t *testing.T) {
	metaTest(t)
	noteEndpointBlackholed("http://10.0.0.9:30080/v1")
	previous := endpointBlackholeTTL
	endpointBlackholeTTL = 0
	t.Cleanup(func() { endpointBlackholeTTL = previous })
	if endpointBlackholed("http://10.0.0.9:30080/v1") {
		t.Fatal("a zero TTL disables the short circuit")
	}
}

func TestDetectLocalServerTypeBlackholeConnectTimeoutAbortsWaterfallAfterOneProbe(t *testing.T) {
	ctx, ft := failingProbeTransport(t, dialTimeout())
	if DetectLocalServerType(ctx, blackholeURL, "") != "" {
		t.Fatal("a dead host has no server type")
	}
	if ft.count() != 1 {
		t.Fatalf("%d probes against a dead host", ft.count())
	}
	if !endpointBlackholed(blackholeURL) {
		t.Fatal("the connect timeout must blackhole the host")
	}
}

func TestDetectLocalServerTypeBlackholeSecondCallMakesNoRequestAtAll(t *testing.T) {
	ctx, ft := failingProbeTransport(t, dialTimeout())
	DetectLocalServerType(ctx, blackholeURL, "")
	first := ft.count()
	if DetectLocalServerType(ctx, blackholeURL, "") != "" || ft.count() != first {
		t.Fatalf("second detection probed again: %d -> %d", first, ft.count())
	}
}

func TestDetectLocalServerTypeBlackholeRefusedDoesNotBlackholeAndRunsFullWaterfall(t *testing.T) {
	ctx, ft := failingProbeTransport(t, errors.New("connection refused"))
	if DetectLocalServerType(ctx, blackholeURL, "") != "" {
		t.Fatal("a refused host has no server type")
	}
	if ft.count() <= 1 {
		t.Fatalf("refused must run the whole waterfall, got %d probes", ft.count())
	}
	if endpointBlackholed(blackholeURL) {
		t.Fatal("refused must not blackhole")
	}
}

func TestDetectLocalServerTypeBlackholeReadTimeoutDoesNotBlackhole(t *testing.T) {
	ctx, _ := failingProbeTransport(t, readTimeoutErr())
	DetectLocalServerType(ctx, blackholeURL, "")
	if endpointBlackholed(blackholeURL) {
		t.Fatal("a read timeout means the connection was accepted")
	}
}

func TestFetchEndpointModelMetadataBlackholeConnectTimeoutSkipsRemainingCandidates(t *testing.T) {
	ctx, ft := failingProbeTransport(t, dialTimeout())
	settleDetection(blackholeURL, "")
	if got := FetchEndpointModelMetadata(ctx, blackholeURL, "", false); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if ft.count() != 1 {
		t.Fatalf("%d candidate requests after a connect timeout", ft.count())
	}
	if !endpointBlackholed(blackholeURL) {
		t.Fatal("the timeout condemns the host")
	}
}

func TestFetchEndpointModelMetadataBlackholeRefusedTriesEveryCandidateAndDoesNotBlackhole(t *testing.T) {
	ctx, ft := failingProbeTransport(t, errors.New("refused"))
	settleDetection(blackholeURL, "")
	if got := FetchEndpointModelMetadata(ctx, blackholeURL, "", false); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if ft.count() != 2 {
		t.Fatalf("%d candidate requests, want the /v1 and bare candidates", ft.count())
	}
	if endpointBlackholed(blackholeURL) {
		t.Fatal("refused must not blackhole")
	}
}

func TestFetchEndpointModelMetadataBlackholeBlackholedEndpointIssuesNoRequest(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	settleDetection(blackholeURL, "")
	noteEndpointBlackholed(blackholeURL)
	if got := FetchEndpointModelMetadata(ctx, blackholeURL, "", true); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
	if ft.count() != 0 {
		t.Fatalf("%d requests to a blackholed endpoint", ft.count())
	}
}

func TestQueryOllamaApiShowBlackholeConnectTimeoutRecordsBlackhole(t *testing.T) {
	ctx, ft := failingProbeTransport(t, dialTimeout())
	if queryOllamaAPIShowUncached(ctx, "some-model", blackholeURL, "") != 0 {
		t.Fatal("a dead host has no context length")
	}
	if ft.count() != 1 || !endpointBlackholed(blackholeURL) {
		t.Fatalf("%d requests, blackholed %t", ft.count(), endpointBlackholed(blackholeURL))
	}
}

func TestQueryOllamaApiShowBlackholeBlackholedEndpointIssuesNoRequest(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	noteEndpointBlackholed(blackholeURL)
	if queryOllamaAPIShowUncached(ctx, "some-model", blackholeURL, "") != 0 || ft.count() != 0 {
		t.Fatalf("%d requests to a blackholed endpoint", ft.count())
	}
}

func TestQueryOllamaApiShowBlackholeReadTimeoutDoesNotBlackhole(t *testing.T) {
	ctx, _ := failingProbeTransport(t, readTimeoutErr())
	if queryOllamaAPIShowUncached(ctx, "some-model", blackholeURL, "") != 0 {
		t.Fatal("a slow host has no context length")
	}
	if endpointBlackholed(blackholeURL) {
		t.Fatal("a read timeout must not blackhole")
	}
}

func TestQueryLocalContextLengthBlackholeConnectTimeoutRecordsBlackhole(t *testing.T) {
	ctx, _ := failingProbeTransport(t, dialTimeout())
	settleDetection(blackholeURL, "")
	if queryLocalContextLengthUncached(ctx, "some-model", blackholeURL, "") != 0 {
		t.Fatal("a dead host has no context length")
	}
	if !endpointBlackholed(blackholeURL) {
		t.Fatal("the connect timeout must blackhole the host")
	}
}

func TestQueryLocalContextLengthBlackholeBlackholedEndpointSkipsDetectionAndRequests(t *testing.T) {
	ctx, ft, _ := metaTest(t)
	noteEndpointBlackholed(blackholeURL)
	if queryLocalContextLengthUncached(ctx, "some-model", blackholeURL, "") != 0 {
		t.Fatal("a blackholed host has no context length")
	}
	endpointProbePathMu.Lock()
	detected := len(endpointProbePathCache)
	endpointProbePathMu.Unlock()
	if ft.count() != 0 || detected != 0 {
		t.Fatalf("%d requests and %d detections behind the guard", ft.count(), detected)
	}
}

func TestQueryLocalContextLengthBlackholeReadTimeoutDoesNotBlackhole(t *testing.T) {
	ctx, _ := failingProbeTransport(t, readTimeoutErr())
	settleDetection(blackholeURL, "")
	if queryLocalContextLengthUncached(ctx, "some-model", blackholeURL, "") != 0 {
		t.Fatal("a slow host has no context length")
	}
	if endpointBlackholed(blackholeURL) {
		t.Fatal("a read timeout must not blackhole")
	}
}
