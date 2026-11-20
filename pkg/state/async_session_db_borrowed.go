package state

// Async door onto a handle another owner closes, so a handle sweep skips it
func NewBorrowedAsyncSessionDB(db *SessionDB) *AsyncSessionDB {
	return &AsyncSessionDB{db: db, borrowed: true}
}

// Whether the wrapped handle belongs to another owner that closes it
func (a *AsyncSessionDB) Borrowed() bool { return a != nil && a.borrowed }
