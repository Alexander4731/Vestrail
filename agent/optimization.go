package agent

import (
	"os"
	"strings"
)

// OptimizationEnabled reads an opt-out switch at the operation boundary, so
// tests and local A/B runs do not need global configuration changes.
func OptimizationEnabled(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}
