package fm

import (
	"math"
	"testing"
)

// link runs audio from tx to rx at cnr dB and returns rx's audio, scaled
// to the radio's output level.
func link(tx, rx Radio, audio []float32, cnrDB float64, seed uint64) []float32 {
	f := InterpolationFactor(refRate, tx.DeviationHz, tx.AudioHighHz)
	t := NewTransmitter(tx.TxPath(), refRate, f)
	r := NewReceiver(rx.RxPath(), refRate, f, true, seed)
	out := run(t, r, audio, cnrDB)
	g := float32(rx.OutputGain())
	for i := range out {
		out[i] *= g
	}
	return out
}

func tone(hz, amplitude float64, seconds float64) []float32 {
	n := int(seconds * refRate)
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(amplitude * math.Sin(2*math.Pi*hz*float64(i)/refRate))
	}
	return x
}

// The defaulted radios' noise figures put the model's 12 dB SINAD point at
// Tait's measured sensitivity. The model's 20 dB SINAD point is logged
// against Tait's too; it comes out 3 to 6 dB better than the real radio,
// which is a known limitation of an ideal limiter-discriminator (see
// docs/fm-channel.md), not something to tune away.
func TestNoiseFigureFit(t *testing.T) {
	if testing.Short() {
		t.Skip("bisects SINAD against CNR; about 20 s")
	}
	tait20 := map[Channel]float64{Narrow: -111, Mid: -113, Wide: -113}
	for _, ch := range []Channel{Narrow, Mid, Wide} {
		radio := Radio{Channel: ch, Path: VoicePath}.Default()
		sig := tone(1000, 0.6, 2)
		cnrFor := func(target float64) float64 {
			lo, hi := -5.0, 40.0
			for it := 0; it < 14; it++ {
				mid := (lo + hi) / 2
				var s float64
				for seed := uint64(1); seed <= 6; seed++ {
					s += sinad(link(radio, radio, sig, mid, seed))
				}
				if s/6 < target {
					lo = mid
				} else {
					hi = mid
				}
			}
			return (lo + hi) / 2
		}
		c12, c20 := cnrFor(12), cnrFor(20)
		nf := -121 - (ThermalNoiseDBmPerHz + 10*math.Log10(radio.IFBandwidthHz)) - c12
		t.Logf("%s: 12 dB SINAD at CNR %.1f dB, noise figure %.1f dB; 20 dB SINAD predicted at %.1f dBm, Tait measured %.0f",
			ch, c12, nf, -121+c20-c12, tait20[ch])
		if math.Abs(nf-radio.NoiseFigureDB) > 0.5 {
			t.Errorf("%s: fitted noise figure is %.1f dB, the default is %.1f", ch, nf, radio.NoiseFigureDB)
		}
	}
}

// An open-squelch receiver is louder with nobody transmitting than with a
// data signal on the channel, and an unmodulated carrier quiets it most of
// all. That is the property the old model had backwards.
func TestOpenSquelchIsLoudestWhenIdle(t *testing.T) {
	radio := Radio{Channel: Narrow, Path: DataPath}.Default()
	data := make([]float32, refRate)
	// A 1200 baud-ish signal at 60 % deviation: alternate tones.
	for i := range data {
		hz := 1200.0
		if (i/40)%2 == 1 {
			hz = 2200
		}
		data[i] = float32(0.6 * math.Sin(2*math.Pi*hz*float64(i)/refRate))
	}
	strong := 40.0
	idle := meanSquare(link(radio, radio, make([]float32, refRate), -60, 1))
	keyed := meanSquare(link(radio, radio, data, strong, 1))
	carrier := meanSquare(link(radio, radio, make([]float32, refRate), strong, 1))
	db := func(x float64) float64 { return 10 * math.Log10(x) }
	t.Logf("idle %.1f dBFS, data %.1f dBFS, unmodulated carrier %.1f dBFS", db(idle), db(keyed), db(carrier))
	if !(idle > keyed && keyed > carrier) {
		t.Fatalf("want idle > data > unmodulated carrier, got %.1f, %.1f, %.1f dBFS", db(idle), db(keyed), db(carrier))
	}
	// The absolute idle level is calibrated to radio1's -15.9 dBFS through
	// the default receive level, so this only checks the calibration held.
	// The relative levels are the independent check: radio1 measured data
	// 9 dB under idle, and an unmodulated carrier 39 dB under.
	if math.Abs(db(idle)+15.9) > 1.5 {
		t.Errorf("idle hiss %.1f dBFS, want about -15.9 (radio1)", db(idle))
	}
	if d := db(idle) - db(keyed); d < 6 || d > 12 {
		t.Errorf("data sits %.1f dB under idle hiss, radio1 measured about 9", d)
	}
	if db(idle)-db(carrier) < 30 {
		t.Errorf("a strong unmodulated carrier quiets the receiver by only %.1f dB; radio1 measured 39", db(idle)-db(carrier))
	}
}

