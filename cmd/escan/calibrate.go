package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

// The area "escan calibrate" scans, in 1/600 inch units: a 3 x 2 inch window
// half an inch in from the corner, which is where a page laid against the
// corner stop has its printing.
var calibrationArea = cx4300.Area{X: 300, Y: 300, W: 1800, H: 1200}

// planeNames indexes the colour planes the way the wire format does.
var planeNames = [3]string{"red", "green", "blue"}

// pass is one resolution's worth of measurement.
type pass struct {
	dpi    int
	width  int
	rows   [3][]float64
	stride cx4300.StrideMeasurement
	lag    cx4300.LagMeasurement
	levels cx4300.ChannelLevels
}

// runCalibrate implements "escan calibrate": it measures what has to be
// measured per unit rather than derived from the protocol, checks the
// assumptions the decode makes about this scanner, proves the result improves
// things, and only then writes it.
//
// It is a subcommand rather than a flag because it does not serve: it scans,
// measures, writes and exits.
func runCalibrate(args []string) int {
	fset := flag.NewFlagSet("escan calibrate", flag.ExitOnError)
	path := fset.String("calibration", defaultCalibrationPath,
		"file to write the measured calibration to")
	show := fset.Bool("show", false,
		"print the stored calibration and exit without scanning")
	dry := fset.Bool("dry-run", false,
		"measure and print, but do not write the file")
	quick := fset.Bool("quick", false,
		"measure at 300 dpi only instead of every resolution the device offers")
	fset.Usage = func() {
		fmt.Fprintf(fset.Output(),
			"Usage: escan calibrate [flags]\n\n"+
				"Measures this scanner's colour plane alignment and stores it, so the\n"+
				"decode uses this unit's numbers instead of the built-in ones.\n\n"+
				"Put a printed page on the glass first, against the top-left corner:\n"+
				"text, a table, anything with detail running across the page. A blank\n"+
				"sheet or an empty platen cannot be measured and nothing is written.\n\n"+
				"It scans once at each resolution the device offers, checks that this\n"+
				"unit pads its wire lines the way the decode expects, checks the\n"+
				"resolutions agree with each other, and verifies that the calibration\n"+
				"it arrived at actually reduces the misalignment before writing.\n\n"+
				"Flags:\n")
		fset.PrintDefaults()
	}
	if err := fset.Parse(args); err != nil {
		return 2
	}

	stored, found, err := cx4300.LoadCalibration(*path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "escan: %v\n", err)
		fmt.Fprintf(os.Stderr, "escan: the built-in calibration is in force until that is fixed\n")
		if *show {
			return 1
		}
	}
	if *show {
		printCalibration(*path, stored, found)
		return 0
	}

	resolutions := cx4300.SupportedDPI
	if *quick {
		resolutions = []int{cx4300.OpticalDPI}
	}

	dev, info, cleanup, status := openForCalibration()
	if status != 0 {
		return status
	}
	defer cleanup()

	fmt.Printf("Measuring %q over a %.1f x %.1f inch window, at %d resolutions.\n",
		info.Model, float64(calibrationArea.W)/cx4300.Unit,
		float64(calibrationArea.H)/cx4300.Unit, len(resolutions))
	fmt.Println("The page on the glass needs detail across it; a blank sheet cannot be measured.")

	passes := make([]pass, 0, len(resolutions))
	for _, dpi := range resolutions {
		fmt.Printf("\n%d dpi: ", dpi)
		started := time.Now()
		raw, width, _, err := dev.ScanRaw(cx4300.Params{DPI: dpi, Area: calibrationArea})
		if err != nil {
			fmt.Fprintf(os.Stderr, "\nescan: %v\n", err)
			return 1
		}
		fmt.Printf("scanned in %s\n", time.Since(started).Round(time.Second))

		p := pass{
			dpi:    dpi,
			width:  width,
			rows:   cx4300.PlaneRowMeans(raw, width, dpi),
			stride: cx4300.MeasureLineStride(raw, width, dpi),
			levels: cx4300.MeasureChannelLevels(raw, width, dpi),
		}
		p.lag = cx4300.MeasurePlaneLagFrom(p.rows)
		passes = append(passes, p)
		reportPass(p)
	}

	if !checkStride(passes) {
		return 1
	}
	reportLevels(passes)

	combined, spread, counts := cx4300.AverageLag(lags(passes))
	if !reportCombined(combined, spread, counts, len(passes)) {
		return 1
	}

	updated, changed := stored.Apply(combined, info.Model)
	updated.Confidence = combined.Confidence
	updated.Spread = spread
	reportChanges(stored, updated, combined, changed)
	if !changed[1] && !changed[2] {
		fmt.Fprintln(os.Stderr, "\nescan: nothing usable was measured, so nothing was written.")
		return 1
	}
	if !verify(passes, stored, updated) {
		return 1
	}

	if *dry {
		fmt.Printf("\nNot written: --dry-run. It would have gone to %s\n", *path)
		return 0
	}
	if err := updated.Save(*path); err != nil {
		fmt.Fprintf(os.Stderr, "\nescan: could not write %s: %v\n", *path, err)
		if os.IsPermission(err) {
			fmt.Fprintln(os.Stderr, "escan: that location needs root; either run this "+
				"under sudo or pass --calibration with somewhere you can write")
		}
		return 1
	}
	fmt.Printf("\nWritten to %s. It takes effect the next time escan or a SANE "+
		"frontend starts.\n", *path)
	return 0
}

