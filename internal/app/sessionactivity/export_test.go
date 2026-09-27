package sessionactivity

import "maps"

// CountsForTest is every count w keeps, copied under its lock: the
// replica's, and each user's and each key's that has a wait running (an
// entry is deleted when its last wait gives its slot back). For tests
// only: the external test package asserts on it through this file.
func (w *Waiter) CountsForTest() (replica int, perUser, perKey map[string]int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.active, maps.Clone(w.perUser), maps.Clone(w.perKey)
}