// Co-channel rejection: with the wanted signal a little above sensitivity,
// how far below it can an interferer be before the wanted SINAD falls to
// 14 dB? Tait's compliance limits (MMA-00072-03 p.13) are -12 dB narrow and
// -8 dB wide; measured radios manage -5.7 and -2.7 dB.
func TestCoChannelRejection(t *testing.T) {
	if testing.Short() {
		t.Skip("bisects interferer level; a few seconds")
	}
	for _, c := range []struct {
		ch          Channel
		limit, meas float64
	}{{Narrow, -12, -5.7}, {Wide, -8, -2.7}} {
		radio := Radio{Channel: c.ch, Path: VoicePath}.Default()
		f := InterpolationFactor(refRate, radio.DeviationHz, radio.AudioHighHz)
		wanted := tone(1000, 0.6, 1.5)
		unwanted := tone(400, 0.6, 1.5)
		wantedCNR := 20.0
		sinadAt := func(ratioDB float64) float64 {
			var s float64
			for seed := uint64(1); seed <= 4; seed++ {
				tw := NewTransmitter(radio.TxPath(), refRate, f)
				tu := NewTransmitter(radio.TxPath(), refRate, f)
				rx := NewReceiver(radio.RxPath(), refRate, f, true, seed)
				aw := complex(Amplitude(wantedCNR), 0)
				au := complex(Amplitude(wantedCNR+ratioDB), 0)
				var out []float32
				var iw, iu []complex128
				for off := 0; off < len(wanted); off += 480 {
					iw = tw.Process(wanted[off:off+480], iw[:0])
					iu = tu.Process(unwanted[off:off+480], iu[:0])
					for i := range iw {
						iw[i] = aw*iw[i] + au*iu[i]
					}
					out = rx.Process(iw, out)
				}
				s += sinad(out)
			}
			return s / 4
		}
		lo, hi := -40.0, 0.0
		for it := 0; it < 12; it++ {
			mid := (lo + hi) / 2
			if sinadAt(mid) > 14 {
				lo = mid
			} else {
				hi = mid
			}
		}
		rej := (lo + hi) / 2
		t.Logf("%s: co-channel rejection %.1f dB (Tait limit %.0f, measured %.1f)", c.ch, rej, c.limit, c.meas)
		if rej < c.limit {
			t.Errorf("%s: co-channel rejection %.1f dB is worse than Tait's compliance limit %.0f dB", c.ch, rej, c.limit)
		}
	}
}

func TestNoiseFloor(t *testing.T) {
	r := Radio{Channel: Narrow, Site: SiteNone}.Default()
	got := r.NoiseFloorDBm(145)
	want := -174 + 10*math.Log10(9750) + 6.0
	if math.Abs(got-want) > 0.01 {
		t.Errorf("narrow receiver with no site noise: floor %.2f dBm, want %.2f", got, want)
	}
	// Residential man-made noise at 145 MHz is about 12.6 dB of external
	// noise figure, which dominates a 6 dB receiver.
	res := Radio{Channel: Narrow}.Default().NoiseFloorDBm(145)
	if res-got < 6 || res-got > 9 {
		t.Errorf("residential site raises the floor by %.1f dB, want about 7.5", res-got)
	}
}
