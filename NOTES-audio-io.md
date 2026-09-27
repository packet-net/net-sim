# Phase 1 — Audio I/O findings

Goal: feed PCM into samoyed and read TX audio out without going through any
kernel sound stack. Source for everything below: the samoyed tree at
`/opt/samoyed` (commit `1ba1c88`, built 2026-05-06).

## Verdict

samoyed exposes two non-kernel audio paths that, combined, give us a complete
loop:

| Direction | Mechanism | samoyed config |
|---|---|---|
| Audio in (RX from peer) | stdin | `ADEVICE - <out>` or `ADEVICE stdin <out>` |
| Audio in (RX from peer) | UDP listen | `ADEVICE udp:<port> <out>` |
| Audio out (TX to peer) | UDP datagrams | `ADEVICE <in> udp:<host>:<port>` |

**Stock Dire Wolf 1.8.1 has stdin input but *no* UDP output yet** — it
falls through to ALSA when handed `udp:host:port`. For ports with
`tnc: direwolf`, the router instead writes a per-port `.asoundrc` that
defines an ALSA `file` plugin pointing at a FIFO, and points
`ADEVICE - <name>` at it. Direwolf writes raw PCM to the FIFO; the
router reads the FIFO. Loops audio in userspace just fine, but it's a
workaround we'll happily delete once Dire Wolf supports `udp:` output.

```
pcm.dwout {
  type file
  slave.pcm null
  file "<workdir>/dw-<node>-<port>/tx.fifo"
  format raw
}
```

These are inherited from Dire Wolf. The router is going to use **stdin in /
UDP out** for every samoyed instance. Picked stdin over UDP for input because
backpressure is well-defined (kernel pipe buffer flow controls the router) and
because the router can stop writing to drop a port out of the bus cleanly.

Audio format: **mono / 48000 Hz / signed 16-bit LE**. This started at
samoyed's default of 44.1 kHz; it moved to 48 kHz in 2026-09 (see
docs/fm-channel-plan.md) because pdn-soundmodem, real stations' USB sound
cards and the FM channel model's calibration data are all 48 kHz. samoyed and
direwolf are set to it with `ARATE 48000` in their per-port configs.

Stock Dire Wolf 1.8.1 (`apt install direwolf`) was kept as a reference —
useful for A/B sanity checks — but is **not** used at runtime. It only links
against ALSA (no UDP audio output), so it can't be the audio sink in this
architecture.

## PortAudio (resolved upstream)

Earlier samoyed builds called `Pa_Initialize()` unconditionally at startup
and bailed out (`Pointless to continue without audio device`) on hosts
with no PortAudio backend, even when `ADEVICE` selected `udp:` + `stdin`
and no PortAudio stream would ever be opened. We worked around it with
a tiny `LD_PRELOAD` shim that no-op'd `Pa_Initialize` / `Pa_Terminate`.

