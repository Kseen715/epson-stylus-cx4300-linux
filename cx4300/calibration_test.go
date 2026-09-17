package cx4300

import (
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// docWire builds a capture with each plane reading a row offset from red's,
// which is what the measurement has to recover.
//
// The content is band-limited - nothing faster than a six-row period - because
// that is what a real scan looks like once the optics have filtered it, and it
// is the condition under which shifting by interpolation means anything. Fed
// step edges instead, with energy far above the sampling rate, no interpolation
// reconstructs them and the exercise measures the fixture rather than the code.
func docWire(width, dpi, lines int, lag [3]float64) []byte {
	plane := PlaneStride(width, dpi)
	stride := plane * 3
	raw := make([]byte, stride*lines)
	// The y terms carry the detail the alignment works on; the x term gives the
	// rows some structure across the page without cancelling in their means.
	at := func(y float64, x int) byte {
		v := 140 +
			50*math.Sin(2*math.Pi*y/6.3) +
			30*math.Sin(2*math.Pi*y/17) +
			20*math.Sin(float64(x)/3)
		return byte(math.Max(0, math.Min(255, v)))
	}
	for y := 0; y < lines; y++ {
		for c := 0; c < 3; c++ {
			base := y*stride + c*plane
			for x := 0; x < plane; x++ {
				raw[base+x] = at(float64(y)+lag[c], x)
			}
		}
	}
	return raw
}

func TestMeasurePlaneLagRecoversTheOffsets(t *testing.T) {
	const width, dpi, lines = 64, 300, 260
	want := [3]float64{0, 0.25, 0.75}
	m := MeasurePlaneLag(docWire(width, dpi, lines, want), width, dpi)

	for c := 1; c < 3; c++ {
		if !m.Measured[c] {
			t.Fatalf("plane %d was not measured (confidence %.3f)", c, m.Confidence[c])
		}
		if diff := m.Lag[c] - want[c]; diff < -0.05 || diff > 0.05 {
			t.Errorf("plane %d lag %.3f, want about %.2f", c, m.Lag[c], want[c])
		}
	}
	if !m.Measured[0] || m.Lag[0] != 0 {
		t.Errorf("red is the reference: measured=%v lag=%v", m.Measured[0], m.Lag[0])
	}
}

// A featureless capture has nothing to align. Reporting a lag of zero there
// would be indistinguishable from "the planes are aligned", so it must report
// that it could not measure instead.
func TestMeasurePlaneLagRefusesAFlatCapture(t *testing.T) {
	const width, dpi, lines = 64, 300, 200
	plane := PlaneStride(width, dpi)
	raw := make([]byte, plane*3*lines)
	for i := range raw {
		raw[i] = 250 // blank platen
	}
	m := MeasurePlaneLag(raw, width, dpi)
	for c := 1; c < 3; c++ {
		if m.Measured[c] {
			t.Errorf("plane %d reported a lag of %.3f from a blank capture", c, m.Lag[c])
		}
	}
}

// The point of the calibration is that applying it brings the planes together.
// Measuring it is only the means, so this checks the end: a capture whose
// planes are offset must decode with far less difference between its channels
// once the measured lag is in force than with none at all.
func TestMeasuredLagReducesChannelMismatch(t *testing.T) {
	original := ActiveCalibration()
	t.Cleanup(func() { UseCalibration(original) })

	const width, dpi, lines = 64, 300, 200
	injected := [3]float64{0, 0.30, 0.85}
	raw := docWire(width, dpi, lines, injected)

	mismatch := func() float64 {
		img := Deinterleave(raw, width, lines, dpi, ModeColor).(*image.RGBA)
		sum, n := 0.0, 0
		// The first and last rows have no neighbour to mix, so skip them.
		for y := 2; y < lines-2; y++ {
			row := img.Pix[y*img.Stride:]
			for x := 0; x < width; x++ {
				sum += math.Abs(float64(row[x*4+0]) - float64(row[x*4+2]))
				n++
			}
		}
		return sum / float64(n)
	}

	if err := UseCalibration(Calibration{}); err != nil {
		t.Fatal(err)
	}
	before := mismatch()

	m := MeasurePlaneLag(raw, width, dpi)
	cal, changed := Calibration{}.Apply(m, "fixture")
	if !changed[1] || !changed[2] {
		t.Fatalf("the measurement did not produce both planes: %+v", m)
	}
	if err := UseCalibration(cal); err != nil {
		t.Fatal(err)
	}
	after := mismatch()

	if after > before/3 {
		t.Errorf("red to blue difference went from %.2f to %.2f; the measured "+
			"calibration %v should have removed most of it", before, after, cal.PlaneRowLag)
	}
}

func TestApplyKeepsUnmeasuredPlanes(t *testing.T) {
	base := Calibration{PlaneRowLag: [3]float64{0, 0.17, 0.90}}
	m := LagMeasurement{
		Lag:      [3]float64{0, 0.41, 0.77},
		Measured: [3]bool{true, true, false}, // blue could not be measured
	}
	got, changed := base.Apply(m, "test device")
	if got.PlaneRowLag[1] != 0.41 {
		t.Errorf("green is %v, want the measured 0.41", got.PlaneRowLag[1])
	}
	if got.PlaneRowLag[2] != 0.90 {
		t.Errorf("blue is %v; an unmeasured plane must keep its previous value", got.PlaneRowLag[2])
	}
	if !changed[1] || changed[2] {
		t.Errorf("changed = %v, want green only", changed)
	}
	if got.Device != "test device" || got.MeasuredAt.IsZero() {
		t.Error("provenance was not recorded")
	}
}

// A measurement outside the range the decode can honour must not be stored. A
// negative lag or one of a whole line or more is not applicable with the single
// line of history the streaming decode keeps, so the previous value stands.
func TestApplyRejectsUnusableMeasurements(t *testing.T) {
	base := Calibration{PlaneRowLag: [3]float64{0, 0.17, 0.90}}
	for _, lag := range []float64{-0.4, 1.0, 2.3} {
		m := LagMeasurement{
			Lag:      [3]float64{0, lag, lag},
			Measured: [3]bool{true, true, true},
		}
		got, changed := base.Apply(m, "test device")
		if changed[1] || changed[2] {
			t.Errorf("lag %v was stored", lag)
		}
		if got.PlaneRowLag != base.PlaneRowLag {
			t.Errorf("lag %v changed the calibration to %v", lag, got.PlaneRowLag)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("lag %v produced an unusable calibration: %v", lag, err)
		}
	}
}

func TestValidateRejectsUnusableCalibrations(t *testing.T) {
	for _, tc := range []struct {
		name string
		lag  [3]float64
	}{
		{"red not the reference", [3]float64{0.2, 0.1, 0.5}},
		{"a whole line", [3]float64{0, 0.1, 1.0}},
		{"more than a line", [3]float64{0, 0.1, 2.5}},
		{"negative", [3]float64{0, -0.1, 0.5}},
		{"not a number", [3]float64{0, math.NaN(), 0.5}},
	} {
		if err := (Calibration{PlaneRowLag: tc.lag}).Validate(); err == nil {
			t.Errorf("%s: accepted %v", tc.name, tc.lag)
		}
	}
	if err := (Calibration{PlaneRowLag: [3]float64{0, 0, 0.999}}).Validate(); err != nil {
		t.Errorf("rejected a usable calibration: %v", err)
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "calibration.json")

	// A missing file is the documented fallback, not a failure.
	got, found, err := LoadCalibration(path)
	if err != nil || found {
		t.Fatalf("missing file: found=%v err=%v", found, err)
	}
	if got != BuiltinCalibration() {
		t.Errorf("missing file gave %v, want the built-in calibration", got)
	}

	want := Calibration{PlaneRowLag: [3]float64{0, 0.21, 0.88}, Device: "unit under test"}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, found, err = LoadCalibration(path)
	if err != nil || !found {
		t.Fatalf("after Save: found=%v err=%v", found, err)
	}
	if got.PlaneRowLag != want.PlaneRowLag || got.Device != want.Device {
		t.Errorf("round tripped to %+v, want %+v", got, want)
	}
	if left, _ := filepath.Glob(path + ".new"); len(left) != 0 {
		t.Error("the temporary file was left behind")
	}
}

// A corrupt or out-of-range file must not quietly produce wrong colour.
func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, body string }{
		{"not json", "{ this is not json"},
		{"lag beyond a line", `{"planeRowLag":[0,0.1,4]}`},
		{"red not the reference", `{"planeRowLag":[0.5,0.1,0.2]}`},
	} {
		path := filepath.Join(dir, tc.name+".json")
		if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		got, found, err := LoadCalibration(path)
		if err == nil {
			t.Errorf("%s: accepted %q", tc.name, tc.body)
		}
		if found || got != BuiltinCalibration() {
			t.Errorf("%s: fell back to %+v found=%v, want the built-in calibration",
				tc.name, got, found)
		}
	}
}

