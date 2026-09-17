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
		dpi    = flag.Int("dpi", 300, "scan resolution")
		x      = flag.Int("x", 0, "area origin x, 1/600 inch")
		y      = flag.Int("y", 0, "area origin y, 1/600 inch")
		w      = flag.Int("w", 1200, "area width, 1/600 inch (1200 = 2 inch)")
		h      = flag.Int("h", 1200, "area height, 1/600 inch")
		in     = flag.String("in", "", "analyse this raw capture instead of scanning")
		out    = flag.String("out", "wirediag", "output prefix for .raw and .png")
		width  = flag.Int("width", 0, "pixel width of -in capture (default: from -w/-dpi)")
		height = flag.Int("height", 0, "pixel height of -in capture")
	)
	flag.Parse()

	p := cx4300.Params{DPI: *dpi, Area: cx4300.Area{X: *x, Y: *y, W: *w, H: *h}}
	pw, ph := p.Area.Pixels(*dpi)
	if *width != 0 {
		pw = *width
	}
	if *height != 0 {
		ph = *height
	}

	var raw []byte
	if *in != "" {
		b, err := os.ReadFile(*in)
		if err != nil {
			log.Fatal(err)
		}
		raw = b
		fmt.Printf("read %s: %d bytes\n", *in, len(raw))
	} else {
		if err := p.Validate(); err != nil {
			log.Fatal(err)
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
		b, cw, ch, err := dev.ScanRaw(p)
		if err != nil {
			log.Fatal(err)
		}
		raw, pw, ph = b, cw, ch
		if err := os.WriteFile(*out+".raw", raw, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s.raw: %d bytes\n", *out, len(raw))
	}

	assumed := cx4300.PlaneStride(pw, *dpi) * 3
	fmt.Printf("\nrequested %dx%d px at %d dpi\n", pw, ph, *dpi)
	fmt.Printf("decoder assumes %d bytes per wire line (plane stride %d px)\n",
		assumed, cx4300.PlaneStride(pw, *dpi))
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
	case best.stride != assumed:
		fmt.Printf("  MISMATCH: measured %d bytes, decoder uses %d, off by %d per line.\n",
			best.stride, assumed, best.stride-assumed)
		fmt.Printf("  Using the measured %d bytes for the checks below.\n", best.stride)
		stride = best.stride
	default:
		fmt.Printf("  measured %d bytes, matching the decoder.\n", best.stride)
	}

	plane := stride / 3
	if len(raw) < stride*8 {
		fmt.Println("\ncapture too short for the remaining checks")
		return
	}
	lines := len(raw) / stride

	fmt.Printf("\n-- vertical offset between colour planes --\n")
	fmt.Println("(a tri-linear CCD reads R, G and B on physically separate rows;")
	fmt.Println(" the decoder takes all three from the same wire line, so a non-zero")
	fmt.Println(" offset here is real colour misregistration - and a printed dither")
	fmt.Println(" sampled through it turns grey into a hue that rotates across the page)")
	fmt.Println(" needs horizontal detail - rules, text baselines - to measure at all")
	for _, pr := range [][2]int{{0, 1}, {0, 2}} {
		dy, ok, curve := planeOffset(raw, stride, plane, pw, lines, pr[0], pr[1])
		if !ok {
			fmt.Printf("  plane %d vs plane %d: too flat to measure - recapture over table rules or text\n",
				pr[0], pr[1])
			continue
		}
		fmt.Printf("  plane %d vs plane %d: best offset %+.2f rows\n", pr[0], pr[1], dy)
		fmt.Printf("      correlation %s\n", curve)
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

	img := cx4300.Deinterleave(raw, pw, ph, *dpi)
	if err := writePNG(*out+".png", img); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nwrote %s.png using the current decode\n", *out)
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

// planeOffset finds the vertical shift that best aligns plane b onto plane a,
// correlating their per-row means and interpolating the peak against its
// neighbours so a fraction of a row still shows. It refuses to answer when the
// rows carry too little contrast, or when the correlation has no distinct peak,
// because a flat curve there means the capture cannot decide - which is a
// different statement from "the planes are aligned".
func planeOffset(raw []byte, stride, plane, width, lines, a, b int) (dy float64, ok bool, curve string) {
	// Differenced rather than high-passed with a window: a moving-average
	// high-pass whose width is near the content's own period distorts phase and
	// walks the peak off the true offset. Differencing has linear phase, and its
	// half-row delay is identical for both planes, so it cancels here.
	ra := difference(rowMeans(raw, stride, plane, width, lines, a))
	rb := difference(rowMeans(raw, stride, plane, width, lines, b))
	if stddev(ra) < 1.0 || stddev(rb) < 1.0 {
		return 0, false, ""
	}
	const span = 6
	scores := make(map[int]float64, 2*span+1)
	best, bestScore := 0, -2.0
	var parts []string
	for s := -span; s <= span; s++ {
		v := correlate(ra, rb, s)
		scores[s] = v
		if v > bestScore {
			best, bestScore = s, v
		}
	}
	// Printed around the winner, so the peak the offset came from is visible
	// even when it sits several rows out.
	lo, hi := best-3, best+3
	if lo < -span {
		lo, hi = -span, -span+6
	}
	if hi > span {
		lo, hi = span-6, span
	}
	for s := lo; s <= hi; s++ {
		parts = append(parts, fmt.Sprintf("%+d:%.3f", s, scores[s]))
	}
	curve = strings.Join(parts, "  ")
	// A genuine alignment stands above its neighbours; a flat curve decides nothing.
	if bestScore < 0.3 || best == -span || best == span {
		return 0, false, curve
	}
	if margin := bestScore - math.Max(scores[best-1], scores[best+1]); margin < 0.02 {
		return 0, false, curve + "   (no distinct peak)"
	}
	l, r := scores[best-1], scores[best+1]
	denom := 2 * (2*bestScore - l - r)
	frac := 0.0
	if denom != 0 {
		frac = (r - l) / denom
	}
	return float64(best) + frac, true, curve
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
