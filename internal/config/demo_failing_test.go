package config

import "testing"

// Deliberately failing test used once to prove CI blocks merges. Never merged.
func TestDeliberatelyFailing(t *testing.T) { t.Fatal("this PR must be blocked") }
