package state

import (
	crand "crypto/rand"
	"encoding/hex"
	"regexp"
	"time"
)

// Matches the timestamp prefix every minted session id starts with
var SessionIDPattern = regexp.MustCompile(`^\p{Nd}{8}_\p{Nd}{6}_`)

// Matches every id shape a surface of this repo mints
var SessionIDRecognizers = []*regexp.Regexp{
	SessionIDPattern,
	regexp.MustCompile(`^cron_[A-Za-z0-9][A-Za-z0-9._-]*_\p{Nd}{8}_\p{Nd}{6}\n?$`),
	regexp.MustCompile(`^bg_(?:\p{Nd}{6}_)?[0-9a-f]{6}\n?$`),
	regexp.MustCompile(`^room_[0-9a-f]{32}\n?$`),
	regexp.MustCompile(`^api_\p{Nd}{9,}_[0-9a-f]{8}\n?$`),
	regexp.MustCompile(`^api-[0-9a-f]{16}\n?$`),
	regexp.MustCompile(`^run_[0-9a-f]{32}\n?$`),
	regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\n?$`),
}

// Reports whether a value has the shape of an id this repo really mints
func IsKnownSessionID(value string) bool {
	for _, pattern := range SessionIDRecognizers {
		if pattern.MatchString(value) {
			return true
		}
	}
	return false
}

// The random hex length of a minted session id
const DefaultHexLen = 6

// Hex of a random version 4 UUID
func uuid4Hex() string {
	var b [16]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b[:])
}

// Mints a timestamp and random hex session id, the zero time meaning now
func NewSessionIDAt(now time.Time, hexLen int) string {
	if now.IsZero() {
		now = time.Now()
	}
	random := uuid4Hex()
	switch {
	case hexLen < 0:
		random = random[:max(0, len(random)+hexLen)]
	case hexLen < len(random):
		random = random[:hexLen]
	}
	return now.Format("20060102_150405") + "_" + random
}