func TestUseCalibrationAffectsTheDecode(t *testing.T) {
	original := ActiveCalibration()
	t.Cleanup(func() { UseCalibration(original) })

	if err := UseCalibration(Calibration{PlaneRowLag: [3]float64{0, 0.1, 4}}); err == nil {
		t.Error("an unusable calibration was accepted")
	}

	// With no lag at all the decode must hand back the wire bytes untouched,
	// which is the clearest way to see the calibration reaching the decode.
	if err := UseCalibration(Calibration{}); err != nil {
		t.Fatalf("UseCalibration: %v", err)
	}
	const w, h, dpi = 2, 4, 300
	plane := PlaneStride(w, dpi)
	raw := make([]byte, plane*3*h)
	for y := 0; y < h; y++ {
		for c := 0; c < 3; c++ {
			for x := 0; x < plane; x++ {
				raw[y*plane*3+c*plane+x] = byte(30 * y)
			}
		}
	}
	img := Deinterleave(raw, w, h, dpi, ModeColor)
	for y := 0; y < h; y++ {
		r, _, _, _ := img.At(0, y).RGBA()
		if want := uint32(30 * y); r>>8 != want {
			t.Fatalf("row %d red = %d, want %d with no lag applied", y, r>>8, want)
		}
	}
}

// strideWire lays out a capture at the given line length in bytes, with content
// that is smooth across the page and drifts aperiodically down it. The stride
// sweep works by finding the length at which each line resembles the one above,
// so the fixture must not repeat down the page: content with a short vertical
// period resembles itself at many offsets, which is the case the sweep reports
// as inconclusive rather than answering.
func strideWire(lineBytes, lines int) []byte {
	raw := make([]byte, lineBytes*lines)
	drift := 0.0
	rng := 12345
	for y := 0; y < lines; y++ {
		rng = (rng*1103515245 + 12345) & 0x7fffffff
		drift += float64(rng%200-100) / 100 // a random walk: smooth, but never repeating
		for i := 0; i < lineBytes; i++ {
			v := 128 + 70*math.Sin(float64(i)/9) + 20*math.Sin(float64(i)/31) + drift
			raw[y*lineBytes+i] = byte(math.Max(0, math.Min(255, v)))
		}
	}
	return raw
}

