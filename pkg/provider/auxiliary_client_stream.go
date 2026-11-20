package provider

// Floor and timeout multiple of the absolute bound on a streamed auxiliary call
const (
	auxStreamCeilingFloorSeconds = 600.0
	auxStreamCeilingMultiplier   = 4.0
)

// Absolute wall clock bound of a streamed auxiliary call, zero standing for an unset timeout
var auxStreamTotalCeilingFn = auxStreamTotalCeiling

func auxStreamTotalCeiling(effectiveTimeout float64) float64 {
	return max(auxStreamCeilingFloorSeconds, auxStreamCeilingMultiplier*effectiveTimeout)
}
