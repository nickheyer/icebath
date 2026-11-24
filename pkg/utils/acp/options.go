package acputil

import "time"

// Line limit a connection starts with, 50 MiB so prompts carrying images fit in one frame
const DefaultLineLimit = 50 << 20

// Time RunAgent gives running handlers to return once it has closed the connection, unless ShutdownGrace sets another
const DefaultShutdownGrace = 5 * time.Second

// Setting of a connection
type Option func(*config)

type config struct {
	limit    int
	unstable bool
	grace    time.Duration
}

// Frames longer than n bytes are dropped without ending the connection: such a request is answered with an invalid request error and such a reply fails its request with ErrFrameTooLong; zero or less lifts the limit
func LineLimit(n int) Option { return func(c *config) { c.limit = n } }

// Whether an agent connection serves the methods of the unstable protocol instead of answering them with method not found
func UnstableMethods(on bool) Option { return func(c *config) { c.unstable = on } }

// Time RunAgent waits for running handlers to return after it closed the connection, before it reports the ones left; zero or less does not wait
func ShutdownGrace(d time.Duration) Option { return func(c *config) { c.grace = d } }

func newConfig(opts []Option) config {
	c := config{limit: DefaultLineLimit, grace: DefaultShutdownGrace}
	for _, opt := range opts {
		opt(&c)
	}
	return c
}