That has been fixed upstream (samoyed#501, merged in samoyed `cba41c1`):
samoyed now only initialises PortAudio when an `ADEVICE` actually needs
it. The shim and all its plumbing have been removed; net-sim spawns
`samoyed-direwolf` with no `LD_PRELOAD`. Smoke-tested on a host with no
PulseAudio / ALSA card / JACK — samoyed reports the channel and serves
KISS without complaint.

## Working pipeline

### One-shot decode test (Phase 1 acceptance)

```bash
# Generate 10 known frames at 1200 baud as WAV
samoyed-gen_packets -N 10 -o /tmp/g1200.wav

cat > /tmp/d.conf <<EOF
ADEVICE - udp:127.0.0.1:9999
ACHANNELS 1
CHANNEL 0
MYCALL N0CALL
MODEM 1200
KISSPORT 0
KISSPORT 8001
AGWPORT 0
DNSSD 0
EOF

# Strip 44-byte WAV header, feed raw PCM via stdin
( dd if=/tmp/g1200.wav bs=1 skip=44 status=none ) | \
  samoyed-direwolf -c /tmp/d.conf -t 0 -q d
```

Result: all 10 frames decoded (`WB2OSZ-15>TEST:...0001 of 0010` through
`...0010 of 0010`).

The WA8LMF *Track 1* TNC test recording is not bundled in the Debian
direwolf package on this LXC. `samoyed-gen_packets` produces deterministic
known-good signals at any supported baud, so we use that as the canonical
fixture instead.

### End-to-end loopback (TX of one samoyed → RX of another)

The full simulator topology in miniature: samoyed-A modulates a KISS frame to
UDP audio, a userspace bridge forwards the audio to samoyed-B's UDP input,
samoyed-B demodulates and emits a KISS frame on its TCP port.

```bash
# A: KISS in on 8201, audio out on UDP 7100
cat > /tmp/dA.conf <<EOF
ADEVICE - udp:127.0.0.1:7100
ACHANNELS 1
CHANNEL 0
MYCALL N0A
MODEM 1200
KISSPORT 0
KISSPORT 8201
AGWPORT 0
DNSSD 0
EOF

# B: audio in via UDP 7200, dummy audio out (we don't care), KISS on 8202
cat > /tmp/dB.conf <<EOF
ADEVICE udp:7200 udp:127.0.0.1:9999
ACHANNELS 1
CHANNEL 0
MYCALL N0B
MODEM 1200
KISSPORT 0
KISSPORT 8202
AGWPORT 0
DNSSD 0
EOF

# Bridge A's TX UDP → B's RX UDP
socat -u UDP-RECV:7100,reuseaddr UDP-DATAGRAM:127.0.0.1:7200 &

# Start B (RX side first so its UDP listener is up)
samoyed-direwolf -c /tmp/dB.conf -t 0 -q d &

# Start A (eat /dev/zero on stdin → silence baseline)
cat /dev/zero | samoyed-direwolf -c /tmp/dA.conf -t 0 -q d &

# Send a KISS frame to A → expect it to come out of B
nc 127.0.0.1 8201 < kiss_frame.bin
nc 127.0.0.1 8202    # see decoded KISS frame
```

Verified: `N0A-1>APDW01:HELLOFROMA` decoded by samoyed-B with the original
payload intact, KISS framing on B's TCP port correct.

## Per-port samoyed config (canonical)

The router will template this per port:

```
ADEVICE - udp:127.0.0.1:<rx_audio_udp_port>
ACHANNELS 1
CHANNEL 0
MYCALL N0<derived from node id>   # samoyed wants something here, never used by the simulator
KISSPORT 0                  # remove the default 8001
KISSPORT <kiss_tcp_port>    # the one we expose
AGWPORT 0                   # we don't use AGW
DNSSD 0                     # don't spam mDNS
```

Plus per-modem CLI flags (see modem-mode table below).

`KISSPORT 0` is essential: without it samoyed *also* opens port 8001 by
default, and a second instance fails to bind. Same applies to `AGWPORT`
which defaults to 8000.

## Modem mode enumeration

Tested against this samoyed binary:

| Catalogue `mode` | Status | Invocation | Notes |
|---|---|---|---|
| `afsk1200` | ✅ works | config: `MODEM 1200` | Bell 202, classic 2m FM AFSK. Phase 1 acceptance verified. |
| `gfsk9600` | ✅ works | config: `MODEM 9600` | K9NG/G3RUH 9600 baud GMSK. `MODEM 9600` already implies G3RUH at that baud. (Earlier samoyed builds silently ignored this directive — the bug was tracked at samoyed#502 and is fixed in upstream main.) |
| `bpsk` | ❌ not supported | — | samoyed has 2400 QPSK (`-B 2400`) and 4800 8PSK (`-B 4800`); HF 300/1200 BPSK is not in the modem catalogue. Router must reject this mode at startup. |
| `il2p` | ⚠ partial | config: `IL2PTX 1` for strong FEC, `IL2PTX 0` for weak FEC | IL2P transmit is wrapped around the underlying modem via the `IL2PTX` directive. RX-side detection is automatic — nothing needed. The `inner` config key maps to the underlying `MODEM` line; `fec` maps to `IL2PTX 1` vs `IL2PTX 0`. |

### Other CLI flags that matter

- `-c <conf>` — config file (we always template one per port)
- `-t 0` — disable text colour (cleaner logs)
- `-q d` — suppress APRS-decode chatter (we don't care about APRS semantics
  in the simulator; the router only needs the audio + KISS plumbing)
- `-q h` — suppress per-frame "audio level" lines (drop in production; keep
  during dev to spot quiet/clipped TX)
- `--error-rate Rn` — receive-side frame clobber rate. Useful as a *future*
  alternative noise model; v1 uses analog noise injection per the plan.
- `--bit-error-rate n` — receiver BER. Ditto.

### `MODEM` directive (resolved upstream)

Earlier samoyed builds silently ignored `MODEM 9600` (and `MODEM 9600 g3ruh`)
in the config file when the channel was first seen — the channel reported
"1200 baud, AFSK" regardless. The router worked around this by passing
`-B 9600 -g` on the CLI. samoyed#502 traced it to a `-B` override always
firing, even when the flag wasn't passed; fixed in samoyed `b310445` (PR
#505). The router now emits `MODEM 9600` in the per-port config and passes
no modem CLI flags.

## Translation table (mode → samoyed config directives)

This is the source of truth for `internal/tnc/modemConfLines`:

```
afsk1200:   "MODEM 1200"
gfsk9600:   "MODEM 9600"
il2p:
  inner=afsk1200, fec=strong:  "MODEM 1200" + "IL2PTX 1"
  inner=afsk1200, fec=weak:    "MODEM 1200" + "IL2PTX 0"
  inner=gfsk9600, fec=strong:  "MODEM 9600" + "IL2PTX 1"
  inner=gfsk9600, fec=weak:    "MODEM 9600" + "IL2PTX 0"
bpsk:       refuse — not supported by current samoyed
```

samoyed's `IL2PTX 0/1` controls Reed-Solomon parity-byte count, *not* the
optional 2-byte trailing CRC defined by the IL2P spec — that variant
("IL2P+CRC" / "IL2Pc") is not implemented in samoyed at all (verified
in `src/il2p_send.go` and `src/il2p_codec.go`: the encode path only
takes `max_fec`). `fec` was named `crc` originally; renamed because the
old name implied a feature samoyed doesn't have.

## Upstream issues (closed)

Both of the original blocking samoyed bugs that net-sim worked around
have been fixed upstream and the workarounds removed here:

- samoyed#501 — `Pa_Initialize()` was fatal on daemon-less hosts even
  when `ADEVICE` was `udp:` / `stdin`. Fixed in samoyed `cba41c1`
  (PR #507): PortAudio is now initialised lazily.
- samoyed#502 — `MODEM <baud>` directive in the config file silently
  fell back to defaults when the channel was first seen. Fixed in
  samoyed `b310445` (PR #505): the `-B` CLI override no longer fires
  when the flag wasn't passed.

Hard rule: we don't patch samoyed from net-sim. Anything we hit goes
into samoyed's tracker as an issue or PR.
