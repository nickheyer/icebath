package provider

// Drops the cached billing token so the next call reads the freshly granted one
func InvalidateNousBillingCachedToken() { nousBillingInvalidateCachedToken() }
