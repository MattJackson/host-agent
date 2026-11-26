//go:build !(darwin && cgo)

package main

// readTemperatures has no sensor source without cgo (see sensors_darwin.go).
// Builds with CGO_ENABLED=0 therefore report no temperature series.
func readTemperatures() map[string]float64 {
	return nil
}
