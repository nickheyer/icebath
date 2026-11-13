package connections

import (
	"context"
	"unicode/utf16"

	"github.com/nickheyer/icebath/pkg/desktop/backend"
)

// Sends an event to every renderer window
func (s *Service) broadcast(event string, payload any) { s.local.broadcast(event, payload) }

// Writes the buffered desktop.log lines out before an error crosses to the renderer
func (s *Service) flushLog() { s.local.flushLog() }

// Reloads the primary window after its profile changed under it
func (s *Service) reloadMainWindow() { s.local.reloadMainWindow() }

// Install id resolver of the backend the primary session aggregate reads
func (s *Service) primaryInstallID() *PrimaryInstallIDResolver {
	s.local.primaryInstallOnce.Do(func() {
		s.local.primaryInstall = NewPrimaryInstallIDResolver(
			func() (any, error) {
				c, err := s.local.backend.EnsureBackend("", backend.EnsureOptions{Passive: true})
				return map[string]any(c), err
			},
			func(descriptor any) (any, error) {
				return s.fetchJSONForBackend(context.Background(), backend.Connection(jsObject(descriptor)), "/api/status", backendFetchOptions{TimeoutMs: 2000})
			})
	})
	return s.local.primaryInstall
}

// String.prototype.slice of text from zero to n UTF-16 units
func jsStringSlice(text string, n int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= n {
		return text
	}
	return string(utf16.Decode(units[:n]))
}

// First line of the Node stack of an Error carrying the message
func errorDetail(err error) string { return "Error: " + err.Error() }
