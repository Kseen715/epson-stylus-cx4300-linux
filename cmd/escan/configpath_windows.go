package main

// Windows has no /etc; the machine-wide equivalent is ProgramData, which is
// where a service-style install puts its configuration.
const defaultConfigPath = `C:\ProgramData\escan\escan.conf`

// defaultCalibrationPath sits beside the configuration, in the machine-wide
// location a service-style install uses.
const defaultCalibrationPath = `C:\ProgramData\escan\escan-calibration.json`
