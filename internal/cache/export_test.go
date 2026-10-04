package cache

// SetBuildHook lets tests pause a seat-map rebuild at a known point.
func SetBuildHook(c *Cache, f func()) { c.buildHook = f }
