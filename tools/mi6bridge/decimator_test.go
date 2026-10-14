package main

import (
	"math"
	"testing"
)

func toneRMS(t *testing.T, freq float64) float64 {
	d := newDecimator(48000, 8000)
	in := make([]int16, 48000)
	for i := range in {
		in[i] = int16(10000 * math.Sin(2*math.Pi*freq*float64(i)/48000))
	}
	out := make([]int16, 8000)
	for i := 0; i < len(in); i += 960 {
		d.process(in[i:i+960], out[i/6:i/6+160])
	}
	return rms(out[2000:])
}

func TestDecimatorPassesSpeechBand(t *testing.T) {
	if r := toneRMS(t, 1000); r < 6500 || r > 7500 {
		t.Fatalf("1 kHz rms %.0f, want ~7071", r)
	}
}

func TestDecimatorRejectsAboveNyquist(t *testing.T) {
	if r := toneRMS(t, 6000); r > 100 {
		t.Fatalf("6 kHz leaked: rms %.0f", r)
	}
}
