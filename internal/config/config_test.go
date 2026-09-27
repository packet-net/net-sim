package config

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrictUnknownField(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        bogus: yes
links: []
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestModemMismatch(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: gfsk9600 }
        kiss_port: 8002
links:
  - from: a.vhf
    to:   b.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "modem mismatch") {
		t.Fatalf("expected modem mismatch error, got %v", err)
	}
}

func TestUnknownPort(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links:
  - from: a.vhf
    to:   ghost.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "unknown port") {
		t.Fatalf("expected unknown port error, got %v", err)
	}
}

func TestKissPortCollision(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "kiss_port 8001 used by both") {
		t.Fatalf("expected kiss_port collision error, got %v", err)
	}
}

func TestIL2PRequiresInnerAndFEC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200 }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "fec") {
		t.Fatalf("expected fec error, got %v", err)
	}
}

func TestIL2PRejectsBadFEC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200, fec: medium }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "fec=") {
		t.Fatalf("expected fec value error, got %v", err)
	}
}

func TestIL2PRejectsLegacyCRC(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: il2p, inner: afsk1200, crc: true }
        kiss_port: 8001
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "crc") {
		t.Fatalf("expected unknown-field error mentioning crc, got %v", err)
	}
}

func TestValidConfig(t *testing.T) {
	yaml := `
nodes:
  - id: rdg
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
  - id: bsg
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
links:
  - from: rdg.vhf
    to:   bsg.vhf
    path_loss_db: 120
  - from: bsg.vhf
    to:   rdg.vhf
    path_loss_db: 120
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if len(cfg.Links) != 2 {
		t.Errorf("links = %d, want 2", len(cfg.Links))
	}
}

func TestTimeScaleDefaultsToRealTime(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.TimeScale != 1.0 {
		t.Errorf("time_scale default = %g, want 1.0", cfg.TimeScale)
	}
}

func TestTimeScaleAccepted(t *testing.T) {
	yaml := `
time_scale: 8.0
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.TimeScale != 8.0 {
		t.Errorf("time_scale = %g, want 8.0", cfg.TimeScale)
	}
}

func TestTimeScaleRejectsSlowerThanRealTime(t *testing.T) {
	for _, scale := range []string{"0.5", "-2"} {
		yaml := `
time_scale: ` + scale + `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links: []
`
		_, err := Parse(strings.NewReader(yaml))
		if err == nil || !strings.Contains(err.Error(), "time_scale") {
			t.Fatalf("time_scale=%s: expected time_scale error, got %v", scale, err)
		}
	}
}

func TestSelfLoopRejected(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
links:
  - from: a.vhf
    to:   a.vhf
`
	_, err := Parse(strings.NewReader(yaml))
	if err == nil || !strings.Contains(err.Error(), "self-loop") {
		t.Fatalf("expected self-loop error, got %v", err)
	}
}

func TestMultiportNodeOK(t *testing.T) {
	yaml := `
nodes:
  - id: hub
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
      - id: uhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
links: []
`
	if _, err := Parse(strings.NewReader(yaml)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestPdnBackend(t *testing.T) {
	cfg := func(tnc, mode, extra string) string {
		return `
` + extra + `
nodes:
  - id: a
    ports:
      - { id: fm, tnc: ` + tnc + `, modem: { mode: ` + mode + ` }, kiss_port: 8001 }
  - id: b
    ports:
      - { id: fm, tnc: ` + tnc + `, modem: { mode: ` + mode + ` }, kiss_port: 8002 }
links:
  - { from: a.fm, to: b.fm, path_loss_db: 120 }
`
	}
	for _, c := range []struct {
		name, tnc, mode, extra, wantErr string
	}{
		{name: "pdn mode on pdn", tnc: "pdn", mode: "qpsk3600"},
		{name: "shared mode on pdn", tnc: "pdn", mode: "afsk1200"},
		{name: "pdn mode on samoyed", tnc: "samoyed", mode: "qpsk3600", wantErr: "need tnc: pdn"},
		{name: "il2p on pdn", tnc: "pdn", mode: "il2p", wantErr: "afsk1200-il2p-nocrc"},
		{name: "bad mode name", tnc: "pdn", mode: "'Q PSK'", wantErr: "not a pdn-soundmodem mode name"},
		{name: "plugin mode", tnc: "pdn", mode: "'x:y'", wantErr: "not a pdn-soundmodem mode name"},
		{name: "ardop", tnc: "pdn", mode: "ardop", wantErr: "virtual TNC"},
		{name: "scaled time", tnc: "pdn", mode: "qpsk3600", extra: "time_scale: 2", wantErr: "time_scale 1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse(strings.NewReader(cfg(c.tnc, c.mode, c.extra)))
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestPdnModeMismatchAcrossLink(t *testing.T) {
	yaml := `
nodes:
  - id: a
    ports:
      - { id: fm, tnc: pdn, modem: { mode: qpsk3600 }, kiss_port: 8001 }
  - id: b
    ports:
      - { id: fm, tnc: pdn, modem: { mode: c4fsk9600 }, kiss_port: 8002 }
links:
  - { from: a.fm, to: b.fm, path_loss_db: 120 }
`
	if _, err := Parse(strings.NewReader(yaml)); err == nil || !strings.Contains(err.Error(), "modem mismatch") {
		t.Fatalf("want modem mismatch, got %v", err)
	}
}

func TestIDsRejectPathsAndDots(t *testing.T) {
	for _, id := range []string{"x/../../escaped", "a.b", "with space"} {
		yaml := `
nodes:
  - id: "` + id + `"
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001 }
links: []
`
		if _, err := Parse(strings.NewReader(yaml)); err == nil || !strings.Contains(err.Error(), "may only contain") {
			t.Errorf("node id %q: want rejection, got %v", id, err)
		}
	}
}

func TestRejectsUnusableNumbers(t *testing.T) {
	base := `
