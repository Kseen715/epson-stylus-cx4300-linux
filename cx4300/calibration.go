package cx4300

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Calibration holds what has to be measured per scanner rather than derived
// from the protocol. The built-in values come from the unit this driver was
// written against; another unit may differ, which is what CalibrationPath and
// MeasurePlaneLag are for.
//
// It is deliberately not per resolution. Measured across 75, 150, 300 and 600
// dpi on one unit, the plane lag, the horizontal registration and the channel
// gains were the same at every resolution - the lag is in wire lines, not in
// inches - so one set of numbers serves them all. See PROTOCOL.md.
type Calibration struct {
	// PlaneRowLag is how far each colour plane trails red, in wire lines.
	// Red is the reference and is always 0.
	PlaneRowLag [3]float64 `json:"planeRowLag"`

	// Provenance, so an operator can tell a measured file from a hand-written
	// one, know what it was measured on, and see how well. None of it affects
	// decoding.
	MeasuredAt time.Time `json:"measuredAt,omitempty"`
	Device     string    `json:"device,omitempty"`
	// Confidence is the correlation the alignment reached, and Spread the
	// widest disagreement between the resolutions it was measured at - which
	// should be small, since the lag does not vary with resolution.
	Confidence [3]float64 `json:"confidence,omitempty"`
	Spread     [3]float64 `json:"spread,omitempty"`
}

// builtinCalibration is the fallback: measured on the unit this driver was
// developed against, at 300 and 600 dpi, over a document with table rules and
// text. Aligning each plane's differenced row means against red's gave green
// 0.15 and 0.18, blue 0.85 and 0.95.
var builtinCalibration = Calibration{PlaneRowLag: [3]float64{0, 0.17, 0.90}}

// BuiltinCalibration is the compiled-in fallback, for a frontend that wants to
// show what would be used if nothing were stored.
func BuiltinCalibration() Calibration { return builtinCalibration }

// active is read once per scan rather than per pixel, so replacing the
// calibration between scans needs no locking on the decode path.
var active atomic.Pointer[Calibration]

// ActiveCalibration is the calibration in force.
func ActiveCalibration() Calibration {
	if c := active.Load(); c != nil {
		return *c
	}
	return builtinCalibration
}

// UseCalibration puts a calibration into force for scans started afterwards.
// It refuses one it cannot decode with, so a corrupt file cannot quietly
// produce wrong colour.
func UseCalibration(c Calibration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	active.Store(&c)
	return nil
}

// Validate reports whether a calibration is usable. The lag has to stay inside
// one line because the streaming decode keeps exactly one line of history.
func (c Calibration) Validate() error {
	if c.PlaneRowLag[0] != 0 {
		return fmt.Errorf("cx4300: red is the reference plane, its lag must be 0, got %v",
			c.PlaneRowLag[0])
	}
	for i, lag := range c.PlaneRowLag {
		if math.IsNaN(lag) || lag < 0 || lag >= 1 {
			return fmt.Errorf("cx4300: plane %d lag %v is outside [0,1)", i, lag)
		}
	}
	return nil
}

// PlaneRowLag reports the lag the decode is calibrated with, so a diagnostic
// can measure what a capture has left over after the correction rather than
// only what it started with.
func PlaneRowLag() [3]float64 { return ActiveCalibration().PlaneRowLag }

// lagWeights renders the lag as the 1/256 share of the previous wire line to
// mix into each plane, which is what keeps the resample integer.
func lagWeights(lag [3]float64) [3]int {
	var w [3]int
	for i, d := range lag {
		w[i] = int(math.Round(d * 256))
	}
	return w
}

// LoadCalibration reads a calibration file. A missing file is not an error -
// the built-in values are the documented fallback - so found reports whether
// there was one.
func LoadCalibration(path string) (c Calibration, found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return builtinCalibration, false, nil
	}
	if err != nil {
		return builtinCalibration, false, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return builtinCalibration, false, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return builtinCalibration, false, fmt.Errorf("%s: %w", path, err)
	}
	return c, true, nil
}

