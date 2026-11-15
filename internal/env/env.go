// Package env resolves Vestrail settings while accepting legacy ARTEX names.
package env

import (
	"os"
	"strings"
)

// Lookup returns the value and the name that supplied it. An explicitly set
// VESTRAIL_ variable, even empty, overrides its legacy equivalent.
func Lookup(name string) (value, source string) {
	if value, ok := os.LookupEnv(name); ok {
		return value, name
	}
	if suffix, ok := strings.CutPrefix(name, "VESTRAIL_"); ok {
		legacy := "ARTEX_" + suffix
		if value, ok := os.LookupEnv(legacy); ok {
			return value, legacy
		}
	}
	return "", ""
}

func Get(name string) string {
	value, _ := Lookup(name)
	return value
}
