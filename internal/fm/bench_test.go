package fm

import (
	"math"
	"testing"
)

// BenchmarkLink10ms is one router block (10 ms) through one transmitter
// and one receiver, noise on. b.N blocks; divide ns/op by 10 ms for the
// fraction of a core one receiving port costs.
func BenchmarkLink10ms(b *testing.B) {
	for _, c := range []struct {
		name          string
		dev, ifbw, hi float64
	}{{"12k5-data", 2500, 8000, 4000}, {"25k-data", 5000, 16000, 8000}} {
		b.Run(c.name, func(b *testing.B) {
			f := InterpolationFactor(refRate, c.dev, c.hi)
			tx := NewTransmitter(TxPath{AudioLowHz: 20, AudioHighHz: c.hi, DeviationPerUnitHz: c.dev}, refRate, f)
			rx := NewReceiver(RxPath{IFBandwidthHz: c.ifbw, AudioLowHz: 20, AudioHighHz: c.hi, FullScaleDeviationHz: c.dev}, refRate, f, true, 1)
			audio := make([]float32, 480)
			for i := range audio {
				audio[i] = float32(0.5 * math.Sin(2*math.Pi*1200*float64(i)/refRate))
			}
			var iq []complex128
			var out []float32
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				iq = tx.Process(audio, iq[:0])
				out = rx.Process(iq, out[:0])
			}
		})
	}
}
