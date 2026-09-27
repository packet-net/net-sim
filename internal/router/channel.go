package router

import (
	"math"
	"math/cmplx"
	"math/rand/v2"
	"sort"
	"sync/atomic"
	"time"

	"github.com/packethacking/net-sim/internal/audio"
	"github.com/packethacking/net-sim/internal/config"
	"github.com/packethacking/net-sim/internal/fm"
)

// The FM channel: each port is a radio. Its transmitter frequency-modulates
// the TNC's audio as the txReader receives it, and the modulated carrier
// (unit amplitude, at the IF rate) is what the link queues carry. Each
// receiving port's rxFeeder sums the carriers reaching it at their
// received levels and frequency offsets, and its receiver adds noise and
// demodulates. Quieting, the threshold, clicks, capture and collisions all
// come out of that; nothing here decides them. See internal/fm and
// docs/fm-channel.md.

// station is one port's radio.
type station struct {
	radio    fm.Radio
	tx       *fm.Transmitter
	rx       *fm.Receiver
	gain     float64 // receiver output scale, rated deviation -> RxLevelDBFS
	humSigma float64 // hum and noise floor, output units
	floorDBm float64 // receiver noise in its IF bandwidth
	hum      *rand.Rand

	// keyedUntil is when this radio's own transmission leaves the air, in
	// wall-clock UnixNano: its receiver is muted until then (half duplex).
	keyedUntil atomic.Int64

	// Receive side, touched only by the port's rxFeeder.
	iq      []complex128
	demod   []float32
	sqOpen  bool
	sqCount int // consecutive blocks pushing the squelch towards changing state
}

// linkRF is a link's radio path, fixed at start.
type linkRF struct {
	rxDBm   float64
	amp     float64    // carrier amplitude relative to the receiver's unit noise
	rotStep complex128 // per-IF-sample rotation for the transmitter/receiver frequency offset
	rot     complex128 // rxFeeder state
}

// newStations builds every port's radio and returns the IF oversampling
// factor they share (the widest any transmitter needs).
func newStations(cfg *config.Config) (map[config.PortRef]*station, int) {
	stations := map[config.PortRef]*station{}
	factor := 2
	i := uint64(0)
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			i++
			ref := config.PortRef{NodeID: n.ID, PortID: p.ID}
			radio := p.Radio.FM()
			f := fm.InterpolationFactor(audio.SampleRate, math.Max(radio.DeviationHz, radio.LimitHz), radio.AudioHighHz)
			factor = max(factor, f)
			stations[ref] = &station{radio: radio, hum: rand.New(rand.NewPCG(i, 0x68756d))}
		}
	}
	i = 0
	for _, n := range cfg.Nodes {
		for _, p := range n.Ports {
			i++
			st := stations[config.PortRef{NodeID: n.ID, PortID: p.ID}]
			r := st.radio
			st.tx = fm.NewTransmitter(r.TxPath(), audio.SampleRate, factor)
			st.rx = fm.NewReceiver(r.RxPath(), audio.SampleRate, factor, true, i)
			st.gain = r.OutputGain()
			// Hum and noise is quoted below a 60 % deviation 1 kHz tone,
			// which after OutputGain has amplitude 0.6 * 10^(RxLevelDBFS/20).
			toneRMS := 0.6 * math.Pow(10, r.RxLevelDBFS/20) / math.Sqrt2
			st.humSigma = toneRMS * math.Pow(10, -r.HumNoiseDB/20)
			st.floorDBm = r.NoiseFloorDBm(cfg.FrequencyMHz)
			st.sqOpen = r.Squelch.Open
		}
	}
	return stations, factor
}

// newLinkRF works out a link's received level and frequency offset.
func newLinkRF(from, to *station, pathLossDB float64, factor int) linkRF {
	rx := fm.ReceivedDBm(from.radio, to.radio, pathLossDB)
	cnr := rx - to.floorDBm
	offset := from.radio.FrequencyErrorHz - to.radio.FrequencyErrorHz
	ifRate := float64(audio.SampleRate * factor)
	return linkRF{
		rxDBm:   rx,
		amp:     fm.Amplitude(cnr),
		rotStep: cmplx.Exp(complex(0, 2*math.Pi*offset/ifRate)),
		rot:     1,
	}
}