func TestMeasureLineStrideConfirmsThePaddingRule(t *testing.T) {
	const width, dpi, lines = 100, 300, 120
	raw := strideWire(PlaneStride(width, dpi)*3, lines)
	m := MeasureLineStride(raw, width, dpi)

	if want := PlaneStride(width, dpi) * 3; m.Expected != want {
		t.Fatalf("Expected %d, want %d", m.Expected, want)
	}
	if !m.Agrees() {
		t.Errorf("did not confirm the padding rule: best %d expected %d distinct %v "+
			"(top %+v)", m.Best, m.Expected, m.Distinct, m.Top)
	}
	if len(m.Top) == 0 || m.Top[0].Bytes != m.Best {
		t.Errorf("Top does not lead with the winner: %+v", m.Top)
	}
}

// A unit that padded its lines differently would shear every image, and the
// calibration has to notice rather than measure colour on top of it. Feeding a
// capture whose real line length is not the expected one stands in for that.
func TestMeasureLineStrideDetectsAMismatch(t *testing.T) {
	const width, dpi, lines = 100, 300, 120
	expected := PlaneStride(width, dpi) * 3
	actual := expected + 12

	// Laid out at the wrong line length: only the real one lines the rows up.
	m := MeasureLineStride(strideWire(actual, lines), width, dpi)
	if m.Agrees() {
		t.Fatal("confirmed the padding rule for a capture that does not follow it")
	}
	if !m.Distinct || m.Best != actual {
		t.Errorf("measured %d (distinct %v), want the real %d", m.Best, m.Distinct, actual)
	}
}

func TestMeasureLineStrideReportsInconclusive(t *testing.T) {
	const width, dpi, lines = 100, 300, 120
	// Identical down the page: every candidate length lines up equally well,
	// so nothing can be concluded - which must not read as agreement.
	plane := PlaneStride(width, dpi)
	raw := make([]byte, plane*3*lines)
	for y := 0; y < lines; y++ {
		for i := 0; i < plane*3; i++ {
			raw[y*plane*3+i] = byte(i % 251)
		}
	}
	m := MeasureLineStride(raw, width, dpi)
	if m.Distinct {
		t.Errorf("content identical down the page gave a distinct answer of %d", m.Best)
	}
	if m.Agrees() {
		t.Error("an inconclusive measurement must not count as confirming the rule")
	}
}

