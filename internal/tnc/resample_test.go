package tnc

import (
	"math"
	"testing"
)

// The filter is centred on each output instant, so output n should equal
// the input signal evaluated at time n/outRate exactly, bar filter error.
// Feeding a tone in odd-sized chunks also exercises the streaming state.
func TestResamplerTones(t *testing.T) {
	for _, rates := range [][2]int{{44100, 48000}, {48000, 44100}} {
		for _, hz := range []float64{300, 1200, 4800, 9600, 16000} {
			in, out := rates[0], rates[1]
			r := newResampler(in, out)
			src := make([]float32, in) // one second
			for i := range src {
				src[i] = float32(0.5 * math.Sin(2*math.Pi*hz*float64(i)/float64(in)))
			}
			var got []float32
			for off := 0; off < len(src); {
				n := min(137+off%91, len(src)-off)
				got = r.Process(src[off:off+n], got)
				off += n
			}
			if len(got) < out-resampleHalfWidth*2 || len(got) > out {
				t.Fatalf("%d->%d %g Hz: got %d samples for one second", in, out, hz, len(got))
			}
			// Skip the start-up transient (zero history) and compare.
			var errSq, sigSq float64
			for n := 200; n < len(got); n++ {
				want := 0.5 * math.Sin(2*math.Pi*hz*float64(n)/float64(out))
				d := float64(got[n]) - want
				errSq += d * d
				sigSq += want * want
			}
			snr := 10 * math.Log10(sigSq/errSq)
			if snr < 60 {
				t.Errorf("%d->%d %g Hz: signal to error %.1f dB, want >= 60", in, out, hz, snr)
			}
		}
	}
}

func TestResamplerRejectsAboveNyquist(t *testing.T) {
	// 23 kHz at 48k in is above the 22.05 kHz output Nyquist and must not
	// alias down into the band.
	r := newResampler(48000, 44100)
	src := make([]float32, 48000)
	for i := range src {
		src[i] = float32(0.5 * math.Sin(2*math.Pi*23000*float64(i)/48000))
	}
	got := r.Process(src, nil)
	var peak float64
	for _, v := range got[200:] {
		peak = math.Max(peak, math.Abs(float64(v)))
	}
	if db := 20 * math.Log10(peak/0.5); db > -60 {
		t.Errorf("23 kHz leaked through at %.1f dB, want below -60", db)
	}
}

func TestResamplerFlushDeliversTail(t *testing.T) {
	r := newResampler(48000, 44100)
	burst := make([]float32, 4800) // 100 ms
	for i := range burst {
		burst[i] = 0.5
	}
	got := r.Process(burst, nil)
	got = r.Flush(got)
	// 100 ms at 44.1 kHz is 4410 samples; with the tail flushed we should
	// have all of it plus at most the filter's run-out.
	if len(got) < 4410 {
		t.Fatalf("got %d samples after flush, want at least 4410", len(got))
	}
	if v := got[4400]; math.Abs(float64(v)-0.5) > 0.01 {
		t.Errorf("sample near the end of the burst is %g, want 0.5", v)
	}
	// After a flush the next burst starts from clean state.
	if n := len(r.buf); n != resampleHalfWidth-1 {
		t.Errorf("buffer holds %d samples after flush, want %d", n, resampleHalfWidth-1)
	}
}
