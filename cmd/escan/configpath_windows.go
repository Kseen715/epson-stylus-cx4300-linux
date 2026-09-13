package main

// Windows has no /etc; the machine-wide equivalent is ProgramData, which is
// where a service-style install puts its configuration.
const defaultConfigPath = `C:\ProgramData\escan\escan.conf`
