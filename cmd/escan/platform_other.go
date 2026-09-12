//go:build !linux

package main

const backendName = "WIA"

func hubWarning() string { return "" }