// openForCalibration gets a device that can hand over raw wire data, which is
// what every measurement here works on.
func openForCalibration() (*cx4300.Device, cx4300.DeviceInfo, func(), int) {
	sc, err := cx4300.Open()
	if err != nil {
		fmt.Fprintf(os.Stderr, "escan: %v\n", err)
		return nil, cx4300.DeviceInfo{}, func() {}, 1
	}
	dev, ok := sc.(*cx4300.Device)
	if !ok {
		sc.Close()
		fmt.Fprintln(os.Stderr, "escan: calibration needs the raw wire data, which "+
			"only the Linux backend exposes; on Windows the WIA driver hands over a "+
			"finished image and there is nothing to measure")
		return nil, cx4300.DeviceInfo{}, func() {}, 1
	}
	info, err := dev.Identify()
	if err != nil {
		sc.Close()
		fmt.Fprintf(os.Stderr, "escan: %v\n", err)
		return nil, cx4300.DeviceInfo{}, func() {}, 1
	}
	return dev, info, func() { sc.Close() }, 0
}

func lags(passes []pass) []cx4300.LagMeasurement {
	out := make([]cx4300.LagMeasurement, 0, len(passes))
	for _, p := range passes {
		out = append(out, p.lag)
	}
	return out
}

func reportPass(p pass) {
	switch {
	case p.stride.Agrees():
		fmt.Printf("  wire line: %d bytes, as the decode expects\n", p.stride.Best)
	case !p.stride.Distinct:
		fmt.Printf("  wire line: inconclusive - this page repeats down the page, so no "+
			"length stands out (expected %d)\n", p.stride.Expected)
	default:
		fmt.Printf("  wire line: %d bytes, but the decode expects %d - off by %d\n",
			p.stride.Best, p.stride.Expected, p.stride.Best-p.stride.Expected)
	}
	for c := 1; c < 3; c++ {
		if !p.lag.Measured[c] {
			fmt.Printf("  %-5s could not be measured (best correlation %.3f)\n",
				planeNames[c], p.lag.Confidence[c])
			continue
		}
		fmt.Printf("  %-5s trails red by %+.2f lines (correlation %.3f)\n",
			planeNames[c], p.lag.Lag[c], p.lag.Confidence[c])
	}
}

