package main

import (
	"math"
	"testing"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// lagRows builds row means for three planes where each plane samples the same
// band-limited page at its own row offset, which is what a real capture looks
// like once the optics have filtered it.
func lagRows(lines int, lag [3]float64) [3][]float64 {
	page := func(y float64) float64 {
		return 140 + 50*math.Sin(2*math.Pi*y/6.3) + 30*math.Sin(2*math.Pi*y/17)
	}
	var rows [3][]float64
	for c := 0; c < 3; c++ {
		rows[c] = make([]float64, lines)
		for y := 0; y < lines; y++ {
			rows[c][y] = page(float64(y) + lag[c])
		}
	}
	return rows
}

func passWithLag(dpi, lines int, lag [3]float64, stride cx4300.StrideMeasurement) pass {
	p := pass{dpi: dpi, width: 100, rows: lagRows(lines, lag), stride: stride}
	p.lag = cx4300.MeasurePlaneLagFrom(p.rows)
	return p
}

func agreeing() cx4300.StrideMeasurement {
	return cx4300.StrideMeasurement{Expected: 336, Best: 336, Distinct: true}
}

// The whole calibration rests on reading the wire lines correctly. A unit that
// pads them differently shears every image, and measuring colour on top of that
// would produce a plausible-looking file for a scanner that cannot decode at
// all - so the sweep has to stop.
func TestCheckStrideRefusesAMismatch(t *testing.T) {
	mismatch := cx4300.StrideMeasurement{Expected: 336, Best: 348, Distinct: true}
	passes := []pass{
		passWithLag(300, 200, [3]float64{0, 0.2, 0.8}, agreeing()),
		passWithLag(600, 200, [3]float64{0, 0.2, 0.8}, mismatch),
	}
	if checkStride(passes) {
		t.Error("went ahead despite a resolution whose line length disagrees")
	}
}

// Inconclusive is not agreement: it means the page could not confirm the rule,
// so the rule stays unverified and there is nothing to stand on.
func TestCheckStrideRefusesWhenNothingIsConfirmed(t *testing.T) {
	unclear := cx4300.StrideMeasurement{Expected: 336, Distinct: false}
	passes := []pass{
		passWithLag(300, 200, [3]float64{0, 0.2, 0.8}, unclear),
		passWithLag(600, 200, [3]float64{0, 0.2, 0.8}, unclear),
	}
	if checkStride(passes) {
		t.Error("went ahead with the padding rule unverified at every resolution")
	}
}

func TestCheckStrideAcceptsPartialConfirmation(t *testing.T) {
	unclear := cx4300.StrideMeasurement{Expected: 336, Distinct: false}
	passes := []pass{
		passWithLag(75, 200, [3]float64{0, 0.2, 0.8}, unclear),
		passWithLag(300, 200, [3]float64{0, 0.2, 0.8}, agreeing()),
	}
	if !checkStride(passes) {
		t.Error("refused although one resolution confirmed the rule and none disagreed")
	}
}

// residual is what the verification is built on: the misalignment a given lag
// leaves behind. Applying the true lag must leave far less than applying none.
func TestResidualShrinksWithTheRightLag(t *testing.T) {
	const trueLag = 0.8
	p := passWithLag(300, 200, [3]float64{0, 0, trueLag}, agreeing())

	uncorrected := math.Abs(residual(p, 2, 0))
	corrected := math.Abs(residual(p, 2, trueLag))
	if corrected > uncorrected/4 {
		t.Errorf("residual went from %.3f to %.3f applying the true lag %.2f; "+
			"it should have mostly gone", uncorrected, corrected, trueLag)
	}
}

// Measuring a number and that number helping are different things, and the
// verification is the only step that tells them apart.
func TestVerifyRejectsAWorseCalibration(t *testing.T) {
	const trueLag = 0.8
	passes := []pass{
		passWithLag(300, 200, [3]float64{0, 0, trueLag}, agreeing()),
		passWithLag(600, 200, [3]float64{0, 0, trueLag}, agreeing()),
	}
	good := cx4300.Calibration{PlaneRowLag: [3]float64{0, 0, trueLag}}
	bad := cx4300.Calibration{PlaneRowLag: [3]float64{0, 0, 0.1}}

	if !verify(passes, cx4300.Calibration{}, good) {
		t.Error("rejected the true lag as no improvement on no correction at all")
	}
	if verify(passes, good, bad) {
		t.Errorf("accepted %v, which leaves the planes worse aligned than %v",
			bad.PlaneRowLag, good.PlaneRowLag)
	}
}

// A plane the new calibration does not touch is not something to verify, and
// must not be able to fail the check.
func TestVerifyIgnoresUnchangedPlanes(t *testing.T) {
	passes := []pass{passWithLag(300, 200, [3]float64{0, 0, 0.8}, agreeing())}
	same := cx4300.Calibration{PlaneRowLag: [3]float64{0, 0.17, 0.90}}
	if !verify(passes, same, same) {
		t.Error("a calibration that changes nothing failed verification")
	}
}
