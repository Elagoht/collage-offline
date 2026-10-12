package offline

// CacheVersion is cacheVersion, for the external tests.
func CacheVersion(p *Plugin) string { return p.cacheVersion() }
