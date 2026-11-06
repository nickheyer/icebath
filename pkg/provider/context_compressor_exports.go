package provider

// Python TimeoutError, connection error check of the auxiliary client for summary failure classification
func AuxIsConnectionError(err error) bool { return auxIsConnectionError(err) }

// HTTP status carried on the error itself or on its response, zero when absent
func ExcStatusCode(err error) int { return auxExcHTTPStatus(err) }

// Opaque replay items with encrypted payloads removed, the shape the budget estimator charges
func StripOpaqueReplayItems(items any) any { return stripOpaqueReplayItems(items) }

// Count of newest image carriers to retire so the outbound request stays under the image ceilings
func OutboundImageRetireCount(carrierBlocks []int, reservedBlocks int, carrierBytes []int, reservedBytes int, manyImageSafe bool) int {
	return outboundImageRetireCount(carrierBlocks, reservedBlocks, carrierBytes, reservedBytes, manyImageSafe)
}

// Whether every image payload fits the per image limit that applies once many images ride one request
func ImagesAllWithinManyImageLimit(datas []string) bool { return imagesAllWithinManyImageLimit(datas) }

// Widens the retire count through the newest earlier retired carrier and records the carriers retired now
func LatchedRetireCount(retire int, carrierIDsNewestFirst []string, retiredIDs map[string]bool) int {
	if retiredIDs == nil {
		return retire
	}
	total := len(carrierIDsNewestFirst)
	for n, cid := range carrierIDsNewestFirst {
		if cid != "" && retiredIDs[cid] {
			retire = max(retire, total-n)
			break
		}
	}
	start := total - retire
	if start < 0 {
		start = max(start+total, 0)
	}
	for _, cid := range carrierIDsNewestFirst[min(start, total):] {
		if cid != "" {
			retiredIDs[cid] = true
		}
	}
	return retire
}
