package tnc

import (
	"context"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/packethacking/net-sim/internal/config"
)

func TestModemConfLines(t *testing.T) {
	cases := []struct {
		name string
		mode config.Modem
		want []string
	}{
		{
			name: "afsk1200",
			mode: config.Modem{Mode: config.ModeAFSK1200},
			want: []string{"MODEM 1200"},
		},
		{
			name: "gfsk9600",
			mode: config.Modem{Mode: config.ModeGFSK9600},
			want: []string{"MODEM 9600"},
		},
		{
			name: "il2p afsk1200 strong",
			mode: config.Modem{Mode: config.ModeIL2P, Inner: config.ModeAFSK1200, FEC: config.FECStrong},
			want: []string{"MODEM 1200", "IL2PTX 1"},
		},
		{
			name: "il2p afsk1200 weak",
			mode: config.Modem{Mode: config.ModeIL2P, Inner: config.ModeAFSK1200, FEC: config.FECWeak},
			want: []string{"MODEM 1200", "IL2PTX 0"},
		},
		{
			name: "il2p gfsk9600 strong",
			mode: config.Modem{Mode: config.ModeIL2P, Inner: config.ModeGFSK9600, FEC: config.FECStrong},
			want: []string{"MODEM 9600", "IL2PTX 1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := modemConfLines(c.mode)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("modemConfLines = %q, want %q", got, c.want)
			}
		})
	}
}

func TestSamoyedConfContainsModem(t *testing.T) {
	cases := []struct {
		name        string
		spec        Spec
		mustContain []string
	}{
		{
			name: "afsk1200",
			spec: Spec{
				NodeID:         "a",
				PortID:         "vhf",
				Modem:          config.Modem{Mode: config.ModeAFSK1200},
				KissPort:       8001,
				RxAudioUDPPort: 17000,
			},
			mustContain: []string{"MODEM 1200", "KISSPORT 8001", "ADEVICE - udp:127.0.0.1:17000"},
		},
		{
			name: "gfsk9600",
			spec: Spec{
				NodeID:   "b",
				PortID:   "uhf",
				Modem:    config.Modem{Mode: config.ModeGFSK9600},
				KissPort: 8002,
			},
			mustContain: []string{"MODEM 9600", "KISSPORT 8002"},
		},
		{
			name: "il2p strong gfsk9600",
			spec: Spec{
				NodeID: "c", PortID: "vhf",
				Modem: config.Modem{
					Mode:  config.ModeIL2P,
					Inner: config.ModeGFSK9600,
					FEC:   config.FECStrong,
				},
				KissPort: 8003,
			},
			mustContain: []string{"MODEM 9600", "IL2PTX 1"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := samoyedConf(c.spec)
			for _, s := range c.mustContain {
				if !contains(got, s) {
					t.Errorf("conf missing %q. Full output:\n%s", s, got)
				}
			}
		})
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestStartRefusesKissPortInUse(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	_, err = Start(context.Background(), Spec{NodeID: "a", PortID: "p", Modem: config.Modem{Mode: config.ModeAFSK1200}, KissPort: port, SamoyedBin: "/nonexistent"})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("want an in-use error, got %v", err)
	}
}