nodes:
  - id: a
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001 }
  - id: b
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8002 }
`
	for _, bad := range []string{
		"time_scale: 1e9\n",
		"time_scale: .inf\n",
		"time_scale: .nan\n",
		"frequency_mhz: .nan\n",
		"links:\n  - { from: a.vhf, to: b.vhf, path_loss_db: .nan }\n",
	} {
		yml := base
		if strings.HasPrefix(bad, "links") {
			yml += bad
		} else {
			yml = bad + base + "links: []\n"
		}
		if _, err := Parse(strings.NewReader(yml)); err == nil {
			t.Errorf("%q: accepted, want an error", strings.TrimSpace(bad))
		}
	}
}

func TestRetiredKeysAreRefused(t *testing.T) {
	ports := `
nodes:
  - id: a
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001 PORTEXTRA }
  - id: b
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8002 }
links:
  - { from: a.vhf, to: b.vhf LINKEXTRA }
`
	for _, c := range []struct{ top, port, link, want string }{
		{top: "mixer_mode: fm_capture\n", link: ", path_loss_db: 120", want: "mixer_mode is no longer used"},
		{top: "capture_db: 6\n", link: ", path_loss_db: 120", want: "capture_db is no longer used"},
		{top: "collision_mode: noise\n", link: ", path_loss_db: 120", want: "collision_mode is no longer used"},
		{top: "default_noise_db: 40\n", link: ", path_loss_db: 120", want: "default_noise_db is no longer used"},
		{port: ", noise_db: 40", link: ", path_loss_db: 120", want: "noise_db is no longer used"},
		{link: ", loss_db: 10", want: "path_loss_db"},
		{link: ", path_loss_db: 120, noise_db: 20", want: "noise_db is no longer used"},
		{link: ", path_loss_db: 120, squelch_open_ms: 50", want: "squelch belongs to the receiving radio"},
		{link: "", want: "path_loss_db is required"},
	} {
		yml := c.top + strings.Replace(strings.Replace(ports, " PORTEXTRA", c.port, 1), " LINKEXTRA", c.link, 1)
		_, err := Parse(strings.NewReader(yml))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: want an error containing %q, got %v", strings.TrimSpace(c.top+c.port+c.link), c.want, err)
		}
	}
}

func TestRadioBlock(t *testing.T) {
	yml := `
frequency_mhz: 433
nodes:
  - id: a
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8001
        radio:
          channel: wide
          path: voice
          tx_power_w: 5
          site_noise: rural
          frequency_error_hz: 150
          squelch: city
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8002
        radio: { squelch: { threshold_dbm: -110, hysteresis_db: 6, open_ms: 20 } }
links:
  - { from: a.vhf, to: b.vhf, path_loss_db: 130 }
