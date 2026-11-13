package connections

import (
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// AuthHost is the shell side of gateway sign-in, the system browser and the native sign-in windows
type AuthHost interface {
	ExternalBrowser
	// OpenLoginWindow creates a sign-in window over the partition's own webview storage
	OpenLoginWindow(spec LoginWindowSpec) (LoginWindow, error)
	// CanOpenWindows reports whether the app is ready to create windows
	CanOpenWindows() bool
}

// authSlice is the sign-in state of the connections service, built on first use
type authSlice struct {
	once  sync.Once
	inner *authInner
}

type authInner struct {
	mu                sync.Mutex
	host              AuthHost
	wsHeaders         *remoteWsHeaderStore
	headerSources     []RemoteHeaderSource
	headerConfigKey   *connectionConfig
	headerRegistryKey *ConnectionRegistry
	native            nativeTokenCache
	tokens            *NativeAccessTokenCoordinator
	cookies           *RemoteSessionCookieStore
	jars              map[string]*PartitionJar
	flights           *OauthLoginFlights
	providers         map[string][]AdvertisedAuthProvider
	recovery          *CloudSessionRecovery
	portal            portalRenewal
}

func (a *authSlice) state() *authInner {
	a.once.Do(func() {
		a.inner = &authInner{
			wsHeaders: newRemoteWsHeaderStore(100),
			native:    nativeTokenCache{tokens: map[string]NativeTokenSet{}},
			cookies:   NewRemoteSessionCookieStore(),
			jars:      map[string]*PartitionJar{},
			flights:   NewOauthLoginFlights(),
			providers: map[string][]AdvertisedAuthProvider{},
		}
	})
	return a.inner
}

// WireAuth hands the service the shell's system browser and sign-in windows
func (s *Service) WireAuth(host AuthHost) {
	st := s.auth.state()
	st.mu.Lock()
	st.host = host
	st.mu.Unlock()
}

func (s *Service) authHost() AuthHost {
	st := s.auth.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.host
}

// Backend service the routes boot remotes for, attached by the shell
func (s *Service) authBackend() *backend.Service { return s.local.backend }

// Cloud cookie session recovery of the ticket mint, built on first use
func (s *Service) cloudRecovery() *CloudSessionRecovery {
	st := s.auth.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.recovery == nil {
		st.recovery = NewCloudSessionRecovery(CloudRecoveryDeps{
			HasNativeSession:     s.hasNativeSession,
			RestoreCookieSession: s.restoreCloudCookieSession,
			OnRecovered: func(baseURL string) {
				s.log("[cloud] saved gateway session recovered for " + HostLabelFromBaseURL(baseURL))
			},
			Now: func() int64 { return time.Now().UnixMilli() },
		})
	}
	return st.recovery
}

// Coordinator of the native bearer tokens, built on first use
func (s *Service) nativeTokens() *NativeAccessTokenCoordinator {
	st := s.auth.state()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.tokens == nil {
		st.tokens = s.newNativeAccessTokenCoordinator()
	}
	return st.tokens
}
