package main

import (
	"math"
	"math/rand"
	"testing"
)

// scene builds three channels that carry genuinely different pictures, which is
// what a colour target gives and a black-on-white page does not.
func scene(n int) [3][]float64 {
	rng := rand.New(rand.NewSource(1))
	var g [3][]float64
	for c := range g {
		g[c] = make([]float64, n)
		for i := range g[c] {
			g[c][i] = 40*math.Sin(float64(i)/float64(7+c*5)) + 20*rng.Float64() + 120
		}
	}
	return g
}

func TestBestChannelMatchFindsIdentity(t *testing.T) {
	ref := scene(400)
	match, _ := bestChannelMatch(ref, ref)
	for c, got := range match {
		if got != c {
			t.Errorf("channel %d matched %d; an identical capture must map to itself", c, got)
		}
	}
}

func TestBestChannelMatchFindsASwap(t *testing.T) {
	ref := scene(400)
	// The test capture has red and blue exchanged, which is what a plane order
	// that changes with resolution would look like.
	swapped := [3][]float64{ref[2], ref[1], ref[0]}
	match, scores := bestChannelMatch(swapped, ref)
	want := [3]int{2, 1, 0}
	if match != want {
		t.Fatalf("mapping %v, want %v (scores %v)", match, want, scores)
	}
}

func TestChannelIndependenceFlagsANeutralTarget(t *testing.T) {
	// A black-on-white page: every channel carries the same picture.
	one := scene(400)[0]
	_, _, _, least := channelIndependence([3][]float64{one, one, one})
	if least <= 0.98 {
		t.Errorf("a neutral target scored %.4f; it must read as indistinguishable", least)
	}

	if _, _, _, least := channelIndependence(scene(400)); least > 0.98 {
		t.Errorf("a colour target scored %.4f; it must read as distinguishable", least)
	}
}

// resampleChannels has to box-average the planes down and apply the decode's
// plane lag, so that what is compared across resolutions is what the decode
// produces.
func TestResampleChannelsAveragesBoxes(t *testing.T) {
	const width, plane, lines, factor = 4, 16, 4, 2
	stride := plane * 3
	raw := make([]byte, stride*lines)
	for y := 0; y < lines; y++ {
		for c := 0; c < 3; c++ {
			for x := 0; x < plane; x++ {
				// Flat down the page, so the lag mix is a no-op and the box
				// average of each 2x2 is just the constant.
				raw[y*stride+c*plane+x] = byte(50 + c*40)
			}
		}
	}
	g := resampleChannels(raw, plane, width, factor, width/factor, lines/factor)
	for c := 0; c < 3; c++ {
		if len(g[c]) != (width/factor)*(lines/factor) {
			t.Fatalf("channel %d has %d cells, want %d", c, len(g[c]), 4)
		}
		for i, got := range g[c] {
			if want := float64(50 + c*40); math.Abs(got-want) > 0.51 {
				t.Errorf("channel %d cell %d = %.2f, want %.2f", c, i, got, want)
			}
		}
	}
}

// Two captures of the same page at different resolutions do not share an
// origin, so the comparison has to find the offset before it can say anything
// about colour.
func TestBestShiftRecoversAnOffset(t *testing.T) {
	const gw, gh = 20, 16
	ref := make([]float64, gw*gh)
	rng := rand.New(rand.NewSource(7))
	for i := range ref {
		ref[i] = rng.Float64() * 100
	}
	// test[(x,y)] = ref[(x+2, y+1)], so the search must report (+2,+1).
	test := make([]float64, gw*gh)
	for y := 0; y < gh; y++ {
		for x := 0; x < gw; x++ {
			sx, sy := x+2, y+1
			if sx < gw && sy < gh {
				test[y*gw+x] = ref[sy*gw+sx]
			}
		}
	}
	dx, dy := bestShift(test, ref, gw, gh, 3)
	if dx != 2 || dy != 1 {
		t.Errorf("recovered shift (%+d,%+d), want (+2,+1)", dx, dy)
	}
	if got := shiftPearson(test, ref, gw, gh, dx, dy); got < 0.99 {
		t.Errorf("correlation at the recovered shift is %.3f, want ~1", got)
	}
}