// checkStride refuses to go on when this unit does not pad its wire lines the
// way the decode expects. Everything else rests on reading the lines correctly,
// so a mismatch is not something a plane alignment can paper over.
func checkStride(passes []pass) bool {
	var bad, agreed int
	for _, p := range passes {
		switch {
		case p.stride.Agrees():
			agreed++
		case p.stride.Distinct:
			bad++
		}
	}
	if bad > 0 {
		fmt.Fprintf(os.Stderr, "\nescan: this scanner does not pad its wire lines the "+
			"way the decode expects, at %d of %d resolutions. Every image it produces "+
			"is sheared, and a colour calibration cannot help with that - the padding "+
			"rule in cx4300.PlaneStride needs to account for this unit. Nothing was "+
			"written; please report the numbers above.\n", bad, len(passes))
		return false
	}
	if agreed == 0 {
		fmt.Fprintln(os.Stderr, "\nescan: the wire line length could not be confirmed at "+
			"any resolution, so the decode's padding rule is unverified on this unit. "+
			"Use a page with varied printing rather than one that repeats down the "+
			"page. Nothing was written.")
		return false
	}
	fmt.Printf("\nWire line length confirmed at %d of %d resolutions.\n", agreed, len(passes))
	return true
}

// reportLevels reports the channel gains. The decode corrects none of this, so
// it is a warning rather than a measurement that gets stored - but a unit with
// a real cast would show here, and nowhere else.
func reportLevels(passes []pass) {
	worst := passes[0]
	for _, p := range passes {
		if p.levels.Spread > worst.levels.Spread {
			worst = p
		}
	}
	l := worst.levels
	fmt.Printf("Channel levels: red %.1f, green %.1f, blue %.1f",
		l.Mean[0], l.Mean[1], l.Mean[2])
	if l.Spread < 6 {
		fmt.Printf(" - within %.1f counts, no correction needed.\n", l.Spread)
		return
	}
	fmt.Printf(" - %.1f counts apart at %d dpi.\n", l.Spread, worst.dpi)
	fmt.Println("  That is a colour cast this driver does not correct. It applies no")
	fmt.Println("  per-channel gain, because the unit it was written against needed")
	fmt.Println("  none. Worth reporting.")
	if max3(l.Clipped) > 0.25 {
		fmt.Printf("  (%.0f%% of pixels are saturated, which flattens this reading;\n",
			max3(l.Clipped)*100)
		fmt.Println("  a page with no blown highlights would measure it better.)")
	}
}

// reportCombined prints the aggregate and reports whether the resolutions agree
// well enough to trust it. They should: the lag is in wire lines, not inches,
// so it does not vary with resolution - on the unit this was written against.
func reportCombined(m cx4300.LagMeasurement, spread [3]float64, counts [3]int, passes int) bool {
	fmt.Println("\nCombined across resolutions:")
	any := false
	for c := 1; c < 3; c++ {
		if !m.Measured[c] {
			fmt.Printf("  %-5s not measured at any resolution\n", planeNames[c])
			continue
		}
		any = true
		fmt.Printf("  %-5s %+.2f lines from %d of %d resolutions "+
			"(correlation %.3f, spread %.2f)\n",
			planeNames[c], m.Lag[c], counts[c], passes, m.Confidence[c], spread[c])
		if spread[c] > 0.25 {
			fmt.Printf("         the resolutions disagree by %.2f lines, more than "+
				"expected for a quantity that does not vary with resolution;\n"+
				"         the average is still the best estimate, but treat it as rough\n",
				spread[c])
		}
	}
	if !any {
		fmt.Fprintln(os.Stderr, "\nescan: nothing could be measured at any resolution. "+
			"The glass needs a printed page with detail running across it - rules, "+
			"text, a table. Nothing was written.")
		return false
	}
	return true
}

func reportChanges(before, after cx4300.Calibration, m cx4300.LagMeasurement, changed [3]bool) {
	fmt.Println()
	for c := 1; c < 3; c++ {
		if changed[c] {
			fmt.Printf("  %-5s %.2f -> %.2f\n", planeNames[c],
				before.PlaneRowLag[c], after.PlaneRowLag[c])
			continue
		}
		reason := "this scan could not measure it"
		if m.Measured[c] {
			reason = fmt.Sprintf("the measured %+.2f is outside the usable range, "+
				"since the decode carries one line of history", m.Lag[c])
		}
		fmt.Printf("  %-5s keeps %.2f: %s\n", planeNames[c], after.PlaneRowLag[c], reason)
	}
}