// Save writes a calibration, creating its directory. It is written and then
// renamed, so an interrupted write cannot leave a half-parsed file in place of
// a good one.
func (c Calibration) Save(path string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// LagMeasurement is one measurement of the plane lag, per plane.
type LagMeasurement struct {
	Lag [3]float64
	// Measured is false for a plane the capture could not decide, which is not
	// the same as a lag of zero: a flat or featureless scan has nothing to
	// align. A calibration keeps its previous value for those.
	Measured [3]bool
	// Confidence is the correlation at the chosen offset, and Curve the
	// correlations either side of it, for a diagnostic to show.
	Confidence [3]float64
	Curve      [3][]float64
	// CurveFrom is the offset Curve[0] corresponds to.
	CurveFrom int
}

// lagSpan is how far either side of zero the measurement looks. The lag is
// known to be well under a line; this is wide enough that a real peak is never
// at the edge, which is what lets an edge result be rejected.
const lagSpan = 6

// PlaneRowMeans is each colour plane's mean value per wire line, with the
// padding columns left out. It is what the alignment works on, and is exported
// so a caller can apply a correction to it and measure what is left - a linear
// mix of lines is a linear mix of their means, so that needs no second decode.
func PlaneRowMeans(raw []byte, width, dpi int) [3][]float64 {
	plane := PlaneStride(width, dpi)
	stride := plane * 3
	lines := len(raw) / stride
	var rows [3][]float64
	for c := 0; c < 3; c++ {
		rows[c] = make([]float64, lines)
		for y := 0; y < lines; y++ {
			base := y*stride + c*plane
			sum := 0
			for x := 0; x < width; x++ {
				sum += int(raw[base+x])
			}
			rows[c][y] = float64(sum) / float64(width)
		}
	}
	return rows
}

// ApplyLagToRowMeans mixes each row mean with the one above it, exactly as the
// decode mixes the pixels, so a caller can measure what a correction leaves
// behind without decoding the capture twice.
func ApplyLagToRowMeans(rows []float64, lag float64) []float64 {
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

// MeasurePlaneLag measures, from one raw capture, how far green and blue trail
// red. It needs horizontal detail - table rules, text baselines, anything with
// edges across the page - and reports Measured false rather than a guess when
// the capture has none.
func MeasurePlaneLag(raw []byte, width, dpi int) LagMeasurement {
	return MeasurePlaneLagFrom(PlaneRowMeans(raw, width, dpi))
}

// AverageLag combines several measurements of the same scanner into one, over
// the planes each of them managed to measure. Weighting by confidence lets a
// capture that aligned poorly count for less without having to be excluded by
// hand, and Spread - the widest disagreement between the measurements - is what
// says whether they actually agree.
func AverageLag(ms []LagMeasurement) (m LagMeasurement, spread [3]float64, n [3]int) {
	m.CurveFrom = -lagSpan
	m.Measured[0] = true
	for c := 1; c < 3; c++ {
		var sum, weight float64
		lo, hi := math.Inf(1), math.Inf(-1)
		for _, one := range ms {
			if !one.Measured[c] {
				continue
			}
			w := one.Confidence[c]
			if w <= 0 {
				continue
			}
			sum += one.Lag[c] * w
			weight += w
			lo = math.Min(lo, one.Lag[c])
			hi = math.Max(hi, one.Lag[c])
			n[c]++
		}
		if n[c] == 0 {
			continue
		}
		m.Lag[c] = sum / weight
		m.Confidence[c] = weight / float64(n[c])
		m.Measured[c] = true
		spread[c] = hi - lo
	}
	return m, spread, n
}

// StrideCandidate is one candidate number of bytes per wire line and how rough
// the image is vertically when decoded with it.
type StrideCandidate struct {
	Bytes     int
	Roughness float64
}

// StrideMeasurement is what a capture says about its own line length. The
// padding rule in PlaneStride was measured on one unit; this is how a caller
// checks it holds on the scanner in front of it, because a wrong line length
// shears the image and no other calibration means anything.
type StrideMeasurement struct {
	Expected int // what PlaneStride implies
	Best     int // the lowest-roughness candidate
	// Distinct is false when the minimum does not stand clearly below the rest,
	// which happens on content that repeats down the page. The measurement then
	// decides nothing, which is not the same as agreeing with Expected.
	Distinct bool
	Rival    float64 // the best roughness far enough away to be a real rival
	Top      []StrideCandidate
}

// Agrees reports whether the capture confirms the padding rule.
func (s StrideMeasurement) Agrees() bool { return s.Distinct && s.Best == s.Expected }

// MeasureLineStride sweeps candidate line lengths and picks the one that lines
// each wire line up with the one above it. The true length leaves the image
// vertically smooth; a wrong one shears it and the difference between adjacent
// lines jumps.
func MeasureLineStride(raw []byte, width, dpi int) StrideMeasurement {
	out := StrideMeasurement{Expected: PlaneStride(width, dpi) * 3}
	var all []StrideCandidate
	for bytes := width * 3; bytes <= width*3+400; bytes++ {
		if bytes%3 != 0 || len(raw) < bytes*6 {
			continue
		}
		lines := len(raw)/bytes - 1
		if lines > 200 {
			lines = 200
		}
		sum, count := 0.0, 0
		for l := 0; l < lines; l++ {
			a, b := raw[l*bytes:(l+1)*bytes], raw[(l+1)*bytes:(l+2)*bytes]
			for i := 0; i < len(a); i += 7 { // sparse sample: enough, and fast
				d := float64(a[i]) - float64(b[i])
				sum += d * d
				count++
			}
		}
		if count == 0 {
			continue
		}
		all = append(all, StrideCandidate{bytes, math.Sqrt(sum / float64(count))})
	}
	if len(all) == 0 {
		return out
	}

	winner := all[0]
	for _, c := range all {
		if c.Roughness < winner.Roughness {
			winner = c
		}
	}
	// Neighbouring lengths shear the image only slightly and are expected to
	// score almost as well, so a rival has to sit further out to count.
	rival := math.Inf(1)
	for _, c := range all {
		if c.Bytes-winner.Bytes >= 9 || winner.Bytes-c.Bytes >= 9 {
			rival = math.Min(rival, c.Roughness)
		}
	}
	sorted := append([]StrideCandidate(nil), all...)
	for len(out.Top) < 5 && len(sorted) > 0 {
		j := 0
		for i, c := range sorted {
			if c.Roughness < sorted[j].Roughness {
				j = i
			}
		}
		out.Top = append(out.Top, sorted[j])
		sorted = append(sorted[:j], sorted[j+1:]...)
	}
	out.Best, out.Rival = winner.Bytes, rival
	out.Distinct = winner.Roughness < 0.8*rival
	return out
}

// ChannelLevels is what a capture says about the three channels' gains.
type ChannelLevels struct {
	Mean    [3]float64
	Clipped [3]float64 // share of pixels at 255
	Spread  float64    // widest difference between the means
}

// MeasureChannelLevels reports the per-channel levels. The decode applies no
// gain correction - on the unit this driver was written against the three
// channels sit within 1.3 counts of each other, which is invisible - so this
// exists to catch a unit where that is not true, rather than to be stored.
//
// The verdict comes from the means, not from paper white: this device drives
// white to saturation, and clipped highlights read 255 in every channel however
// far apart the gains behind them are.
func MeasureChannelLevels(raw []byte, width, dpi int) ChannelLevels {
	plane := PlaneStride(width, dpi)
	stride := plane * 3
	lines := len(raw) / stride
	var out ChannelLevels
	if lines == 0 {
		return out
	}
	for c := 0; c < 3; c++ {
		sum, clipped := 0, 0
		for y := 0; y < lines; y++ {
			row := raw[y*stride+c*plane:]
			for x := 0; x < width; x++ {
				sum += int(row[x])
				if row[x] == 0xff {
					clipped++
				}
			}
		}
		n := float64(width * lines)
		out.Mean[c] = float64(sum) / n
		out.Clipped[c] = float64(clipped) / n
	}
	hi, lo := out.Mean[0], out.Mean[0]
	for _, v := range out.Mean {
		hi, lo = math.Max(hi, v), math.Min(lo, v)
	}
	out.Spread = hi - lo
	return out
}

// MeasurePlaneLagFrom measures from row means already computed, so a caller
// with its own row statistics - a diagnostic measuring what is left after a
// correction, say - shares this peak finding rather than repeating it.
func MeasurePlaneLagFrom(rows [3][]float64) LagMeasurement {
	m := LagMeasurement{CurveFrom: -lagSpan}
	m.Measured[0] = true

	// Differenced rather than high-passed with a window: a moving-average
	// high-pass whose width is near the content's own period distorts phase and
	// walks the peak off the true offset. Differencing has linear phase, and
	// its half-line delay is identical for both planes, so it cancels.
	ref := difference(rows[0])
	for c := 1; c < 3; c++ {
		lag, conf, curve, ok := alignSeries(ref, difference(rows[c]))
		m.Curve[c] = curve
		if !ok {
			continue
		}
		m.Lag[c], m.Confidence[c], m.Measured[c] = lag, conf, true
	}
	return m
}

// alignSeries finds the shift that best aligns b onto a, interpolating the peak
// against its neighbours so a fraction of a line still shows. It refuses to
// answer when either series is too flat to carry an answer, or when the
// correlation has no distinct peak - a flat curve means the capture cannot
// decide, which is a different statement from "the planes are aligned".
//
// The sign is flipped on the way out: a plane that trails red correlates best
// at a negative shift, and a lag is reported as a positive number of lines. A
// negative result is returned as measured rather than suppressed - it is what a
// correctly corrected capture leaves behind, and a diagnostic needs to see it.
// Whether a lag is one the decode can apply is Apply's business, not this.
func alignSeries(a, b []float64) (lag, confidence float64, curve []float64, ok bool) {
	curve = make([]float64, 2*lagSpan+1)
	if len(a) < 8 || len(b) < 8 || stddev(a) < 1.0 || stddev(b) < 1.0 {
		return 0, 0, curve, false
	}
	best, bestScore := 0, -2.0
	for s := -lagSpan; s <= lagSpan; s++ {
		v := correlate(a, b, s)
		curve[s+lagSpan] = v
		if v > bestScore {
			best, bestScore = s, v
		}
	}
	if bestScore < 0.3 || best == -lagSpan || best == lagSpan {
		return 0, bestScore, curve, false
	}
	l, r := curve[best+lagSpan-1], curve[best+lagSpan+1]
	if bestScore-math.Max(l, r) < 0.02 {
		return 0, bestScore, curve, false // no distinct peak
	}
	frac := 0.0
	if den := 2 * (2*bestScore - l - r); den != 0 {
		frac = (r - l) / den
	}
	return -(float64(best) + frac), bestScore, curve, true
}

// difference returns the line-to-line change, which removes slow drift without
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

// correlate is the normalised correlation of a against b, with b shifted.
func correlate(a, b []float64, shift int) float64 {
	ma, mb := seriesMean(a), seriesMean(b)
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

func seriesMean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func stddev(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := seriesMean(v)
	s := 0.0
	for _, x := range v {
		s += (x - m) * (x - m)
	}
	return math.Sqrt(s / float64(len(v)))
}

// Apply folds a measurement into a calibration and reports which planes it
// changed. A plane is left at its previous value when the capture could not
// decide it, and when the measurement falls outside the range the decode can
// apply - the streaming decode carries one line of history, so a lag of a whole
// line or more, or a negative one, is not something it can honour. Either way
// the previous value stands rather than a number that would not work.
func (c Calibration) Apply(m LagMeasurement, device string) (Calibration, [3]bool) {
	out := c
	var changed [3]bool
	for i := 1; i < 3; i++ {
		if !m.Measured[i] {
			continue
		}
		lag := math.Round(m.Lag[i]*100) / 100
		if lag < 0 || lag >= 1 {
			continue
		}
		out.PlaneRowLag[i] = lag
		changed[i] = true
	}
	out.MeasuredAt = time.Now().UTC().Truncate(time.Second)
	out.Device = device
	return out, changed
}
