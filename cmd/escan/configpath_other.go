//go:build !windows

package main

// defaultConfigPath is read at startup when it exists, and silently skipped
// when it does not, so escan still runs with no configuration at all.
const defaultConfigPath = "/etc/escan.conf"

// defaultCalibrationPath holds what was measured from the scanner rather than
// what an operator wrote, so it sits beside the configuration rather than in
// it: it is rewritten by "escan calibrate" and never hand-edited.
const defaultCalibrationPath = "/etc/escan-calibration.json"
