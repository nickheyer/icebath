package gateway

import "sort"

// Platforms in the insertion order a Python dict keeps, platforms stored without SetPlatform following sorted
func (c *GatewayConfig) PlatformKeys() []Platform {
	out := make([]Platform, 0, len(c.Platforms))
	seen := make(map[Platform]bool, len(c.Platforms))
	for _, p := range c.platformOrder {
		if _, ok := c.Platforms[p]; ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	rest := make([]Platform, 0, len(c.Platforms)-len(out))
	for p := range c.Platforms {
		if !seen[p] {
			rest = append(rest, p)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i] < rest[j] })
	return append(out, rest...)
}

// Stores a platform block, a platform new to the mapping moved to the end of the order as a dict insert does
func (c *GatewayConfig) SetPlatform(p Platform, pc *PlatformConfig) {
	if c.Platforms == nil {
		c.Platforms = map[Platform]*PlatformConfig{}
	}
	if _, ok := c.Platforms[p]; !ok {
		order := c.PlatformKeys()
		c.platformOrder = append(order[:len(order):len(order)], p)
	}
	c.Platforms[p] = pc
}

// Config of a platform, created with the defaults and appended to the order when absent
func (c *GatewayConfig) PlatformSetDefault(p Platform) *PlatformConfig {
	if pc, ok := c.Platforms[p]; ok {
		return pc
	}
	pc := NewPlatformConfig()
	c.SetPlatform(p, pc)
	return pc
}
