package fm

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// These tests hold the port to M0LTE.FmChannel 0.7.0, through vectors
// tools/fmref generated from the published package.

const refRate = 48000

type refProfile struct {
	PeakDeviationHz, IfBandwidthHz                  float64
	TxAudioLowHz, TxAudioHighHz                     float64
	RxAudioLowHz, RxAudioHighHz                     float64
	PreEmphasisMicroseconds, DeEmphasisMicroseconds float64
	InterpolationFactor                             int
}

type reference struct {
	Profiles map[string]refProfile
	Sinad    map[string][]struct{ Cnr, SinadDb float64 } `json:"sinad_1khz_60pct"`
	Noise    map[string][]struct{ Cnr, NoiseDb float64 } `json:"unmodulated_noise"`
}

func loadReference(t *testing.T) reference {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "reference.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ref reference
	if err := json.Unmarshal(b, &ref); err != nil {
		t.Fatal(err)
	}
	// encoding/json matches sinad_db to SinadDb case-insensitively only
	// without the underscore, so map those by hand.
	var raw struct {
		Sinad map[string][]map[string]float64 `json:"sinad_1khz_60pct"`
		Noise map[string][]map[string]float64 `json:"unmodulated_noise"`
	}
	_ = json.Unmarshal(b, &raw)
	for name, rows := range raw.Sinad {
		for i, r := range rows {
			ref.Sinad[name][i].SinadDb = r["sinad_db"]
		}
	}
	for name, rows := range raw.Noise {
		for i, r := range rows {
			ref.Noise[name][i].NoiseDb = r["noise_db"]
		}
	}
	return ref
}

func readF32(t *testing.T, name string) []float32 {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func chainFor(p refProfile, noise bool, seed uint64) (*Transmitter, *Receiver) {
	tx := NewTransmitter(TxPath{
		AudioLowHz: p.TxAudioLowHz, AudioHighHz: p.TxAudioHighHz,
		PreEmphasisUs: p.PreEmphasisMicroseconds, DeviationPerUnitHz: p.PeakDeviationHz,
	}, refRate, p.InterpolationFactor)
	rx := NewReceiver(RxPath{
		IFBandwidthHz: p.IfBandwidthHz, AudioLowHz: p.RxAudioLowHz, AudioHighHz: p.RxAudioHighHz,
		DeEmphasisUs: p.DeEmphasisMicroseconds, FullScaleDeviationHz: p.PeakDeviationHz,
	}, refRate, p.InterpolationFactor, noise, seed)
	return tx, rx
}

// run streams audio through the link in router-sized blocks at one CNR.
func run(tx *Transmitter, rx *Receiver, audio []float32, cnrDB float64) []float32 {
	const block = 480
	amp := complex(Amplitude(cnrDB), 0)
	var out []float32
	var iq []complex128
	for off := 0; off < len(audio); off += block {
		end := min(off+block, len(audio))
		iq = tx.Process(audio[off:end], iq[:0])
		for i := range iq {
			iq[i] *= amp
		}
		out = rx.Process(iq, out)
	}
	return out
}

func TestNoiselessMatchesReference(t *testing.T) {
	ref := loadReference(t)
	in := readF32(t, "noiseless-in.f32")
	for name, p := range ref.Profiles {
		want := readF32(t, "noiseless-"+name+".f32")
		tx, rx := chainFor(p, false, 1)
		got := run(tx, rx, in, 0)
		var errPow, sigPow float64
		for i := 500; i < len(want); i++ {
			d := float64(got[i] - want[i])
			errPow += d * d
			sigPow += float64(want[i]) * float64(want[i])
		}
		db := 10 * math.Log10(errPow/sigPow)
		t.Logf("%s: error %.1f dB relative to the reference", name, db)
		if db > -40 {
			t.Errorf("%s: output differs from M0LTE.FmChannel by %.1f dB, want below -40", name, db)
		}
	}
}

func TestNoiseStatisticsMatchReference(t *testing.T) {
	if testing.Short() {
		t.Skip("statistical comparison takes a few seconds")
	}
	ref := loadReference(t)
	const seeds = 4
	n := refRate * 3 / 2
	tone := make([]float32, n)
	for i := range tone {
		tone[i] = float32(0.6 * math.Sin(2*math.Pi*1000*float64(i)/refRate))
	}
	silence := make([]float32, n)
	for name, p := range ref.Profiles {
		for i, row := range ref.Sinad[name] {
			var s, q float64
			for seed := uint64(1); seed <= seeds; seed++ {
				tx, rx := chainFor(p, true, seed)
				s += sinad(run(tx, rx, tone, row.Cnr))
				tx, rx = chainFor(p, true, seed+100)
				q += meanSquare(run(tx, rx, silence, row.Cnr))
			}
			gotS := s / seeds
			gotQ := 10 * math.Log10(q/seeds)
			wantQ := ref.Noise[name][i].NoiseDb
			// Tolerances: the two use different random generators, so
			// each side carries its own sampling error on 4 seeds, and
			// that error grows where the curve is steep (a seed's clicks
			// move it most there). Measured on mic8k at CNR 22, where the
			// curve climbs 2.6 dB per dB: one seed scatters by 1.3 dB, a
			// 4-seed mean by 0.66.
			tol := 0.8 + 0.4*slope(ref.Sinad[name], i)
			if math.Abs(gotS-row.SinadDb) > tol {
				t.Errorf("%s at CNR %g: SINAD %.2f dB, reference %.2f", name, row.Cnr, gotS, row.SinadDb)
			}
			if math.Abs(gotQ-wantQ) > 0.8 {
				t.Errorf("%s at CNR %g: unmodulated noise %.2f dB, reference %.2f", name, row.Cnr, gotQ, wantQ)
			}
		}
	}
}

// slope is the reference SINAD curve's local gradient, dB per dB of CNR.
func slope(rows []struct{ Cnr, SinadDb float64 }, i int) float64 {
	a, b := max(i-1, 0), min(i+1, len(rows)-1)
	return math.Abs(rows[b].SinadDb-rows[a].SinadDb) / (rows[b].Cnr - rows[a].Cnr)
}

// window, sinad and meanSquare mirror tools/fmref exactly.
func window(n int) (int, int) { return refRate * 3 / 10, n - refRate/20 }

func sinad(x []float32) float64 {
	a, b := window(len(x))
	var ss, sc, cc, xs, xc, total, mean float64
	for i := a; i < b; i++ {
		mean += float64(x[i])
	}
	mean /= float64(b - a)
	for i := a; i < b; i++ {
		s, c := math.Sincos(2 * math.Pi * 1000 * float64(i) / refRate)
		v := float64(x[i]) - mean
		ss += s * s
		cc += c * c
		sc += s * c
		xs += v * s
		xc += v * c
		total += v * v
	}
	det := ss*cc - sc*sc
	A := (xs*cc - xc*sc) / det
	B := (xc*ss - xs*sc) / det
	var residual float64
	for i := a; i < b; i++ {
		s, c := math.Sincos(2 * math.Pi * 1000 * float64(i) / refRate)
		r := float64(x[i]) - mean - A*s - B*c
		residual += r * r
	}
	return 10 * math.Log10(total/residual)
}

func meanSquare(x []float32) float64 {
	a, b := window(len(x))
	var sum float64
	for i := a; i < b; i++ {
		sum += float64(x[i]) * float64(x[i])
	}
	return sum / float64(b-a)
}