// modulate turns one block of TX audio into its carrier, and marks the
// radio as on the air for another block.
func (st *station) modulate(blk audio.Block, period time.Duration) []complex128 {
	samples := make([]float32, len(blk)/2)
	for i := range samples {
		samples[i] = float32(int16(uint16(blk[2*i])|uint16(blk[2*i+1])<<8)) / 32768
	}
	now := time.Now().UnixNano()
	for {
		old := st.keyedUntil.Load()
		next := max(old, now) + int64(period)
		if st.keyedUntil.CompareAndSwap(old, next) {
			break
		}
	}
	return st.tx.Process(samples, make([]complex128, 0, len(samples)*st.tx.Factor()))
}

// heard is one carrier reaching a receiver this block.
type heard struct {
	rf *linkRF
	iq []complex128
}

// receive produces one block of this radio's receive audio from the
// carriers reaching it; openBlocks and closeBlocks are the squelch's
// delays. It reports whether the audio is open.
func (st *station) receive(carriers []heard, openBlocks, closeBlocks int) (audio.Block, bool) {
	out := audio.Silence()
	if time.Now().UnixNano() < st.keyedUntil.Load() {
		// Transmitting: a half-duplex radio's receiver hears nothing.
		return out, false
	}
	n := len(st.iq)
	clear(st.iq)
	totalMW := math.Pow(10, st.floorDBm/10)
	for _, c := range carriers {
		amp := complex(c.rf.amp, 0)
		rot, step := c.rf.rot, c.rf.rotStep
		for i := 0; i < n && i < len(c.iq); i++ {
			st.iq[i] += amp * rot * c.iq[i]
			rot *= step
		}
		c.rf.rot = rot / complex(cmplx.Abs(rot), 0)
		totalMW += math.Pow(10, c.rf.rxDBm/10)
	}
	st.demod = st.rx.Process(st.iq, st.demod[:0])

	if !st.radio.Squelch.Open {
		rssi := 10 * math.Log10(totalMW)
		want := st.sqOpen
		if st.sqOpen && rssi < st.radio.Squelch.ThresholdDBm-st.radio.Squelch.HysteresisDB {
			want = false
		} else if !st.sqOpen && rssi >= st.radio.Squelch.ThresholdDBm {
			want = true
		}
		if want != st.sqOpen {
			st.sqCount++
			delay := closeBlocks
			if want {
				delay = openBlocks
			}
			if st.sqCount > delay {
				st.sqOpen, st.sqCount = want, 0
			}
		} else {
			st.sqCount = 0
		}
		if !st.sqOpen {
			return out, false
		}
	}

	for i, v := range st.demod {
		x := float64(v)*st.gain + st.humSigma*st.hum.NormFloat64()
		x = math.Max(-1, math.Min(1, x))
		s := int16(math.Round(x * 32767))
		out[2*i], out[2*i+1] = byte(uint16(s)), byte(uint16(s)>>8)
	}
	return out, true
}

// idle is n blocks of this receiver's output with nothing on the channel:
// hiss with the squelch open, silence with it closed.
func (st *station) idle(n int) []byte {
	var out []byte
	open, close := msBlocks(st.radio.Squelch.OpenMS), msBlocks(st.radio.Squelch.CloseMS)
	for i := 0; i < n; i++ {
		blk, _ := st.receive(nil, open, close)
		out = append(out, blk...)
	}
	return out
}

// msBlocks converts a delay to whole blocks, rounding up.
func msBlocks(ms float64) int {
	if ms <= 0 {
		return 0
	}
	const blockMS = float64(audio.BlockSamples) * 1000 / audio.SampleRate
	return int(math.Ceil(ms / blockMS))
}

// decisionFor labels what a receiver had on its channel this block, for the
// event stream and the visualiser. It describes the RF situation; the
// audio itself comes from the receiver, not from this label. Two carriers
// within the capture ratio of Tait's measured co-channel rejection (about
// 6 dB) are labelled a collision.
func decisionFor(levels []float64) string {
	switch len(levels) {
	case 0:
		return "silence"
	case 1:
		return "single"
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(levels)))
	if levels[0]-levels[1] >= 6 {
		return "capture"
	}
	return "collision"
}
