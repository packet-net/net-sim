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
// docs/fm-channel.md).
type Radio struct {
	Channel          string  `yaml:"channel,omitempty"` // narrow | mid | wide
	Path             string  `yaml:"path,omitempty"`    // data | voice
	DeviationHz      float64 `yaml:"deviation_hz,omitempty"`
	LimitHz          float64 `yaml:"limit_hz,omitempty"`
	IFBandwidthHz    float64 `yaml:"if_bandwidth_hz,omitempty"`
	AudioLowHz       float64 `yaml:"audio_low_hz,omitempty"`
	AudioHighHz      float64 `yaml:"audio_high_hz,omitempty"`
	EmphasisUs       float64 `yaml:"emphasis_us,omitempty"`
	TxPowerW         float64 `yaml:"tx_power_w,omitempty"`
	AntennaGainDBi   float64 `yaml:"antenna_gain_dbi,omitempty"`
	FeederLossDB     float64 `yaml:"feeder_loss_db,omitempty"`
	NoiseFigureDB    float64 `yaml:"noise_figure_db,omitempty"`
	SiteNoise        string  `yaml:"site_noise,omitempty"` // none | quiet_rural | rural | residential | business
	FrequencyErrorHz float64 `yaml:"frequency_error_hz,omitempty"`
	RxLevelDBFS      float64 `yaml:"rx_level_dbfs,omitempty"`
	HumNoiseDB       float64 `yaml:"hum_noise_db,omitempty"`
	Squelch          Squelch `yaml:"squelch,omitempty"`
}

// Squelch is written either as a preset name (open, country, city, hard)
// or as a mapping: { preset: city, open_ms: 30 } or
// { threshold_dbm: -110, hysteresis_db: 6, open_ms: 20, close_ms: 100 }.
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
	dec := func(v any) error { return n.Decode(v) }
	if err := dec(&p); err != nil {
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
	for name, v := range map[string]float64{
		"deviation_hz": r.DeviationHz, "limit_hz": r.LimitHz, "if_bandwidth_hz": r.IFBandwidthHz,
		"audio_low_hz": r.AudioLowHz, "audio_high_hz": r.AudioHighHz, "emphasis_us": r.EmphasisUs,
		"tx_power_w": r.TxPowerW, "antenna_gain_dbi": r.AntennaGainDBi, "feeder_loss_db": r.FeederLossDB,
		"noise_figure_db": r.NoiseFigureDB, "frequency_error_hz": r.FrequencyErrorHz,
		"rx_level_dbfs": r.RxLevelDBFS, "hum_noise_db": r.HumNoiseDB,
		"squelch threshold_dbm": r.Squelch.ThresholdDBm, "squelch hysteresis_db": r.Squelch.HysteresisDB,
		"squelch open_ms": r.Squelch.OpenMS, "squelch close_ms": r.Squelch.CloseMS,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("%s must be a finite number", name)
		}
	}
	for name, v := range map[string]float64{
		"deviation_hz": r.DeviationHz, "limit_hz": r.LimitHz, "if_bandwidth_hz": r.IFBandwidthHz,
		"audio_low_hz": r.AudioLowHz, "audio_high_hz": r.AudioHighHz, "emphasis_us": r.EmphasisUs,
		"tx_power_w": r.TxPowerW, "feeder_loss_db": r.FeederLossDB, "noise_figure_db": r.NoiseFigureDB,
		"hum_noise_db": r.HumNoiseDB, "squelch hysteresis_db": r.Squelch.HysteresisDB,
		"squelch open_ms": r.Squelch.OpenMS, "squelch close_ms": r.Squelch.CloseMS,
	} {
		if v < 0 {
			return fmt.Errorf("%s must not be negative", name)
		}
	}
	if r.DeviationHz > 20000 || r.IFBandwidthHz > 40000 || r.AudioHighHz > 20000 {
		return errors.New("deviation_hz, if_bandwidth_hz or audio_high_hz is beyond what a 48 kHz simulator can carry")
	}
	if r.AudioHighHz != 0 && r.AudioLowHz >= r.AudioHighHz {
		return errors.New("audio_low_hz must be below audio_high_hz")
	}
	if r.RxLevelDBFS > 0 {
		return errors.New("rx_level_dbfs must be at or below 0")
	}
	if r.Squelch.Preset != "" && r.Squelch.ThresholdDBm != 0 {
		return errors.New("squelch takes a preset or a threshold_dbm, not both")
	}
	if r.Squelch.Preset != "" {
		if _, err := fm.SquelchPreset(r.Squelch.Preset); err != nil {
			return err
		}
	}
	if r.Squelch.ThresholdDBm > 0 {
		return errors.New("squelch threshold_dbm must be negative (dBm)")
	}
	return nil
}

// FM returns the fm.Radio this configures, with every default filled in.
func (r Radio) FM() fm.Radio {
	out := fm.Radio{
		Channel: fm.Channel(r.Channel), Path: fm.Path(r.Path),
		DeviationHz: r.DeviationHz, LimitHz: r.LimitHz, IFBandwidthHz: r.IFBandwidthHz,
		AudioLowHz: r.AudioLowHz, AudioHighHz: r.AudioHighHz, EmphasisUs: r.EmphasisUs,
		AntennaGainDBi: r.AntennaGainDBi, FeederLossDB: r.FeederLossDB,
		NoiseFigureDB: r.NoiseFigureDB, Site: fm.Site(r.SiteNoise),
		FrequencyErrorHz: r.FrequencyErrorHz, RxLevelDBFS: r.RxLevelDBFS, HumNoiseDB: r.HumNoiseDB,
	}
	if r.TxPowerW > 0 {
		out.TxPowerDBm = 10 * math.Log10(r.TxPowerW*1000)
	}
	sq := r.Squelch
	switch {
	case sq.ThresholdDBm != 0:
		out.Squelch = fm.Squelch{ThresholdDBm: sq.ThresholdDBm, HysteresisDB: sq.HysteresisDB}
	default:
		out.Squelch, _ = fm.SquelchPreset(sq.Preset)
	}
	out.Squelch.OpenMS, out.Squelch.CloseMS = sq.OpenMS, sq.CloseMS
	return out.Default()
}
