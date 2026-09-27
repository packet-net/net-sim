package fm

import (
	"fmt"
	"math"
)

// Radio is one station's FM transceiver as the channel model sees it.
// Every zero field takes a default from Default, which is a Tait
// TM8100/TM8200 built from its datasheets; each default says where it came
// from, and the ones that aren't Tait's figures say so.
type Radio struct {
	Channel Channel // narrow (12.5 kHz), mid (20 kHz) or wide (25 kHz)
	Path    Path    // data (flat taps) or voice (microphone and speaker)

	DeviationHz   float64 // peak deviation from full-scale transmit audio (voice: a full-scale 1 kHz tone)
	LimitHz       float64 // transmit limiter ceiling; 0 = none
	IFBandwidthHz float64 // -6 dB total width of the receiver's channel filter
	AudioLowHz    float64
	AudioHighHz   float64
	EmphasisUs    float64 // pre-emphasis and de-emphasis time constant; 0 = flat

	TxPowerDBm     float64
	AntennaGainDBi float64
	FeederLossDB   float64
	NoiseFigureDB  float64
	Site           Site

	FrequencyErrorHz float64 // this radio's offset from the channel centre, transmit and receive

	RxLevelDBFS float64 // receive audio level of a signal at rated deviation (voice: at 1 kHz)
	HumNoiseDB  float64 // hum and noise floor below a 60 % deviation 1 kHz tone

	Squelch Squelch
}

// Channel is a Tait bandwidth class.
type Channel string

const (
	Narrow Channel = "narrow"
	Mid    Channel = "mid"
	Wide   Channel = "wide"
)

// Path is which of the radio's audio taps the TNC is on.
type Path string

const (
	// DataPath is the flat taps: transmit audio straight into the
	// modulator, receive audio straight off the discriminator, no emphasis,
	// no voice filtering, no limiter. What a 9600 data port gives, and on a
	// Tait the R1 and T13 taps (MMA-00005-05 p.56).
	DataPath Path = "data"
	// VoicePath is the microphone and speaker: pre- and de-emphasis, a
	// 300-3000 Hz passband and the transmit limiter.
	VoicePath Path = "voice"
)

// Site is where the receiving antenna is, which sets the man-made noise it
// hears (ITU-R P.372). On 2 m this is usually worth more than the
// receiver's own noise figure.
type Site string

const (
	SiteNone        Site = "none"
	SiteQuietRural  Site = "quiet_rural"
	SiteRural       Site = "rural"
	SiteResidential Site = "residential"
	SiteBusiness    Site = "business"
)

// Squelch is the receiver's mute. Open (the zero value) passes everything,
// hiss included, which is how packet stations normally run. Otherwise it is
// Tait's signal-strength squelch: open above ThresholdDBm, close again
// HysteresisDB below it.
type Squelch struct {
	Open         bool
	ThresholdDBm float64
	HysteresisDB float64
	OpenMS       float64 // delay before the audio opens
	CloseMS      float64 // hang after the signal drops
}

// SquelchPreset is one of Tait's signal-strength squelch presets
// (MMA-00072-03 p.14 for the thresholds, MMA-00056-09 for the hysteresis).
func SquelchPreset(name string) (Squelch, error) {
	switch name {
	case "open", "":
		return Squelch{Open: true}, nil
	case "country":
		return Squelch{ThresholdDBm: -115, HysteresisDB: 9}, nil
	case "city":
		return Squelch{ThresholdDBm: -113, HysteresisDB: 8}, nil
	case "hard":
		return Squelch{ThresholdDBm: -107, HysteresisDB: 4}, nil
	}
	return Squelch{}, fmt.Errorf("unknown squelch %q (open, country, city, hard, or a threshold)", name)
}

// RatedDeviationHz is 100 % modulation for a channel (MMA-00072-03 p.6).
func RatedDeviationHz(c Channel) float64 {
	switch c {
	case Mid:
		return 4000
	case Wide:
		return 5000
	}
	return 2500
}

