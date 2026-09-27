package tnc

import (
	"reflect"
	"testing"

	"github.com/packethacking/net-sim/internal/config"
)

func TestParsePdnModes(t *testing.T) {
	help := []byte(`Usage: ...
  --help                  Print this and exit.

Modes for --modem N:MODE:
  afsk1200, afsk1200-fx25, qpsk3600,
  fsk9600, ofdm-fm-8k
  plus ardop, the ARDOP virtual TNC: host port 8515 unless the config file's
  modem entry sets "port".

Documentation: https://github.com/packet-net/pdn-soundmodem
`)
	want := []string{"afsk1200", "afsk1200-fx25", "qpsk3600", "fsk9600", "ofdm-fm-8k"}
	if got := parsePdnModes(help); !reflect.DeepEqual(got, want) {
		t.Errorf("parsePdnModes = %q, want %q", got, want)
	}
}

func TestPdnArgs(t *testing.T) {
	s := Spec{KissPort: 8001, Modem: config.Modem{Mode: config.ModeGFSK9600}}
	got := pdnArgs(s, "/w/rx.fifo", "/w/tx.fifo")
	want := []string{
		"--device", "pipe:/w/rx.fifo,/w/tx.fifo,48000",
		"--kiss", "8001",
		"--bind", "127.0.0.1",
		"--modem", "0:fsk9600", // net-sim's gfsk9600 is pdn's fsk9600
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("pdnArgs = %q, want %q", got, want)
	}
}