func TestAverageLagWeightsAndReportsSpread(t *testing.T) {
	ms := []LagMeasurement{
		{Lag: [3]float64{0, 0.20, 0.80}, Measured: [3]bool{true, true, true},
			Confidence: [3]float64{1, 0.9, 0.9}},
		{Lag: [3]float64{0, 0.30, 0.90}, Measured: [3]bool{true, true, false},
			Confidence: [3]float64{1, 0.9, 0.1}},
		{Lag: [3]float64{0, 0.40, 0.70}, Measured: [3]bool{true, false, true},
			Confidence: [3]float64{1, 0.0, 0.9}},
	}
	m, spread, counts := AverageLag(ms)

	if counts[1] != 2 || counts[2] != 2 {
		t.Fatalf("counts %v, want 2 usable measurements per plane", counts)
	}
	// Green: 0.20 and 0.30 at equal confidence.
	if diff := m.Lag[1] - 0.25; diff < -0.001 || diff > 0.001 {
		t.Errorf("green averaged to %.4f, want 0.25", m.Lag[1])
	}
	// Blue: 0.80 and 0.70; the 0.90 was not measured and must not count.
	if diff := m.Lag[2] - 0.75; diff < -0.001 || diff > 0.001 {
		t.Errorf("blue averaged to %.4f, want 0.75", m.Lag[2])
	}
	if diff := spread[1] - 0.10; diff < -0.001 || diff > 0.001 {
		t.Errorf("green spread %.4f, want 0.10", spread[1])
	}
	if diff := spread[2] - 0.10; diff < -0.001 || diff > 0.001 {
		t.Errorf("blue spread %.4f, want 0.10", spread[2])
	}
}

// Weighting by confidence has to actually move the answer, or a capture that
// aligned poorly counts as much as one that aligned well.
func TestAverageLagPrefersTheConfidentMeasurement(t *testing.T) {
	m, _, _ := AverageLag([]LagMeasurement{
		{Lag: [3]float64{0, 0.9, 0}, Measured: [3]bool{true, true, false},
			Confidence: [3]float64{1, 0.99, 0}},
		{Lag: [3]float64{0, 0.1, 0}, Measured: [3]bool{true, true, false},
			Confidence: [3]float64{1, 0.01, 0}},
	})
	if m.Lag[1] < 0.85 {
		t.Errorf("green averaged to %.3f; the 0.99-confidence 0.9 should dominate "+
			"the 0.01-confidence 0.1", m.Lag[1])
	}
}

func TestAverageLagOfNothingMeasuresNothing(t *testing.T) {
	m, _, counts := AverageLag([]LagMeasurement{
		{Measured: [3]bool{true, false, false}},
	})
	for c := 1; c < 3; c++ {
		if m.Measured[c] || counts[c] != 0 {
			t.Errorf("plane %d reported a result from no measurements", c)
		}
	}
}

func TestMeasureChannelLevels(t *testing.T) {
	const width, dpi, lines = 32, 300, 40
	plane := PlaneStride(width, dpi)
	raw := make([]byte, plane*3*lines)
	want := [3]byte{100, 140, 255}
	for y := 0; y < lines; y++ {
		for c := 0; c < 3; c++ {
			for x := 0; x < plane; x++ {
				raw[y*plane*3+c*plane+x] = want[c]
			}
		}
	}
	l := MeasureChannelLevels(raw, width, dpi)
	for c := 0; c < 3; c++ {
		if diff := l.Mean[c] - float64(want[c]); diff < -0.5 || diff > 0.5 {
			t.Errorf("channel %d mean %.2f, want %d", c, l.Mean[c], want[c])
		}
	}
	if l.Spread != 155 {
		t.Errorf("spread %.1f, want 155", l.Spread)
	}
	if l.Clipped[2] != 1 || l.Clipped[0] != 0 {
		t.Errorf("clipped %v, want blue fully saturated and red not at all", l.Clipped)
	}
}

// The row means are what every alignment works on, and applying a correction to
// them has to match applying it to the pixels - that equivalence is what lets
// the calibration verify itself without decoding twice.
func TestApplyLagToRowMeansMatchesTheDecode(t *testing.T) {
	const width, dpi, lines = 8, 300, 6
	plane := PlaneStride(width, dpi)
	raw := make([]byte, plane*3*lines)
	for y := 0; y < lines; y++ {
		for c := 0; c < 3; c++ {
			for x := 0; x < plane; x++ {
				raw[y*plane*3+c*plane+x] = byte(20 + 30*y)
			}
		}
	}
	const lag = 0.75
	got := ApplyLagToRowMeans(PlaneRowMeans(raw, width, dpi)[0], lag)
	for y := 0; y < lines; y++ {
		above := y - 1
		if above < 0 {
			above = 0
		}
		want := (1-lag)*float64(20+30*y) + lag*float64(20+30*above)
		if diff := got[y] - want; diff < -0.01 || diff > 0.01 {
			t.Errorf("row %d = %.3f, want %.3f", y, got[y], want)
		}
	}
}
