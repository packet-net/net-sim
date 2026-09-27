package config

import (
	"errors"
	"fmt"
	"math"

	"gopkg.in/yaml.v3"

	"github.com/packethacking/net-sim/internal/fm"
)

// Radio is a port's FM transceiver. Every field is optional; the defaults
// are a Tait TM8100/TM8200 from its datasheets (see internal/fm and
// docs/fm-channel.md). Fields where zero is a meaningful setting (flat
// emphasis, no limiter, a 0 dBFS receive level) are pointers, so an
// explicit zero isn't mistaken for "use the default".
type Radio struct {
	Channel          string   `yaml:"channel,omitempty"` // narrow | mid | wide
	Path             string   `yaml:"path,omitempty"`    // data | voice
	DeviationHz      float64  `yaml:"deviation_hz,omitempty"`
	LimitHz          *float64 `yaml:"limit_hz,omitempty"` // 0 = no limiter
	IFBandwidthHz    float64  `yaml:"if_bandwidth_hz,omitempty"`
	AudioLowHz       float64  `yaml:"audio_low_hz,omitempty"`
	AudioHighHz      float64  `yaml:"audio_high_hz,omitempty"`
	EmphasisUs       *float64 `yaml:"emphasis_us,omitempty"` // 0 = flat
	TxPowerW         *float64 `yaml:"tx_power_w,omitempty"`
	AntennaGainDBi   float64  `yaml:"antenna_gain_dbi,omitempty"`
	FeederLossDB     float64  `yaml:"feeder_loss_db,omitempty"`
	NoiseFigureDB    *float64 `yaml:"noise_figure_db,omitempty"`
	SiteNoise        string   `yaml:"site_noise,omitempty"` // none | quiet_rural | rural | residential | business
	FrequencyErrorHz float64  `yaml:"frequency_error_hz,omitempty"`
	RxLevelDBFS      *float64 `yaml:"rx_level_dbfs,omitempty"`
	HumNoiseDB       *float64 `yaml:"hum_noise_db,omitempty"`
	Squelch          Squelch  `yaml:"squelch,omitempty"`
}

// Squelch is written either as a preset name (open, country, city, hard)
// or as a mapping: { preset: city, open_ms: 30 } or
// { threshold_dbm: -110, hysteresis_db: 6, open_ms: 20, close_ms: 100 }.
// With a preset, hysteresis_db overrides the preset's.
type Squelch struct {
	Preset       string  `yaml:"preset,omitempty"`
	ThresholdDBm float64 `yaml:"threshold_dbm,omitempty"`
	HysteresisDB float64 `yaml:"hysteresis_db,omitempty"`
	OpenMS       float64 `yaml:"open_ms,omitempty"`
	CloseMS      float64 `yaml:"close_ms,omitempty"`
}

// UnmarshalYAML accepts the scalar preset form as well as the mapping.
func (s *Squelch) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		s.Preset = n.Value
		return nil
	}
	type plain Squelch
	var p plain
	if err := n.Decode(&p); err != nil {
		return err
	}
	*s = Squelch(p)
	return nil
}

