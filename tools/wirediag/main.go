// Command wirediag captures the scanner's raw wire bytes and measures the
// assumptions the decoder makes about them: bytes per wire line, the vertical
// offset between the three colour planes, and any periodic per-row or
// per-column variation. It is a diagnostic for banding and colour-stripe
// artifacts, where the question is whether the pattern comes from the decode,
// from the sensor, or from the halftone screen of the original.
//
// It either scans (-dpi, -x, -y, -w, -h) or re-analyses a previously captured
// raw file (-in), so a capture can be measured repeatedly without the device.
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"log"
	"math"
	"os"
	"strings"

	"github.com/Kseen715/epson-stylus-cx4300-linux/cx4300"
)

func main() {
	var (
		dpi   = flag.Int("dpi", 300, "scan resolution")
		sweep = flag.Bool("sweep", false,
			"capture and analyse every resolution the device supports, in one pass")
		x     = flag.Int("x", 0, "area origin x, 1/600 inch")
		y     = flag.Int("y", 0, "area origin y, 1/600 inch")
		w     = flag.Int("w", 1200, "area width, 1/600 inch (1200 = 2 inch)")
		h     = flag.Int("h", 1200, "area height, 1/600 inch")
		in    = flag.String("in", "", "analyse this raw capture instead of scanning")
		reuse = flag.String("compare", "",
			"re-analyse a sweep already on disk, as <prefix>-<dpi>dpi.raw, without rescanning")
		out    = flag.String("out", "wirediag", "output prefix for .raw and .png")
		width  = flag.Int("width", 0, "pixel width of -in capture (default: from -w/-dpi)")
		height = flag.Int("height", 0, "pixel height of -in capture")
	)
	flag.Parse()

	area := cx4300.Area{X: *x, Y: *y, W: *w, H: *h}

	if *in != "" {
		raw, err := os.ReadFile(*in)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("read %s: %d bytes\n", *in, len(raw))
		pw, ph := area.Pixels(*dpi)
		if *width != 0 {
			pw = *width
		}
		if *height != 0 {
			ph = *height
		}
		analyse(raw, pw, ph, *dpi, *out)
		return
	}

	resolutions := []int{*dpi}
	if *sweep || *reuse != "" {
		resolutions = cx4300.SupportedDPI
	}
	for _, d := range resolutions {
		if err := (cx4300.Params{DPI: d, Area: area}).Validate(); err != nil {
			log.Fatal(err)
		}
	}

	if *reuse != "" {
		var summary []report
		for _, d := range resolutions {
			path := fmt.Sprintf("%s-%ddpi.raw", *reuse, d)
			raw, err := os.ReadFile(path)
			if err != nil {
				fmt.Printf("\nskipping %d dpi: %v\n", d, err)
				continue
			}
			fmt.Printf("\n================ %d dpi ================\n", d)
			fmt.Printf("read %s: %d bytes\n", path, len(raw))
			pw, ph := area.Pixels(d)
			summary = append(summary,
				gridded(analyse(raw, pw, ph, d, ""), raw, area, pw, d, resolutions))
		}
		if len(summary) > 1 {
			printSummary(summary)
			compareColour(summary)
		}
		return
	}

	sc, err := cx4300.Open()
	if err != nil {
		log.Fatal(err)
	}
	defer sc.Close()
	dev, ok := sc.(*cx4300.Device)
	if !ok {
		log.Fatal("raw capture needs the Linux backend")
	}

	var summary []report
	for _, d := range resolutions {
		prefix := *out
		if *sweep {
			prefix = fmt.Sprintf("%s-%ddpi", *out, d)
		}
		fmt.Printf("\n================ %d dpi ================\n", d)
		raw, pw, ph, err := dev.ScanRaw(cx4300.Params{DPI: d, Area: area})
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(prefix+".raw", raw, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s.raw: %d bytes\n", prefix, len(raw))
		summary = append(summary,
			gridded(analyse(raw, pw, ph, d, prefix), raw, area, pw, d, resolutions))
	}
	if len(summary) > 1 {
		printSummary(summary)
		compareColour(summary)
	}
}

// report is one resolution's findings, for the cross-resolution summary. What
// matters there is whether a single calibration can serve every resolution, so
// each field is the thing that would have to be per-resolution if it varied.
type report struct {
	dpi      int
	stride   string
	lag      [3]string // measured, before the decode's correction
	residual [3]string // what is left after it
	means    [3]float64
	clipped  [3]float64
	cast     string

	// grid holds the three channels box-averaged onto a resolution-independent
	// grid, so captures of the same area at different resolutions can be
	// compared pixel for pixel. Only a sweep fills it.
	grid   [3][]float64
	gw, gh int
}

// analyse runs every check over one capture and prints it, returning the parts
// worth comparing between resolutions.
func analyse(raw []byte, pw, ph, dpi int, outPrefix string) report {
	rep := report{dpi: dpi}

	assumed := cx4300.PlaneStride(pw, dpi) * 3
	fmt.Printf("\nrequested %dx%d px at %d dpi\n", pw, ph, dpi)
	fmt.Printf("decoder assumes %d bytes per wire line (plane stride %d px)\n",
		assumed, cx4300.PlaneStride(pw, dpi))
	fmt.Printf("capture holds %d whole lines at that stride; %d were requested\n",
		len(raw)/assumed, ph)

	best := findStride(raw, pw*3, pw*3+400)
	fmt.Printf("\n-- bytes per wire line --\n")
	for _, c := range best.top {
		fmt.Printf("  %6d bytes  roughness %8.3f%s\n", c.stride, c.cost,
			map[bool]string{true: "   <- decoder"}[c.stride == assumed])
	}

	// A sweep only decides the stride if its minimum stands clearly below the
	// rest. Content that repeats down the page makes every candidate look alike,
	// and picking the nominal winner there would hand every later check a
	// wrongly sliced set of planes.
	stride := assumed
	switch {
	case !best.distinct:
		fmt.Printf("  inconclusive: no distinct minimum (best %.3f against %.3f elsewhere).\n",
			best.cost, best.rival)
		fmt.Printf("  Using the decoder's %d bytes for the checks below.\n", assumed)
		rep.stride = "inconclusive"
	case best.stride != assumed:
		fmt.Printf("  MISMATCH: measured %d bytes, decoder uses %d, off by %d per line.\n",
			best.stride, assumed, best.stride-assumed)
		fmt.Printf("  Using the measured %d bytes for the checks below.\n", best.stride)
		stride = best.stride
		rep.stride = fmt.Sprintf("%+d bytes", best.stride-assumed)
	default:
		fmt.Printf("  measured %d bytes, matching the decoder.\n", best.stride)
		rep.stride = "ok"
	}

	plane := stride / 3
	if len(raw) < stride*8 {
		fmt.Println("\ncapture too short for the remaining checks")
		return rep
	}
	lines := len(raw) / stride

	fmt.Printf("\n-- channel levels --\n")
	fmt.Println("(a cast is a per-channel gain the decode does not correct. The verdict")
	fmt.Println(" is read from the channel means, not from paper white: this device")
	fmt.Println(" drives white to saturation, and clipped highlights are equal in every")
	fmt.Println(" channel however far apart the gains are)")
	white := whiteLevels(raw, stride, plane, pw, lines)
	rep.means = channelMeans(raw, stride, plane, pw, lines)
	rep.clipped = clippedFraction(raw, stride, plane, pw, lines)
	names := [3]string{"red  ", "green", "blue "}
	for c := 0; c < 3; c++ {
		fmt.Printf("  %s: mean %6.2f   95th percentile %5.1f   at 255: %5.2f%%\n",
			names[c], rep.means[c], white[c], rep.clipped[c]*100)
	}
	rep.cast = describeCast(rep.means, rep.clipped)
	fmt.Printf("  %s\n", rep.cast)

	fmt.Printf("\n-- vertical offset between colour planes --\n")
	fmt.Println("(a tri-linear CCD reads R, G and B on physically separate rows;")
	fmt.Println(" the decoder takes all three from the same wire line, so a non-zero")
	fmt.Println(" offset here is real colour misregistration - and a printed dither")
	fmt.Println(" sampled through it turns grey into a hue that rotates across the page)")
	fmt.Println(" needs horizontal detail - rules, text baselines - to measure at all")
	// Measured by the package, so the calibration a scan uses and the number a
	// diagnostic prints come from one piece of code rather than two copies.
	lag := cx4300.PlaneRowLag()
	var rows [3][]float64
	for c := 0; c < 3; c++ {
		rows[c] = rowMeans(raw, stride, plane, pw, lines, c)
	}
	m := cx4300.MeasurePlaneLagFrom(rows)

	// The same measurement with the decode's correction already applied to the
	// row means: a linear mix of lines is a linear mix of their means, so what
	// comes back is what the correction leaves behind.
	var corrected [3][]float64
	for c := 0; c < 3; c++ {
		corrected[c] = applyLag(rows[c], lag[c])
	}
	res := cx4300.MeasurePlaneLagFrom(corrected)

	planeNames := [3]string{"red", "green", "blue"}
	for c := 1; c < 3; c++ {
		if !m.Measured[c] {
			fmt.Printf("  %s vs red: too flat to measure (best correlation %.3f)"+
				" - recapture over table rules or text\n", planeNames[c], m.Confidence[c])
			rep.lag[c], rep.residual[c] = "flat", "flat"
			continue
		}
		fmt.Printf("  %s vs red: trails by %.2f rows (correlation %.3f)\n",
			planeNames[c], m.Lag[c], m.Confidence[c])
		fmt.Printf("      correlation %s\n", curveText(m, c))
		rep.lag[c] = fmt.Sprintf("%+.2f", -m.Lag[c])

		if !res.Measured[c] {
			rep.residual[c] = "flat"
			continue
		}
		fmt.Printf("      after the decode's %.2f row correction: %+.2f rows left\n",
			lag[c], res.Lag[c])
		rep.residual[c] = fmt.Sprintf("%+.2f", -res.Lag[c])
	}

	fmt.Printf("\n-- horizontal plane registration across the width --\n")
	fmt.Println("(the decoder gives all three planes the same pitch; if the device")
	fmt.Println(" does not, the best shift drifts steadily from left to right, which")
	fmt.Println(" turns a printed grey dither into a hue gradient)")
	reportRegistration(raw, stride, plane, pw, lines)

	fmt.Printf("\n-- periodic variation --\n")
	for c := 0; c < 3; c++ {
		rows := rowMeans(raw, stride, plane, pw, lines, c)
		sd := stddev(rows)
		fmt.Printf("  plane %d: row-mean sd %6.2f, dominant period %s\n",
			c, sd, describePeriod(rows))
	}
	fmt.Printf("  even/odd column split: %s\n", oddEvenColumns(raw, stride, plane, pw, lines))

	// An empty prefix means re-analysis of a capture already on disk: there is
	// nothing to write, and a PNG left behind by an earlier sudo run must not
	// be able to end the run either way.
	if outPrefix != "" {
		img := cx4300.Deinterleave(raw, pw, ph, dpi, cx4300.ModeColor)
		if err := writePNG(outPrefix+".png", img); err != nil {
			fmt.Printf("\ncould not write %s.png: %v\n", outPrefix, err)
		} else {
			fmt.Printf("\nwrote %s.png using the current decode\n", outPrefix)
		}
	}
	return rep
}

// printSummary is the point of a sweep: one place to see whether a single
// calibration serves every resolution, or whether each needs its own.
func printSummary(reps []report) {
	fmt.Printf("\n================ across resolutions ================\n")
	fmt.Printf("%6s  %-12s  %-10s  %-10s  %-10s  %-10s  %-20s  %s\n",
		"dpi", "stride", "green lag", "left", "blue lag", "left",
		"means R/G/B", "saturated R/G/B")
	for _, r := range reps {
		fmt.Printf("%6d  %-12s  %-10s  %-10s  %-10s  %-10s  %5.1f/%5.1f/%5.1f  %3.0f%%/%3.0f%%/%3.0f%%\n",
			r.dpi, r.stride, r.lag[1], r.residual[1], r.lag[2], r.residual[2],
			r.means[0], r.means[1], r.means[2],
			r.clipped[0]*100, r.clipped[1]*100, r.clipped[2]*100)
	}
	fmt.Println()
	for _, r := range reps {
		fmt.Printf("  %4d dpi: %s\n", r.dpi, r.cast)
	}
}

// gridded attaches the resolution-independent grid the colour comparison needs.
// The coarsest resolution of the sweep sets it, and every other one is a whole
// multiple of that, because the area is fixed and the pixel count is the area
// times the resolution.
func gridded(rep report, raw []byte, area cx4300.Area, pw, dpi int, resolutions []int) report {
	coarsest := resolutions[0]
	for _, o := range resolutions {
		if o < coarsest {
			coarsest = o
		}
	}
	gw, gh := area.Pixels(coarsest)
	rep.gw, rep.gh = gw, gh
	rep.grid = resampleChannels(raw, cx4300.PlaneStride(pw, dpi), pw, dpi/coarsest, gw, gh)
	return rep
}

// resampleChannels box-averages each colour plane down by factor onto a
// gw x gh grid, applying the decode's plane lag on the way so that what is
// compared is what the decode produces rather than the wire bytes.
func resampleChannels(raw []byte, plane, width, factor, gw, gh int) [3][]float64 {
	stride := plane * 3
	lines := len(raw) / stride
	lag := cx4300.PlaneRowLag()

	var out [3][]float64
	for c := range out {
		out[c] = make([]float64, gw*gh)
	}
	counts := make([]float64, gw*gh)

	for y := 0; y < lines; y++ {
		gy := y / factor
		if gy >= gh {
			break
		}
		cur := raw[y*stride:]
		above := cur
		if y > 0 {
			above = raw[(y-1)*stride:]
		}
		for x := 0; x < width; x++ {
			gx := x / factor
			if gx >= gw {
				break
			}
			counts[gy*gw+gx]++
			for c := 0; c < 3; c++ {
				v := float64(cur[c*plane+x])
				if d := lag[c]; d != 0 {
					v = (1-d)*v + d*float64(above[c*plane+x])
				}
				out[c][gy*gw+gx] += v
			}
		}
	}
	for i, n := range counts {
		if n == 0 {
			continue
		}
		for c := 0; c < 3; c++ {
			out[c][i] /= n
		}
	}
	return out
}

// compareColour asks whether the other resolutions agree with 300 dpi about
// colour, which is the one thing the per-capture checks cannot see: a swapped
// or rotated plane order leaves every stride, lag, registration and level
// measurement looking perfect.
//
// It needs a target with real colour in it. On a black-on-white page all three
// channels carry the same signal, so no comparison can tell them apart, and it
// says so instead of reporting a meaningless winner.
func compareColour(reps []report) {
	var ref *report
	for i := range reps {
		if reps[i].dpi == 300 {
			ref = &reps[i]
		}
	}
	if ref == nil || ref.grid[0] == nil {
		return
	}

	fmt.Printf("\n================ do the resolutions agree about colour? ================\n")
	fmt.Println("(300 dpi is the reference. A plane order that changes with resolution")
	fmt.Println(" would show here and nowhere else.)")

	// How distinguishable the reference's own channels are. If red, green and
	// blue all carry the same picture, nothing below can mean anything.
	names := [3]string{"red", "green", "blue"}

	// Whether the captures agree at all is worth knowing even on a neutral
	// target. Each is first aligned to the reference on its luminance: the scan
	// origin and the optical sampling are not identical across resolutions, and
	// on a page of fine text even a fraction of a cell of offset decorrelates
	// everything. Without that step this reads geometry as a colour fault.
	//
	// What matters afterwards is not the absolute level - detail the coarser
	// grid never resolved keeps it below 1 - but whether the three channels
	// come out level with each other. A colour fault is per-channel; blur and
	// misregistration are not.
	fmt.Println("\n  agreement with the reference, after aligning on luminance:")
	for i := range reps {
		r := &reps[i]
		if r.dpi == 300 || r.grid[0] == nil {
			continue
		}
		dx, dy := bestShift(lumaGrid(r.grid), lumaGrid(ref.grid), r.gw, r.gh, 3)
		var per [3]float64
		var parts []string
		for c := 0; c < 3; c++ {
			per[c] = shiftPearson(r.grid[c], ref.grid[c], r.gw, r.gh, dx, dy)
			parts = append(parts, fmt.Sprintf("%s %+.3f", names[c], per[c]))
		}
		spread := maxOf(per) - minOf(per)
		note := fmt.Sprintf("channels level to within %.3f - no per-channel fault here", spread)
		if spread > 0.05 {
			note = fmt.Sprintf("UNEVEN by %.3f - one channel disagrees more than the others", spread)
		}
		fmt.Printf("    %4d dpi: shift (%+d,%+d)  %s\n              %s\n",
			r.dpi, dx, dy, strings.Join(parts, "  "), note)
	}

	rg, rb, gb, least := channelIndependence(ref.grid)
	fmt.Printf("\n  reference channel independence: R-G %.3f, R-B %.3f, G-B %.3f\n", rg, rb, gb)
	if least > 0.98 {
		fmt.Println("  TARGET TOO NEUTRAL for the plane-order test: the three channels carry")
		fmt.Println("  the same picture, so a swapped order is invisible. Put something with")
		fmt.Println("  saturated colour on the glass - a printed colour block, a book cover -")
		fmt.Println("  and sweep again.")
		return
	}
	fmt.Println("\n  plane order:")
	for i := range reps {
		r := &reps[i]
		if r.dpi == 300 || r.grid[0] == nil {
			continue
		}
		fmt.Printf("\n  %d dpi against 300 dpi:\n", r.dpi)
		match, scores := bestChannelMatch(r.grid, ref.grid)
		for c := 0; c < 3; c++ {
			var parts []string
			for k := 0; k < 3; k++ {
				parts = append(parts, fmt.Sprintf("%s %+.3f", names[k], scores[c][k]))
			}
			verdict := "matches"
			if match[c] != c {
				verdict = "MISMATCH: this plane is the reference's " + names[match[c]]
			}
			fmt.Printf("    its %-5s vs reference  %s   -> %s\n",
				names[c], strings.Join(parts, "  "), verdict)
		}
	}
}

// bestChannelMatch reports, for each of the test capture's channels, which
// reference channel it correlates with most, along with the whole 3x3 of
// scores. An identity mapping means the two captures agree about which plane
// is which; anything else is a plane order that changes with resolution.
func bestChannelMatch(test, ref [3][]float64) (match [3]int, scores [3][3]float64) {
	for c := 0; c < 3; c++ {
		best, bestScore := 0, -2.0
		for k := 0; k < 3; k++ {
			v := pearson(test[c], ref[k])
			scores[c][k] = v
			if v > bestScore {
				best, bestScore = k, v
			}
		}
		match[c] = best
	}
	return match, scores
}

// channelIndependence is the weakest correlation among the three channel pairs
// of one capture. Near 1 means all three carry the same picture - a neutral
// target - and no channel comparison against it can mean anything.
func channelIndependence(g [3][]float64) (rg, rb, gb, least float64) {
	rg = pearson(g[0], g[1])
	rb = pearson(g[0], g[2])
	gb = pearson(g[1], g[2])
	return rg, rb, gb, math.Min(rg, math.Min(rb, gb))
}

// lumaGrid collapses the three channels of a grid to one luminance series, so
// two captures can be aligned on their content rather than on any one channel.
func lumaGrid(g [3][]float64) []float64 {
	out := make([]float64, len(g[0]))
	for i := range out {
		out[i] = 0.299*g[0][i] + 0.587*g[1][i] + 0.114*g[2][i]
	}
	return out
}

// bestShift finds the whole-cell offset that best aligns a onto b. The scan
// origin is not identical at every resolution, and an unaligned comparison
// measures that instead of what it set out to measure.
func bestShift(a, b []float64, gw, gh, span int) (dx, dy int) {
	best := -2.0
	for y := -span; y <= span; y++ {
		for x := -span; x <= span; x++ {
			if v := shiftPearson(a, b, gw, gh, x, y); v > best {
				best, dx, dy = v, x, y
			}
		}
	}
	return dx, dy
}

// shiftPearson correlates a against b with b displaced by (dx,dy) cells, over
// whatever part of the grid both cover.
func shiftPearson(a, b []float64, gw, gh, dx, dy int) float64 {
	var as, bs []float64
	for y := 0; y < gh; y++ {
		sy := y + dy
		if sy < 0 || sy >= gh {
			continue
		}
		for x := 0; x < gw; x++ {
			sx := x + dx
			if sx < 0 || sx >= gw {
				continue
			}
			ai, bi := y*gw+x, sy*gw+sx
			if ai >= len(a) || bi >= len(b) {
				continue
			}
			as = append(as, a[ai])
			bs = append(bs, b[bi])
		}
	}
	if len(as) < 16 {
		return 0
	}
	return pearson(as, bs)
}

// pearson is the correlation of two equal-length series.
func pearson(a, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	ma, mb := mean(a[:n]), mean(b[:n])
	var num, da, db float64
	for i := 0; i < n; i++ {
		x, y := a[i]-ma, b[i]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// whiteLevels is each channel's 95th percentile, which on a page with margins
// is the paper. Comparing the three is how a colour cast shows up as a number
// rather than an impression.
func whiteLevels(raw []byte, stride, plane, width, lines int) [3]float64 {
	var out [3]float64
	for c := 0; c < 3; c++ {
		var hist [256]int
		n := 0
		for y := 0; y < lines; y++ {
			row := raw[y*stride+c*plane:]
			for x := 0; x < width; x++ {
				hist[row[x]]++
				n++
			}
		}
		want, seen := int(float64(n)*0.95), 0
		for v := 0; v < 256; v++ {
			seen += hist[v]
			if seen >= want {
				out[c] = float64(v)
				break
			}
		}
	}
	return out
}

func channelMeans(raw []byte, stride, plane, width, lines int) [3]float64 {
	var out [3]float64
	for c := 0; c < 3; c++ {
		sum := 0
		for y := 0; y < lines; y++ {
			row := raw[y*stride+c*plane:]
			for x := 0; x < width; x++ {
				sum += int(row[x])
			}
		}
		out[c] = float64(sum) / float64(width*lines)
	}
	return out
}

// describeCast turns the channel means into a verdict. The means only speak for
// the colours actually on the page, so heavy clipping is reported alongside:
// saturated pixels are 255 in every channel and pull the means together
// whatever the gains behind them are doing.
func describeCast(means [3]float64, clipped [3]float64) string {
	spread := maxOf(means) - minOf(means)
	caveat := ""
	if maxOf(clipped) > 0.25 {
		caveat = fmt.Sprintf(" (%.0f%% of pixels are saturated, which flattens this - "+
			"recapture over something with no blown highlights to be sure)",
			maxOf(clipped)*100)
	}
	switch {
	case spread < 2:
		return fmt.Sprintf("neutral: channel means span %.2f counts%s", spread, caveat)
	case spread < 6:
		return fmt.Sprintf("slight cast: channel means span %.2f counts%s", spread, caveat)
	default:
		return fmt.Sprintf("CAST: channel means span %.2f counts - a neutral page "+
			"will not look neutral%s", spread, caveat)
	}
}

// clippedFraction is how much of each channel sits at 255. A scan that blows
// its highlights has thrown away the detail a white balance would be read from.
func clippedFraction(raw []byte, stride, plane, width, lines int) [3]float64 {
	var out [3]float64
	for c := 0; c < 3; c++ {
		n := 0
		for y := 0; y < lines; y++ {
			row := raw[y*stride+c*plane:]
			for x := 0; x < width; x++ {
				if row[x] == 0xff {
					n++
				}
			}
		}
		out[c] = float64(n) / float64(width*lines)
	}
	return out
}

func maxOf(v [3]float64) float64 {
	m := v[0]
	for _, x := range v {
		if x > m {
			m = x
		}
	}
	return m
}

func minOf(v [3]float64) float64 {
	m := v[0]
	for _, x := range v {
		if x < m {
			m = x
		}
	}
	return m
}

type cand struct {
	stride int
	cost   float64
}

type strideResult struct {
	stride   int
	cost     float64
	rival    float64 // best cost among strides far enough away to be a real rival
	distinct bool
	top      []cand
}

// findStride measures how rough the image is vertically for each candidate
// number of bytes per wire line. The true stride lines pixels up with the
// pixels above them, so it minimises the difference between adjacent lines;
// a wrong stride shears the image and the difference jumps.
func findStride(raw []byte, lo, hi int) strideResult {
	var all []cand
	for s := lo; s <= hi; s++ {
		if s%3 != 0 || len(raw) < s*6 {
			continue
		}
		n := len(raw)/s - 1
		if n > 200 {
			n = 200
		}
		sum, count := 0.0, 0
		for l := 0; l < n; l++ {
			a, b := raw[l*s:(l+1)*s], raw[(l+1)*s:(l+2)*s]
			for i := 0; i < len(a); i += 7 { // sparse sample: enough, and fast
				d := float64(a[i]) - float64(b[i])
				sum += d * d
				count++
			}
		}
		all = append(all, cand{s, math.Sqrt(sum / float64(count))})
	}
	if len(all) == 0 {
		return strideResult{}
	}
	bestIdx := 0
	for i, c := range all {
		if c.cost < all[bestIdx].cost {
			bestIdx = i
		}
	}
	winner := all[bestIdx]
	// Neighbouring strides shear the image only slightly and are expected to
	// score almost as well, so the rival has to sit further out to count.
	rival := math.Inf(1)
	for _, c := range all {
		if abs(c.stride-winner.stride) >= 9 && c.cost < rival {
			rival = c.cost
		}
	}
	top := make([]cand, 0, 5)
	sorted := append([]cand(nil), all...)
	for len(top) < 5 && len(sorted) > 0 {
		j := 0
		for i, c := range sorted {
			if c.cost < sorted[j].cost {
				j = i
			}
		}
		top = append(top, sorted[j])
		sorted = append(sorted[:j], sorted[j+1:]...)
	}
	return strideResult{
		stride:   winner.stride,
		cost:     winner.cost,
		rival:    rival,
		distinct: winner.cost < 0.8*rival,
		top:      top,
	}
}

// curveText renders the correlations either side of the chosen offset.
func curveText(m cx4300.LagMeasurement, c int) string {
	var parts []string
	for i, v := range m.Curve[c] {
		if s := m.CurveFrom + i; s >= -3 && s <= 3 {
			parts = append(parts, fmt.Sprintf("%+d:%.3f", s, v))
		}
	}
	return strings.Join(parts, "  ")
}

// applyLag mixes each row mean with the one above it, exactly as the decode
// mixes the pixels: a linear mix of lines is a linear mix of their means.
func applyLag(rows []float64, lag float64) []float64 {
	if lag == 0 {
		return rows
	}
	out := make([]float64, len(rows))
	for i := range rows {
		above := i - 1
		if above < 0 {
			above = 0
		}
		out[i] = (1-lag)*rows[i] + lag*rows[above]
	}
	return out
}

func rowMeans(raw []byte, stride, plane, width, lines, c int) []float64 {
	out := make([]float64, 0, lines)
	for y := 0; y < lines; y++ {
		base := y*stride + c*plane
		sum := 0
		for x := 0; x < width; x++ {
			sum += int(raw[base+x])
		}
		out = append(out, float64(sum)/float64(width))
	}
	return out
}

func correlate(a, b []float64, shift int) float64 {
	ma, mb := mean(a), mean(b)
	var num, da, db float64
	for i := range a {
		j := i + shift
		if j < 0 || j >= len(b) {
			continue
		}
		x, y := a[i]-ma, b[j]-mb
		num += x * y
		da += x * x
		db += y * y
	}
	if da == 0 || db == 0 {
		return 0
	}
	return num / math.Sqrt(da*db)
}

// describePeriod reports the strongest repeat in the row means between 2 and 64
// rows. A print halftone screen beaten against the scan grid shows up here as a
// clear short period; sensor or decode faults usually sit at 2 rows or at none.
//
// The means are high-passed first: real image content drifts slowly, and an
// autocorrelation of that drift is near 1.0 at every short lag, which would
// otherwise report a two-row period for any smooth picture. Only a lag that is
// a local peak counts, since a genuine period repeats rather than just decays.
func describePeriod(rows []float64) string {
	if len(rows) < 32 {
		return "capture too short"
	}
	d := highPass(rows, 9)
	ac := make([]float64, 0, 64)
	maxLag := 64
	if l := len(d) / 3; maxLag > l {
		maxLag = l
	}
	for lag := 1; lag <= maxLag; lag++ {
		ac = append(ac, correlate(d, d, lag))
	}
	bestLag, bestScore := 0, 0.0
	for i := 1; i < len(ac)-1; i++ {
		if ac[i] > ac[i-1] && ac[i] >= ac[i+1] && ac[i] > bestScore {
			bestLag, bestScore = i+1, ac[i]
		}
	}
	if bestLag == 0 || bestScore < 0.25 {
		return "none"
	}
	return fmt.Sprintf("%d rows (autocorrelation %.2f)", bestLag, bestScore)
}

// difference returns the row-to-row change, which removes slow drift without
// the phase error a windowed high-pass introduces.
func difference(v []float64) []float64 {
	if len(v) < 2 {
		return nil
	}
	out := make([]float64, len(v)-1)
	for i := range out {
		out[i] = v[i+1] - v[i]
	}
	return out
}

// highPass subtracts a centred moving average of the given width, leaving only
// variation faster than that window.
func highPass(v []float64, window int) []float64 {
	out := make([]float64, len(v))
	half := window / 2
	for i := range v {
		lo, hi := i-half, i+half
		if lo < 0 {
			lo = 0
		}
		if hi >= len(v) {
			hi = len(v) - 1
		}
		sum := 0.0
		for j := lo; j <= hi; j++ {
			sum += v[j]
		}
		out[i] = v[i] - sum/float64(hi-lo+1)
	}
	return out
}

// oddEvenColumns compares even and odd pixel columns. A CCD read out through
// two alternating taps with unequal gain shows a split here, which reads as
// vertical striping rather than horizontal.
func oddEvenColumns(raw []byte, stride, plane, width, lines int) string {
	var even, odd float64
	var ne, no int
	for y := 0; y < lines; y++ {
		for c := 0; c < 3; c++ {
			base := y*stride + c*plane
			for x := 0; x < width; x++ {
				if x%2 == 0 {
					even += float64(raw[base+x])
					ne++
				} else {
					odd += float64(raw[base+x])
					no++
				}
			}
		}
	}
	if ne == 0 || no == 0 {
		return "no data"
	}
	e, o := even/float64(ne), odd/float64(no)
	return fmt.Sprintf("even %.2f, odd %.2f, difference %+.2f", e, o, o-e)
}

// reportRegistration measures, in windows across the width, the horizontal
// shift that best aligns the green and blue planes onto the red one. A constant
// shift is a fixed registration offset; a shift that drifts steadily with x
// means the planes do not share a pitch, which is an assumption Deinterleave
// makes everywhere. Windows whose content is too flat to align are reported as
// such rather than given a meaningless answer.
func reportRegistration(raw []byte, stride, plane, width, lines int) {
	const window = 96
	fmt.Printf("  %-14s %-22s %-22s\n", "columns", "green vs red", "blue vs red")
	var xs, gs, bs []float64
	for x0 := 0; x0+window <= width; x0 += window {
		gdx, gok := columnShift(raw, stride, plane, lines, x0, window, 0, 1)
		bdx, bok := columnShift(raw, stride, plane, lines, x0, window, 0, 2)
		fmt.Printf("  %-14s %-22s %-22s\n",
			fmt.Sprintf("%d-%d", x0, x0+window-1), shiftText(gdx, gok), shiftText(bdx, bok))
		if gok && bok {
			xs = append(xs, float64(x0+window/2))
			gs = append(gs, gdx)
			bs = append(bs, bdx)
		}
	}
	if len(xs) < 3 {
		fmt.Println("  too few usable windows to tell a drift from a fixed offset")
		return
	}
	for _, c := range []struct {
		name string
		v    []float64
	}{{"green", gs}, {"blue", bs}} {
		slope, intercept := fitLine(xs, c.v)
		fmt.Printf("  %s: offset %+.2f px at the left edge, drifting %+.3f px per 100 px",
			c.name, intercept, slope*100)
		switch {
		case math.Abs(slope*float64(width)) > 1.0:
			fmt.Printf("  DRIFT: %+.1f px across the row - planes do not share a pitch\n",
				slope*float64(width))
		case math.Abs(intercept) >= 1.0:
			fmt.Printf("  fixed offset, no drift\n")
		default:
			fmt.Printf("  aligned\n")
		}
	}
}

func shiftText(dx float64, ok bool) string {
	if !ok {
		return "too flat to measure"
	}
	return fmt.Sprintf("%+.2f px", dx)
}

// columnShift aligns plane b onto plane a within one window of columns, using
// the column profile averaged down the whole capture. It interpolates the
// correlation peak against its neighbours, so a half-pixel misregistration is
// still visible. ok is false when the window carries too little detail for the
// peak to mean anything.
func columnShift(raw []byte, stride, plane, lines, x0, window, a, b int) (dx float64, ok bool) {
	pa := colProfile(raw, stride, plane, lines, x0, window, a)
	pb := colProfile(raw, stride, plane, lines, x0, window, b)
	if stddev(pa) < 1.0 || stddev(pb) < 1.0 {
		return 0, false
	}
	const span = 8
	best, bestScore := 0, -2.0
	scores := make(map[int]float64, 2*span+1)
	for s := -span; s <= span; s++ {
		v := correlate(pa, pb, s)
		scores[s] = v
		if v > bestScore {
			best, bestScore = s, v
		}
	}
	if bestScore < 0.3 || best == -span || best == span {
		return 0, false
	}
	// Parabolic interpolation through the peak and its two neighbours.
	l, r := scores[best-1], scores[best+1]
	denom := 2 * (2*bestScore - l - r)
	frac := 0.0
	if denom != 0 {
		frac = (r - l) / denom
	}
	return float64(best) + frac, true
}

func colProfile(raw []byte, stride, plane, lines, x0, window, c int) []float64 {
	out := make([]float64, window)
	for y := 0; y < lines; y++ {
		base := y*stride + c*plane
		for i := 0; i < window; i++ {
			out[i] += float64(raw[base+x0+i])
		}
	}
	for i := range out {
		out[i] /= float64(lines)
	}
	return out
}

// fitLine returns the least-squares slope and intercept of v against x.
func fitLine(x, v []float64) (slope, intercept float64) {
	mx, mv := mean(x), mean(v)
	var num, den float64
	for i := range x {
		num += (x[i] - mx) * (v[i] - mv)
		den += (x[i] - mx) * (x[i] - mx)
	}
	if den == 0 {
		return 0, mv
	}
	slope = num / den
	return slope, mv - slope*mx
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func mean(v []float64) float64 {
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func stddev(v []float64) float64 {
	m := mean(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)))
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
