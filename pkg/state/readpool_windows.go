package state

var rlimInfinity int64 = -1

// Windows has no resource module so the descriptor ceiling reads as unmeasurable
var rlimitNofile func() (int64, error)
