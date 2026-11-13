package agent

import "github.com/nickheyer/icebath/pkg/state"

// Restarts the activity time, description and provenance of a cached agent as one triple
func (a *AIAgent) ResetActivityTriple(desc string) {
	lock := a.livenessActivityLock()
	lock.Lock()
	a.lastActivityTS = activityWallNow()
	a.lastActivityDesc = desc
	a.lastActivityProvenance = state.ActivityProvenanceUnknown
	lock.Unlock()
}

// Moves the session database flush cursor so the next flush persists from index i
func (a *AIAgent) SetLastFlushedDBIdx(i int) { a.lastFlushedDBIdx = i }

// Sets the API call count of the current turn
func (a *AIAgent) SetAPICallCount(n int) { a.apiCallCount.Store(int64(n)) }

// Stamps the activity clock with the description and provenance a producer reports
func (a *AIAgent) TouchActivityAs(desc, provenance string) { a.touchActivity(desc, provenance, false) }
