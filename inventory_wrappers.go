package flob

// Unwrap exposes the primary pool's optional inventory capabilities.
func (s *CacheStores) Unwrap() Stores { return s.Primary }

// Unwrap exposes the primary pool's optional inventory capabilities.
func (s FallbackStores) Unwrap() Stores { return s.Primary }