// verify applies the new calibration to the captures it came from and checks it
// leaves less misalignment than the old one did. Measuring a number and storing
// it are not the same as the number helping, and this is the only step that
// tells the two apart.
func verify(passes []pass, before, after cx4300.Calibration) bool {
	fmt.Println("\nVerifying against the captures it was measured from:")
	ok := true
	for c := 1; c < 3; c++ {
		if before.PlaneRowLag[c] == after.PlaneRowLag[c] {
			continue
		}
		var oldLeft, newLeft float64
		var n int
		for _, p := range passes {
			if !p.lag.Measured[c] {
				continue
			}
			oldLeft += math.Abs(residual(p, c, before.PlaneRowLag[c]))
			newLeft += math.Abs(residual(p, c, after.PlaneRowLag[c]))
			n++
		}
		if n == 0 {
			continue
		}
		oldLeft, newLeft = oldLeft/float64(n), newLeft/float64(n)
		fmt.Printf("  %-5s misalignment left: %.2f lines with the old %.2f, "+
			"%.2f with the new %.2f\n", planeNames[c],
			oldLeft, before.PlaneRowLag[c], newLeft, after.PlaneRowLag[c])
		// Allowed to be no better only within the rounding the stored value
		// carries; anything worse than that means the measurement misled us.
		if newLeft > oldLeft+0.01 {
			ok = false
		}
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "\nescan: the measured calibration leaves the planes "+
			"worse aligned than the one already in force, so it was not written. That "+
			"usually means the page on the glass has too little detail across it, or "+
			"detail too fine for the scan to resolve; try a page of plain text.")
		return false
	}
	fmt.Println("  the new calibration is at least as good at every resolution.")
	return true
}

// residual is what a given lag leaves behind for one plane of one capture. The
// correction is applied to the row means rather than the pixels, which is exact:
// a linear mix of lines is a linear mix of their means.
func residual(p pass, c int, lag float64) float64 {
	var corrected [3][]float64
	corrected[0] = p.rows[0]
	corrected[c] = cx4300.ApplyLagToRowMeans(p.rows[c], lag)
	// The third plane is not involved; give it the reference so the measurement
	// has something well-formed to look at.
	other := 3 - c
	corrected[other] = p.rows[0]
	return cx4300.MeasurePlaneLagFrom(corrected).Lag[c]
}

func max3(v [3]float64) float64 {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

// printCalibration reports what is in force and where it came from, so an
// operator never has to guess which numbers a scan used.
func printCalibration(path string, c cx4300.Calibration, found bool) {
	b := cx4300.BuiltinCalibration()
	fmt.Printf("built-in:  green %.2f, blue %.2f\n", b.PlaneRowLag[1], b.PlaneRowLag[2])
	if !found {
		fmt.Printf("stored:    nothing at %s, so the built-in values are in force\n", path)
		fmt.Println("\nRun \"escan calibrate\" with a printed page on the glass to measure this unit.")
		return
	}
	fmt.Printf("stored:    green %.2f, blue %.2f   <- in force\n",
		c.PlaneRowLag[1], c.PlaneRowLag[2])
	fmt.Printf("           from %s\n", path)
	if !c.MeasuredAt.IsZero() {
		fmt.Printf("           measured %s\n", c.MeasuredAt.Local().Format(time.RFC1123))
	}
	if c.Device != "" {
		fmt.Printf("           on %q\n", c.Device)
	}
	if c.Confidence != [3]float64{} {
		fmt.Printf("           correlation green %.3f, blue %.3f\n",
			c.Confidence[1], c.Confidence[2])
	}
	if c.Spread != [3]float64{} {
		fmt.Printf("           spread across resolutions green %.2f, blue %.2f\n",
			c.Spread[1], c.Spread[2])
	}
}