// taitIFThreeDBHz is Tait's "total IF 3 dB bandwidth" (MMA-00005-05 p.73,
// Table 3.1).
func taitIFThreeDBHz(c Channel) float64 {
	switch c {
	case Mid:
		return 12000
	case Wide:
		return 12600
	}
	return 7800
}

// threeToSixDBShapeFactor converts Tait's -3 dB IF widths to this model's
// -6 dB parameter. It is M0LTE.FmChannel's assumption for a crystal-plus-
// FPGA cascade, not a Tait figure.
const threeToSixDBShapeFactor = 1.25

// fittedNoiseFigureDB is the receiver noise figure that puts this model's
// 12 dB SINAD point (1 kHz at 60 % deviation, voice path, unweighted) at
// Tait's measured -121 dBm (MMA-00072-03 p.14, band B1). Tait don't publish
// a noise figure; TestNoiseFigureFit re-derives these.
func fittedNoiseFigureDB(c Channel) float64 {
	switch c {
	case Mid:
		return 5.3
	case Wide:
		return 5.4
	}
	return 6.0
}

// humNoiseDB is the middle of Tait's measured hum and noise range
// (MMA-00072-03 p.13: NB 40-44, MB 44-47, WB 46-49 dB).
func humNoiseDB(c Channel) float64 {
	switch c {
	case Mid:
		return 45.5
	case Wide:
		return 47.5
	}
	return 42
}

// Default fills every zero field of r.
func (r Radio) Default() Radio {
	if r.Channel == "" {
		r.Channel = Narrow
	}
	if r.Path == "" {
		r.Path = DataPath
	}
	rated := RatedDeviationHz(r.Channel)
	if r.DeviationHz == 0 {
		r.DeviationHz = rated
	}
	if r.IFBandwidthHz == 0 {
		r.IFBandwidthHz = taitIFThreeDBHz(r.Channel) * threeToSixDBShapeFactor
	}
	if r.Path == VoicePath {
		// 300-3000 Hz and 6 dB/octave emphasis (MMA-00072-03 p.13, p.17).
		// Tait publish the slope but not the time constant; 750 us is the
		// usual narrowband figure and M0LTE.FmChannel's.
		if r.AudioLowHz == 0 {
			r.AudioLowHz = 300
		}
		if r.AudioHighHz == 0 {
			r.AudioHighHz = 3000
		}
		if r.EmphasisUs == 0 {
			r.EmphasisUs = 750
		}
		// The limiter clips at rated deviation (MMA-00072-03 p.17), after
		// pre-emphasis (MMA-00005-05 p.58).
		if r.LimitHz == 0 {
			r.LimitHz = rated
		}
	} else {
		// A discriminator can't give back more than half its IF.
		if r.AudioLowHz == 0 {
			r.AudioLowHz = 20
		}
		if r.AudioHighHz == 0 {
			r.AudioHighHz = r.IFBandwidthHz / 2
		}
	}
	if r.TxPowerDBm == 0 {
		r.TxPowerDBm = 10 * math.Log10(25*1000) // the 25 W models (MMA-00072-03 p.9, p.18)
	}
	if r.NoiseFigureDB == 0 {
		r.NoiseFigureDB = fittedNoiseFigureDB(r.Channel)
	}
	if r.Site == "" {
		r.Site = SiteResidential
	}
	if r.RxLevelDBFS == 0 {
		// Calibrated, not a Tait figure: it puts a narrow data radio's idle
		// open-squelch hiss at -16 dBFS, which is what radio1's
		// discriminator tap measured (pdn-soundmodem docs/dev/carrier-sense.md).
		// The sound card's gain is the one thing physics doesn't fix.
		r.RxLevelDBFS = -18
	}
	if r.HumNoiseDB == 0 {
		r.HumNoiseDB = humNoiseDB(r.Channel)
	}
	if !r.Squelch.Open && r.Squelch.ThresholdDBm == 0 {
		r.Squelch.Open = true
	}
	return r
}