func (r Radio) validate() error {
	switch r.Channel {
	case "", "narrow", "mid", "wide":
	default:
		return fmt.Errorf("channel %q (narrow, mid or wide)", r.Channel)
	}
	switch r.Path {
	case "", "data", "voice":
	default:
		return fmt.Errorf("path %q (data or voice)", r.Path)
	}
	switch r.SiteNoise {
	case "", "none", "quiet_rural", "rural", "residential", "business":
	default:
		return fmt.Errorf("site_noise %q (none, quiet_rural, rural, residential or business)", r.SiteNoise)
	}
	deref := func(p *float64) float64 {
		if p == nil {
			return 0
		}
		return *p
	}
	for name, v := range map[string]float64{
		"deviation_hz": r.DeviationHz, "limit_hz": deref(r.LimitHz), "if_bandwidth_hz": r.IFBandwidthHz,
		"audio_low_hz": r.AudioLowHz, "audio_high_hz": r.AudioHighHz, "emphasis_us": deref(r.EmphasisUs),
		"tx_power_w": deref(r.TxPowerW), "antenna_gain_dbi": r.AntennaGainDBi, "feeder_loss_db": r.FeederLossDB,
		"noise_figure_db": deref(r.NoiseFigureDB), "frequency_error_hz": r.FrequencyErrorHz,
		"rx_level_dbfs": deref(r.RxLevelDBFS), "hum_noise_db": deref(r.HumNoiseDB),
		"squelch threshold_dbm": r.Squelch.ThresholdDBm, "squelch hysteresis_db": r.Squelch.HysteresisDB,
		"squelch open_ms": r.Squelch.OpenMS, "squelch close_ms": r.Squelch.CloseMS,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a finite number", name)
		}
	}
	for name, v := range map[string]float64{
		"deviation_hz": r.DeviationHz, "limit_hz": deref(r.LimitHz), "if_bandwidth_hz": r.IFBandwidthHz,
		"audio_low_hz": r.AudioLowHz, "audio_high_hz": r.AudioHighHz, "emphasis_us": deref(r.EmphasisUs),
		"feeder_loss_db": r.FeederLossDB, "noise_figure_db": deref(r.NoiseFigureDB),
		"squelch hysteresis_db": r.Squelch.HysteresisDB,
		"squelch open_ms": r.Squelch.OpenMS, "squelch close_ms": r.Squelch.CloseMS,
	} {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if r.TxPowerW != nil && *r.TxPowerW <= 0 {
		return errors.New("tx_power_w must be above 0")
	}
	if r.HumNoiseDB != nil && *r.HumNoiseDB <= 0 {
		return errors.New("hum_noise_db must be above 0 (dB below a 60 % deviation tone; a large value such as 120 all but removes it)")
	}
	if r.RxLevelDBFS != nil && *r.RxLevelDBFS > 0 {
		return errors.New("rx_level_dbfs must be at or below 0")
	}
	sq := r.Squelch
	if sq.Preset != "" && sq.ThresholdDBm != 0 {
		return errors.New("squelch takes a preset or a threshold_dbm, not both")
	}
	if sq.Preset != "" {
		if _, err := fm.SquelchPreset(sq.Preset); err != nil {
			return err
		}
	}
	if sq.ThresholdDBm > 0 {
		return errors.New("squelch threshold_dbm must be negative (dBm)")
	}
	if (sq.Preset == "" || sq.Preset == "open") && sq.ThresholdDBm == 0 &&
		(sq.OpenMS != 0 || sq.CloseMS != 0 || sq.HysteresisDB != 0) {
		return errors.New("squelch open_ms, close_ms and hysteresis_db need a preset (country, city, hard) or a threshold_dbm; an open squelch has none")
	}

	// Check what the defaults fill in, not just what was written.
	f := r.FM()
	if f.DeviationHz > 20000 || f.IFBandwidthHz > 40000 || f.AudioHighHz > 20000 {
		return errors.New("deviation_hz, if_bandwidth_hz or audio_high_hz is beyond what a 48 kHz simulator can carry")
	}
	if f.AudioLowHz >= f.AudioHighHz {
		return fmt.Errorf("the audio passband is empty: audio_low_hz %g is not below audio_high_hz %g", f.AudioLowHz, f.AudioHighHz)
	}
	if math.Abs(f.FrequencyErrorHz) > f.IFBandwidthHz/2 {
		return fmt.Errorf("frequency_error_hz %g puts the radio outside its own %g Hz channel filter", f.FrequencyErrorHz, f.IFBandwidthHz)
	}
	return nil
}

// FM returns the fm.Radio this configures, with every default filled in.
func (r Radio) FM() fm.Radio {
	out := fm.Radio{
		Channel: fm.Channel(r.Channel), Path: fm.Path(r.Path),
		DeviationHz: r.DeviationHz, IFBandwidthHz: r.IFBandwidthHz,
		AudioLowHz: r.AudioLowHz, AudioHighHz: r.AudioHighHz,
		AntennaGainDBi: r.AntennaGainDBi, FeederLossDB: r.FeederLossDB,
		Site: fm.Site(r.SiteNoise), FrequencyErrorHz: r.FrequencyErrorHz,
	}
	sq := r.Squelch
	switch {
	case sq.ThresholdDBm != 0:
		out.Squelch = fm.Squelch{ThresholdDBm: sq.ThresholdDBm, HysteresisDB: sq.HysteresisDB}
	default:
		out.Squelch, _ = fm.SquelchPreset(sq.Preset)
		if sq.HysteresisDB != 0 && !out.Squelch.Open {
			out.Squelch.HysteresisDB = sq.HysteresisDB
		}
	}
	out.Squelch.OpenMS, out.Squelch.CloseMS = sq.OpenMS, sq.CloseMS
	out = out.Default()

	// Explicit values where zero means something are applied after the
	// defaults, so they aren't replaced by them.
	if r.LimitHz != nil {
		out.LimitHz = *r.LimitHz
	}
	if r.EmphasisUs != nil {
		out.EmphasisUs = *r.EmphasisUs
	}
	if r.TxPowerW != nil {
		out.TxPowerDBm = 10 * math.Log10(*r.TxPowerW*1000)
	}
	if r.NoiseFigureDB != nil {
		out.NoiseFigureDB = *r.NoiseFigureDB
	}
	if r.RxLevelDBFS != nil {
		out.RxLevelDBFS = *r.RxLevelDBFS
	}
	if r.HumNoiseDB != nil {
		out.HumNoiseDB = *r.HumNoiseDB
	}
	return out
}
