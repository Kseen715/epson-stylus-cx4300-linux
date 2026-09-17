//go:build linux

package main

// defaultCalibrationPath is the file "escan calibrate" writes, so calibrating a
// scanner once serves the web UI and every SANE frontend alike.
const defaultCalibrationPath = "/etc/escan-calibration.json"
