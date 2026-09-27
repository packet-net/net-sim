package config

import (
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
mixer_mode: fm_capture
capture_db: 6.0
collision_mode: silence
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
    loss_db: 0
  - from: bsg.vhf
    to:   rdg.vhf
    loss_db: 0
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.CaptureDB != 6.0 {
		t.Errorf("capture_db = %g, want 6.0", cfg.CaptureDB)
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

func TestSquelchOpenMSAccepted(t *testing.T) {
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
        kiss_port: 8002
links:
  - { from: a.vhf, to: b.vhf, loss_db: 0, squelch_open_ms: 50 }
`
	cfg, err := Parse(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	if cfg.Links[0].SquelchOpenMS != 50 {
		t.Errorf("squelch_open_ms = %g, want 50", cfg.Links[0].SquelchOpenMS)
	}
}

func TestSquelchOpenMSRange(t *testing.T) {
	for _, v := range []string{"-1", "501"} {
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
        kiss_port: 8002
links:
  - { from: a.vhf, to: b.vhf, loss_db: 0, squelch_open_ms: ` + v + ` }
`
		_, err := Parse(strings.NewReader(yaml))
		if err == nil || !strings.Contains(err.Error(), "squelch_open_ms") {
			t.Fatalf("squelch_open_ms=%s: expected range error, got %v", v, err)
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
  - { from: a.fm, to: b.fm, loss_db: 0 }
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
  - { from: a.fm, to: b.fm, loss_db: 0 }
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
