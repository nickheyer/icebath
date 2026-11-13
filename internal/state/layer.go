package state

import (
	"context"
	"time"
)

// Hooks of the tables a store manages, each run where the engine reaches it and each optional
type layer struct {
	// Creates and migrates tables on the writer during a writable open, retried while a sibling holds the write lock
	init func(ctx context.Context, s *Store, c Conn) error
	// Prepares every new writer and pooled read connection, the writer flag telling them apart
	connSetup func(ctx context.Context, s *Store, c Conn, writer bool) error
	// Probes table availability with reads only during a read-only open
	probe func(ctx context.Context, s *Store, c Conn) error
	// Repairs a malformed schema in place after taking a backup, reporting whether the file now opens
	repairSchema func(ctx context.Context, e *Engine, path string) (bool, error)
	// Detaches the derived full-text indexes after full-text scoped write corruption, true when the write should run again
	failOpen func(ctx context.Context, s *Store, err error, deadline time.Time, patience time.Duration) (bool, error)
	// Runs bounded incremental full-text merges every thousand successful writes
	merge func(ctx context.Context, s *Store)
	// Drains queued work that needs the writer before the store closes it
	drain func(ctx context.Context, s *Store)
	// Session table state of this store, the full-text flags and the caller's session options
	state *tableState
}

// Table layer of the session store, schema and migrations at open, the CJK tokenizer on every connection
func sessionTables(o SessionOptions) layer {
	return layer{
		init:         initSessionSchema,
		connSetup:    setupSessionConn,
		probe:        probeSessionTables,
		repairSchema: repairMalformedSchema,
		failOpen:     enterFTSFailOpen,
		merge:        tryIncrementalMergeFTS,
		state:        &tableState{options: o},
	}
}