// TxPath is the radio's transmit side for a Transmitter.
func (r Radio) TxPath() TxPath {
	return TxPath{
		AudioLowHz: r.AudioLowHz, AudioHighHz: r.AudioHighHz,
		PreEmphasisUs:      r.EmphasisUs,
		DeviationPerUnitHz: r.DeviationHz / emphasisGainAt1k(r.EmphasisUs),
		LimitHz:            r.LimitHz,
	}
}

// RxPath is the radio's receive side for a Receiver: full scale is rated
// deviation.
func (r Radio) RxPath() RxPath {
	return RxPath{
		IFBandwidthHz: r.IFBandwidthHz, AudioLowHz: r.AudioLowHz, AudioHighHz: r.AudioHighHz,
		DeEmphasisUs: r.EmphasisUs, FullScaleDeviationHz: RatedDeviationHz(r.Channel),
	}
}

// OutputGain scales the Receiver's output (rated deviation = 1.0) so a
// signal at rated deviation, at 1 kHz on the voice path, lands at
// RxLevelDBFS.
func (r Radio) OutputGain() float64 {
	return math.Pow(10, r.RxLevelDBFS/20) * emphasisGainAt1k(r.EmphasisUs)
}

// emphasisGainAt1k is the pre-emphasis network's gain at 1 kHz relative to
// DC. Pre-emphasis here is normalised at DC (it is the exact inverse of the
// one-pole de-emphasis), but a real radio's deviation is set, and its
// receive level quoted, with a 1 kHz tone, so both ends divide this out.
func emphasisGainAt1k(us float64) float64 {
	if us <= 0 {
		return 1
	}
	const rate = 48000.0
	a := emphasisCoefficient(rate, us)
	w := 2 * math.Pi * 1000 / rate
	// |a / (1 - (1-a) e^{-jw})| is the de-emphasis gain; pre is its inverse.
	re := 1 - (1-a)*math.Cos(w)
	im := (1 - a) * math.Sin(w)
	return math.Hypot(re, im) / a
}

// ThermalNoiseDBmPerHz is kT at 290 K.
const ThermalNoiseDBmPerHz = -174.0

// ExternalNoiseFigureDB is a site's man-made noise as a noise figure at a
// frequency (ITU-R P.372; M0LTE.FmChannel's LinkBudget).
func ExternalNoiseFigureDB(s Site, frequencyMHz float64) float64 {
	var c, d float64
	switch s {
	case SiteBusiness:
		c, d = 76.8, 27.7
	case SiteResidential:
		c, d = 72.5, 27.7
	case SiteRural:
		c, d = 67.2, 27.7
	case SiteQuietRural:
		c, d = 53.6, 28.6
	default:
		return 0
	}
	return c - d*math.Log10(frequencyMHz)
}

// NoiseFloorDBm is the noise at the receiver input in its IF bandwidth:
// thermal, times the combined receiver and site noise factors.
func (r Radio) NoiseFloorDBm(frequencyMHz float64) float64 {
	fExt := math.Pow(10, ExternalNoiseFigureDB(r.Site, frequencyMHz)/10)
	if r.Site == SiteNone || r.Site == "" {
		fExt = 1
	}
	fRx := math.Pow(10, r.NoiseFigureDB/10)
	system := 10 * math.Log10(math.Max(fExt+fRx-1, 1e-12))
	return ThermalNoiseDBmPerHz + 10*math.Log10(r.IFBandwidthHz) + system
}

// ReceivedDBm is the carrier at rx's input from tx over pathLossDB.
func ReceivedDBm(tx, rx Radio, pathLossDB float64) float64 {
	return tx.TxPowerDBm - tx.FeederLossDB + tx.AntennaGainDBi - pathLossDB + rx.AntennaGainDBi - rx.FeederLossDB
}