`
	cfg, err := Parse(strings.NewReader(yml))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.FrequencyMHz != 433 || *cfg.Links[0].PathLossDB != 130 {
		t.Errorf("frequency %g, path loss %g", cfg.FrequencyMHz, *cfg.Links[0].PathLossDB)
	}
	a := cfg.Nodes[0].Ports[0].Radio.FM()
	if a.Channel != "wide" || a.Path != "voice" || a.DeviationHz != 5000 || a.LimitHz != 5000 ||
		a.AudioLowHz != 300 || a.EmphasisUs != 750 || a.FrequencyErrorHz != 150 {
		t.Errorf("radio a: %+v", a)
	}
	if math.Abs(a.TxPowerDBm-36.99) > 0.01 {
		t.Errorf("5 W is %g dBm, want 36.99", a.TxPowerDBm)
	}
	if a.Squelch.Open || a.Squelch.ThresholdDBm != -113 || a.Squelch.HysteresisDB != 8 {
		t.Errorf("city squelch: %+v", a.Squelch)
	}
	b := cfg.Nodes[1].Ports[0].Radio.FM()
	if b.Channel != "narrow" || b.Path != "data" || b.AudioHighHz != b.IFBandwidthHz/2 || b.LimitHz != 0 {
		t.Errorf("default radio b: %+v", b)
	}
	if b.Squelch.ThresholdDBm != -110 || b.Squelch.HysteresisDB != 6 || b.Squelch.OpenMS != 20 {
		t.Errorf("custom squelch: %+v", b.Squelch)
	}
}

func TestRadioBlockRejectsNonsense(t *testing.T) {
	for _, radio := range []string{
		"{ channel: huge }",
		"{ path: speaker }",
		"{ site_noise: city }",
		"{ squelch: loud }",
		"{ squelch: { preset: city, threshold_dbm: -110 } }",
		"{ deviation_hz: -1 }",
		"{ rx_level_dbfs: 3 }",
		"{ audio_low_hz: 3000, audio_high_hz: 300 }",
		"{ bogus: 1 }",
	} {
		yml := `
nodes:
  - id: a
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001, radio: ` + radio + ` }
links: []
`
		if _, err := Parse(strings.NewReader(yml)); err == nil {
			t.Errorf("radio %s: accepted, want an error", radio)
		}
	}
}

// Every config shipped in the repository must load: a demo that the
// strict parser refuses is a broken demo.
func TestShippedConfigsLoad(t *testing.T) {
	files, _ := filepath.Glob("../../configs/*.yaml")
	more, _ := filepath.Glob("../../examples/*.yaml")
	files = append(files, more...)
	if len(files) == 0 {
		t.Fatal("no shipped configs found")
	}
	for _, f := range files {
		if filepath.Base(f) == "docker-compose.yml" {
			continue
		}
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func parseRadio(t *testing.T, top, radio string) (*Config, error) {
	t.Helper()
	return Parse(strings.NewReader(top + `
nodes:
  - id: a
    ports:
      - { id: vhf, modem: { mode: afsk1200 }, kiss_port: 8001, radio: ` + radio + ` }
links: []
`))
}

func TestRadioExplicitZerosAreKept(t *testing.T) {
	cfg, err := parseRadio(t, "", "{ path: voice, emphasis_us: 0, limit_hz: 0, rx_level_dbfs: 0, noise_figure_db: 0 }")
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Nodes[0].Ports[0].Radio.FM()
	if r.EmphasisUs != 0 || r.LimitHz != 0 || r.RxLevelDBFS != 0 || r.NoiseFigureDB != 0 {
		t.Errorf("explicit zeros replaced by defaults: %+v", r)
	}
}

func TestSquelchThatCantCloseIsRefused(t *testing.T) {
	// city closes at -121 dBm; a residential site at 145 MHz has a floor
	// of about -120.8, so it would never close.
	if _, err := parseRadio(t, "", "{ squelch: city }"); err == nil || !strings.Contains(err.Error(), "never close") {
		t.Errorf("city squelch at a residential site: want a never-close refusal, got %v", err)
	}
	// A rural site is quiet enough.
	if _, err := parseRadio(t, "", "{ squelch: city, site_noise: rural }"); err != nil {
		t.Errorf("city squelch at a rural site: %v", err)
	}
	if _, err := parseRadio(t, "", "{ squelch: hard }"); err != nil {
		t.Errorf("hard squelch: %v", err)
	}
	// A preset's hysteresis can be overridden, which can rescue it.
	cfg, err := parseRadio(t, "", "{ squelch: { preset: city, hysteresis_db: 2 } }")
	if err != nil {
		t.Fatalf("city with 2 dB hysteresis: %v", err)
	}
	if h := cfg.Nodes[0].Ports[0].Radio.FM().Squelch.HysteresisDB; h != 2 {
		t.Errorf("hysteresis override: got %g, want 2", h)
	}
}

func TestRadioSettingsThatWouldBeIgnoredAreRefused(t *testing.T) {
	for _, radio := range []string{
		"{ squelch: { open_ms: 30 } }",
		"{ squelch: { preset: open, hysteresis_db: 3 } }",
		"{ path: voice, audio_low_hz: 3500 }",
		"{ frequency_error_hz: 90000 }",
		"{ tx_power_w: 0 }",
		"{ hum_noise_db: 0 }",
	} {
		if _, err := parseRadio(t, "", radio); err == nil {
			t.Errorf("radio %s: accepted, want an error", radio)
		}
	}
}

func TestRetiredKeysSetToNullAreRefused(t *testing.T) {
	if _, err := parseRadio(t, "capture_db: null\n", "{}"); err == nil {
		t.Error("capture_db: null accepted")
	}
}
