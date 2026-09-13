//go:build !windows

package main

// defaultConfigPath is read at startup when it exists, and silently skipped
// when it does not, so escan still runs with no configuration at all.
const defaultConfigPath = "/etc/escan.conf"
