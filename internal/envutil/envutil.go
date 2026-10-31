// Package envutil has the one env-var lookup helper every ate-ish package
// that reads an ATE_* override needs: os.Getenv with a default.
package envutil

import "os"

// Get returns the named environment variable, or def if it is unset or empty.
func Get(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
