# net-sim — software AX.25 packet network simulator

A software-only simulator for testing AX.25/IL2P link-layer behaviour,
BPQ routing decisions, collision recovery and hidden-node interactions
without real radios. Multiple
[samoyed](https://github.com/doismellburning/samoyed) instances (or Dire Wolf,
or [pdn-soundmodem](https://github.com/packet-net/pdn-soundmodem) for modes
such as qpsk3600) act as TNCs; `sim-router` gives each one an FM radio and
joins them over a physical FM channel: real frequency modulation, noise at
each receiver, and a limiter and discriminator, so quieting, the threshold
cliff, open-squelch hiss, capture and collisions happen the way they do on
air.

The target is **2 m FM packet**. SSB is not modelled. The radios are built
from the Tait TM8100/TM8200 datasheets; see [docs/fm-channel.md](docs/fm-channel.md)
for the model, how it was checked, and where it falls short.

## Architecture

```
                    KISS  TCP                    KISS  TCP
 ┌────────────┐   <─────────>            <─────────>   ┌────────────┐
 │   your     │                                        │  another   │
 │ AX.25 app  │       ┌───────────────────────────┐    │ AX.25 app  │
 │ (BPQ /     │       │       sim-router          │    │ (kissattach│
 │  kissutil) │       │ • parses YAML topology    │    │  / nc /    │
 │            │       │ • spawns N samoyed kids   │    │  ax25d /   │
 │            │       │ • routes audio per link   │    │  ...)      │
 │            │       │ • an FM radio per port    │    │            │
 └────────────┘       └─────────────┬─────────────┘    └────────────┘
       ▲                            │                          ▲
       │            stdin (RX PCM)  │  UDP (TX PCM)            │
       │                            ▼                          │
       │              ┌─────────────────────────┐              │
       └─KISS  TCP────┤  samoyed-direwolf #N    ├──KISS  TCP───┘
                      │  (one per simulated     │
                      │   port; modem mode set  │
                      │   by per-port config)   │
                      └─────────────────────────┘
```

The TNCs do all the modem work. The router is the radios and the air
between them: each port's transmit audio is frequency-modulated onto a
carrier, and each receiving port hears the sum of the carriers reaching it,
at their received levels, plus its own noise, through its radio's receiver.

The audio path is entirely userspace (`stdin` for RX, UDP datagrams for TX
between samoyed and the router). No PulseAudio, PipeWire, JACK, or
`snd-aloop` is involved. See `NOTES-audio-io.md` for the gory detail.

## Quick run (Docker)

Pre-built images on ghcr:

```
docker pull ghcr.io/packet-net/net-sim:main
```

Bundled default network (two AFSK1200 nodes, KISS on `8001`/`8002`):

```
docker run --rm -p 8080:8080 -p 8001:8001 -p 8002:8002 \
  ghcr.io/packet-net/net-sim:main
```

Open <http://localhost:8080> for the web UI; KISS-attach your AX.25
application to `localhost:8001` / `localhost:8002`.

For a custom topology — point at any YAML on the host, and use
`--network=host` so KISS ports can land anywhere without you having to
predict them:

```
docker run --rm --network=host \
  -v $PWD/my-network.yaml:/etc/sim/network.yaml \
  ghcr.io/packet-net/net-sim:main
```

Tags published:
- `:main` and `:main-<sha>` on every push to main
- `:vX.Y.Z`, `:X.Y`, `:X`, `:latest` on tagged releases

Embedding in another project's tests (e.g. as a fixture for your AX.25
client / BPQ-style router): mount your network YAML, expose the KISS
ports your test connects to, and use the two probe endpoints —

- `GET /healthz` → `200 ok` once the HTTP server is accepting connections
  (use as a readiness probe).
- `GET /api/status` → JSON; check `running:true` to confirm the router
  brought all the samoyed children up.

The default Docker `CMD` is `-autostart`, so `running:true` is the
expected steady state right after startup. Override (`docker run ...
ghcr.io/.../net-sim:main` with extra args) if you'd rather drive
Start/Stop manually from your test harness via `POST /api/start`.

### Smoother audio under host load (`-rt-priority`)

The router paces audio with 10 ms tickers and every TNC child runs a
software demodulator; on a busy shared host, scheduler jitter glitches
both (lost ticks → choppy RX audio → decode failures that look like RF
problems). `sim-router -rt-priority` renices the router *and* each
spawned TNC child to `-10`. Deliberately plain niceness, not
`SCHED_FIFO` — a real-time policy could starve the host; niceness is
enough to keep the tickers honest. It's best-effort: without
`CAP_SYS_NICE` you get a one-line warning and the simulation carries on
at normal priority.

Granting the capability in Docker (`--cap-add SYS_NICE`), or in compose:

```yaml
services:
  net-sim:
    image: ghcr.io/packet-net/net-sim:main
    cap_add: [SYS_NICE]
```

The capability is granted at **runtime** (`--cap-add` / `cap_add`), deliberately — the image carries no file capabilities on its binaries. (An earlier build set `cap_sys_nice+ep` on `sim-web`/`sim-router`; that made them refuse to `exec` with *"operation not permitted"* on any host whose bounding set lacks the cap — e.g. an unprivileged LXC — breaking the image even for runs that never pass `-rt-priority`. Runtime cap-add is the correct mechanism.)

> **Nested-container hosts:** on a host that is itself an unprivileged container (e.g. Docker inside an unprivileged Proxmox LXC), the kernel checks CAP_SYS_NICE against the *init* user namespace, so negative nice is unavailable to anything inside — even container root, even with `cap_add`. The flag then logs its one-line warning and the sim runs at normal priority; everything else is unaffected.

## Quick install (curl | sudo bash)

On a fresh Debian 12 / Ubuntu 24.04+ host (LXC, VM, bare metal — anywhere
you have root and apt):

```
curl -fsSL https://raw.githubusercontent.com/packet-net/net-sim/main/install.sh | sudo bash
```

That script installs apt build-deps, clones and builds samoyed at
`/opt/samoyed`, clones and builds net-sim at `/opt/sim`, installs the
binaries to `/usr/local/bin/`, bootstraps a default two-node network at
`/etc/sim/network.yaml`, and (where systemd is present) registers and
starts a `sim-web.service` listening on `:8080`.

Then open <http://your-host:8080/>. The default page lets you edit the
YAML topology and Start / Stop / Apply-and-restart the simulator.

Override knobs (set as env vars before `sudo bash`):

| Var | Default | Notes |
|---|---|---|
| `SIM_DIR` | `/opt/sim` | net-sim checkout |
| `SAMOYED_DIR` | `/opt/samoyed` | samoyed checkout |
| `NETWORK_YAML` | `/etc/sim/network.yaml` | the active config |
| `WEB_PORT` | `8080` | sim-web listen port |
| `SYSTEMD` | `1` | set to `0` to skip the unit |
| `SIM_REF` | `main` | net-sim git ref to check out |
| `SAMOYED_REPO` / `SAMOYED_REF` | `M0LTE/samoyed` @ ACKMODE commit | samoyed source — temporarily pinned to the ACKMODE fork (see "Known limitations"); revert to `doismellburning/samoyed` `main` once ACKMODE lands upstream |

The script is idempotent — re-run it to update to a newer `main`. It
does **not** install pulseaudio / pipewire / jackd; samoyed initialises
PortAudio lazily so a daemon-less host works fine (see
`NOTES-audio-io.md`).

## Web UI

`sim-web` is a small integrated control surface. One page, one config
file, three buttons:

- **Start** — load the YAML and bring up the router with one samoyed
  child per port.
- **Apply & restart** — save the textarea contents to the YAML file
  (validated strictly) and recycle the router.
- **Stop** — tear it all down.

KISS TCP ports are listed live as the topology comes up so you know
where to point your AX.25 application.

`sim-web` embeds the router; it doesn't shell out to a separate
`sim-router` binary. Either one is a fine entrypoint:

- Engineer-loop / scripting: `sim-router -config configs/two-node.yaml`
- Day-to-day editing: the web UI.

## Manual install / build from source

If you'd rather not run an installer, the equivalent steps:

Prerequisites (Debian/Ubuntu):

- Go 1.22+ (`apt install golang`)
- `gcc`, `make`, `pkg-config`
- `libudev-dev libhamlib-dev portaudio19-dev libavahi-client-dev libbsd-dev libgps-dev libasound2-dev`
  (samoyed build-time deps — required even though we won't use any audio
  backend at runtime)
- samoyed checked out and built at `/opt/samoyed`:
  ```
  git clone https://github.com/doismellburning/samoyed /opt/samoyed
  make -C /opt/samoyed cmds
  ```
- Stock Dire Wolf (`apt install direwolf`) — reference only, not used at
  runtime.

Build:

```
make build       # builds sim-router and sim-web
```

Run the smallest demo:

```
make demo-two-node
```

Two AFSK1200 stations, fully linked, KISS exposed at `127.0.0.1:8001`
and `127.0.0.1:8002`. From another shell:

```
nc 127.0.0.1 8001 < some_kiss_frame.bin
nc 127.0.0.1 8002 | xxd                # see decoded frames
```

Or attach BPQ / kissutil / kissattach directly to those ports — the
router doesn't touch KISS frames, samoyed handles them natively.

## Demo topologies

| Target | What it shows |
|---|---|
| `make demo-two-node` | Single bidirectional AFSK1200 link. Sanity check. |
| `make demo-two-node-noisy` | The same pair at the edge of range: about half the frames get through. |
| `make demo-hidden-node` | A-B, B-C, no A-C; equal path loss. Simultaneous TX from A and C collides at B. |
| `make demo-hidden-node-capture` | Same topology, A loud / C quiet. A captures the demodulator at B; C is suppressed. |
| `make demo-mesh-3` | Three-node fully connected mesh. |
| `make demo-linear-6` | A — B — C — D — E — F chain, each hears only immediate neighbours. |
| `make demo-star-6` | Hub + 5 spokes. |
| `make demo-multiport-3` | Three nodes; the middle one has two independent radio ports. |

Each demo prints its KISS port assignments at startup.

## YAML config

```yaml
frequency_mhz: 145            # the band; sets how much man-made noise each site adds (default 145)
time_scale: 1.0               # run N x faster than wall clock (1 to 100; see below)

nodes:
  - id: a
    ports:
      - id: vhf               # unique within the node
        modem: { mode: afsk1200 }
        kiss_port: 8001       # the host-side TCP port for this port's KISS
      - id: uhf-link
        modem: { mode: gfsk9600 }
        kiss_port: 8002
        radio: { channel: wide }      # 9600 wants a 25 kHz channel
  - id: b
    ports:
      - id: vhf
        modem: { mode: afsk1200 }
        kiss_port: 8003
        radio: { squelch: hard, site_noise: rural }

links:
  # Directional. Both endpoints must use compatible modem configs.
  - { from: a.vhf, to: b.vhf, path_loss_db: 140 }
  - { from: b.vhf, to: a.vhf, path_loss_db: 140 }
```

`path_loss_db` (required) is the RF path loss between the two radios. With
the default radios (25 W, a residential site, a noise floor of about
-120.8 dBm on a 12.5 kHz channel):

| `path_loss_db` | What you get at 1200 baud |
|---|---|
| 120 to 150 | A strong, fully quieted link |
| 156 | The last dB or so of solid copy |
| 157 to 158 | The edge: some frames, then none |
| 159 and up | Below the FM threshold: nothing |

Every port's `radio` block is optional. Its keys, with the defaults (a Tait
TM8100/TM8200 from its datasheets):

| Key | Default | What it is |
|---|---|---|
| `channel` | `narrow` | `narrow` (12.5 kHz, 2.5 kHz rated deviation), `mid` (20 kHz, 4 kHz) or `wide` (25 kHz, 5 kHz) |
| `path` | `data` | `data`: the flat taps a data port gives, no emphasis, audio to half the IF bandwidth. `voice`: microphone and speaker, 300-3000 Hz, 750 us emphasis, a limiter at rated deviation |
| `deviation_hz` | rated | Deviation from full-scale transmit audio (a full-scale 1 kHz tone on the voice path). A TNC that transmits quieter deviates less |
| `limit_hz` | voice: rated | Transmit limiter ceiling; `0` (data path default) is none |
| `tx_power_w` | 25 | Transmitter power |
| `antenna_gain_dbi`, `feeder_loss_db` | 0 | Added to the link budget at both ends |
| `noise_figure_db` | 6.0 / 5.3 / 5.4 | Receiver noise figure, fitted to Tait's -121 dBm sensitivity |
| `site_noise` | `residential` | Man-made noise at the site (ITU-R P.372): `none`, `quiet_rural`, `rural`, `residential`, `business` |
| `frequency_error_hz` | 0 | This radio's offset from the channel; Tait's spec is 1.5 ppm (about 220 Hz on 2 m) |
| `rx_level_dbfs` | -18 | Receive audio level of a signal at rated deviation; the default puts open-squelch hiss at about -16 dBFS, as measured on a real radio |
| `hum_noise_db` | 42 / 45.5 / 47.5 | Receiver hum and noise floor, from Tait's measured figures; a large value such as 120 all but removes it |
| `squelch` | open | `open`, a Tait preset (`country` -115, `city` -113, `hard` -107 dBm), `{ preset, hysteresis_db, open_ms, close_ms }`, or `{ threshold_dbm, hysteresis_db, open_ms, close_ms }` |
| `if_bandwidth_hz`, `audio_low_hz`, `audio_high_hz`, `emphasis_us` | from `channel` and `path` | Override the receive filter and audio path; `emphasis_us: 0` is flat |

Other things the radios do: a transmitting radio's own receiver is muted
(half duplex); with the squelch open a receiver always hears hiss, louder
than any data signal, which drops away when a carrier arrives; and a
signal-strength squelch compares total received power (carriers plus
noise) with its threshold, as a real radio's RSSI does. A squelch whose
closing point (threshold minus hysteresis) sits under the receiver's noise
floor would open on the first signal and never close again, so it is
refused at start-up with the numbers. That rules out `country` and `city`
at the default residential site on 2 m; `hard` works anywhere, and the
other two work at a `rural` site.

Configs from before the FM channel model used `loss_db`, `noise_db`,
`default_noise_db`, `capture_db`, `collision_mode`, `mixer_mode` and
`squelch_open_ms`. They are refused with a message saying what replaced
each; there is no automatic conversion, because an audio attenuation has
no single RF path loss equivalent.

Strict parsing: any unknown key (e.g. `baud_rate` when you meant `baud`) is
an error at startup, not a silent default.

### time_scale — faster-than-real-time simulation

> **TNC pacing does not scale (measured):** samoyed paces its transmissions in
> wall-clock time (the real-airtime sleep before PTT release), so at
> `time_scale > 1` the TNC transmits in real time while the channel runs N×
> faster — TX throughput stays wall-clock-bound and ACKMODE echoes arrive N×
> "late" relative to a host whose protocol timers are scaled to match. In
> practice `time_scale` is currently only sound for receive-path
> experiments; ACKMODE pacing or throughput measurements need `time_scale: 1`
> until the TNC grows a matching speed factor (tracked upstream).
>
> Ports with `tnc: pdn` can't run scaled at all (pdn-soundmodem reads its
> audio in real time), so a config with any pdn port must keep `time_scale: 1`.

`time_scale: N` (or the `-time-scale N` flag on `sim-router`, which
overrides the config) runs the whole simulation N× faster than wall
clock: the router divides every pacing interval by N — the 10 ms
per-block RX ticker, the composite recorder's ticker, and the TX
watchdog's tick and silence window (silence detection has to scale with
the audio rate or `tx_end` events would fire mid-transmission). A 60 s
exchange completes in 60/N wall-clock seconds; recordings still come out
as normal 48 kHz files whose time axis is *sim* time.

**Fidelity caveat — read before trusting numbers from a scaled run.**
Only the router's clocks scale. The TNC child processes (samoyed /
direwolf) still run their own wall-clock behaviours — CSMA persist and
slottime waits, DCD hang times, any internal timeouts — which means at
`time_scale: 4` a TNC's 100 ms slottime is effectively 400 ms of sim
time. `time_scale > 1` is therefore an **accelerated-testing mode** (get
through a long soak/protocol exchange quickly), *not* a calibrated CSMA
/ channel-access simulation; for timing-sensitive contention studies run
at `1.0`. Hosts driving the KISS ports must also scale their own
protocol timers (T1/T2 etc.) by N, or their retries will fire N× too
early in sim time. Large factors are also bounded by CPU: every TNC
demodulator must keep up with N× real-time audio.

### TNC backend per port

Each port chooses which TNC implementation runs the modem:

```yaml
ports:
  - id: vhf
    tnc: samoyed        # default
    modem: { mode: afsk1200 }
    kiss_port: 8001
  - id: uhf
    tnc: direwolf       # stock direwolf 1.8 from apt
    modem: { mode: afsk1200 }
    kiss_port: 8002
  - id: fm
    tnc: pdn            # pdn-soundmodem, for its own modes
    modem: { mode: qpsk3600 }
    kiss_port: 8003
```

| `tnc` | Audio TX path | Notes |
|---|---|---|
| `samoyed` (default) | UDP datagrams | Clean and direct. |
| `direwolf` | ALSA `file` plugin -> named pipe | Workaround until upstream Dire Wolf gains UDP audio out. The router writes a per-port `.asoundrc` and a FIFO into `WorkDir`. |
| `pdn` | pdn-soundmodem's `pipe:` device (two FIFOs) | Adds pdn-soundmodem's modes. See below. |

samoyed and direwolf accept the same modem directives (`MODEM 9600`,
`IL2PTX 1`, etc.), and pdn-soundmodem's `afsk1200` and `fsk9600` are the
same signals, so a mixed-TNC config works fine: link compatibility is
decided by modem alone, not by which TNC is on either end.

#### pdn-soundmodem (`tnc: pdn`)

[pdn-soundmodem](https://github.com/packet-net/pdn-soundmodem) brings the
modes samoyed doesn't have, notably the FM ones: `qpsk3600` (7200 bps in one
FM voice channel), `fsk4800-il2p`, `fsk9600-il2p`, `c4fsk9600`,
`c4fsk19200` and the `ofdm-fm-*` family. With `tnc: pdn`, `mode` takes any
mode name `pdn-soundmodem --help` lists, and net-sim checks the name against
that list before starting anything. `afsk1200` and `gfsk9600` work too
(net-sim's `gfsk9600` is pdn's `fsk9600`), and talk to samoyed and direwolf
ports. `il2p` and `bpsk` are refused on pdn ports: use pdn's own names
(`afsk1200-il2p-nocrc`, `bpsk1200`, ...), which are only compatible with other
pdn ports.

Install it from the packet-net apt repository (the Docker image and
`install.sh` already include it):

```
curl -fsSL https://packet-net.github.io/apt/pubkey.asc | sudo gpg --dearmor -o /usr/share/keyrings/packet-net.gpg
echo "deb [signed-by=/usr/share/keyrings/packet-net.gpg] https://packet-net.github.io/apt ./" | sudo tee /etc/apt/sources.list.d/packet-net.list
sudo apt update && sudo apt install pdn-soundmodem
```

`sim-router` and `sim-web` find it on `$PATH`; `-pdn PATH` points at another
build. `make demo-pdn-fm-modes` runs qpsk3600 and c4fsk9600 pairs plus a
pdn-to-samoyed afsk1200 link.

Things to know:

- pdn-soundmodem reads its audio in real time, so `tnc: pdn` needs
  `time_scale: 1`; config validation refuses anything else. About 40 ms of
  audio is queued ahead of it to absorb scheduling jitter, which adds that
  much receive latency.
- pdn's receivers need a moment of audio after start-up before they decode
  reliably, so with any pdn port the router feeds audio for a second before
  it reports itself started.
- Each port runs `pdn-soundmodem --device pipe:... --kiss PORT --modem 0:MODE`
  with no config file, so pdn defaults apply: TXDELAY 300 ms until the host
  sets it over KISS, and carrier sense from pdn's in-band energy detector.
- Put each mode on the radio it belongs on: pdn's mode table gives the
  deviation (qpsk3600 and c4fsk19200 are 5 kHz modes, so `channel: wide`;
  c4fsk9600 is 2.5 kHz), and the OFDM-FM presets need audio up to their
  span (`ofdm-fm-8k` needs a wide data port).
- With the squelch open, which is how packet stations usually run, the
  receiver hears loud hiss between transmissions and a quieter signal
  during them. pdn behaves on that as its own documentation says it does
  on air without a control cable to the radio:
  - its audio-only carrier sense can hold a transmission for about ten
    seconds after hearing traffic;
  - its c4fsk receiver misses frames (pdn issue #518);
  - its qpsk receiver loses some frames too: it resets its frequency
    tracker when the level rises at the start of a burst, and on an
    open-squelch radio the level falls instead.

  A closed squelch (`radio: { squelch: hard }`) avoids all three, as it
  would on air. net-sim primes pdn's input with its radio's own hiss before
  pdn starts, so a station doesn't start on silence the way no real one
  does.

### Modem catalogue

| `mode` | Required params | Status (samoyed / direwolf) |
|---|---|---|
| `afsk1200` | none | ✅ supported. Default workhorse. |
| `gfsk9600` | none | ✅ supported. Auto-selected for 9600 baud + G3RUH. |
| `bpsk` | `baud`, optional `carrier_hz` | ❌ not yet — refused at startup. |
| `il2p` | `inner` (afsk1200 / gfsk9600), `fec` (`strong`/`weak`) | ✅ supported (FEC strength only — see Known limitations). |

Modes the YAML accepts but samoyed can't actually run *fail at startup
with a clear error*. Adding a new mode is config plumbing: see the
translation table in `internal/tnc/tnc.go` and the source-of-truth
table in `NOTES-audio-io.md`. Ports with `tnc: pdn` take pdn-soundmodem's
own mode names instead (see above).

## Recording runs

Both `sim-router` and `sim-web` can write per-port WAV recordings of
every transmission and every receive-side mix. Files are mono 16-bit LE
PCM at 48 kHz, the simulator's native format, so recording is a
straight tee with no resampling.

**`sim-router`** — pass `-record DIR`. Recording starts as soon as the
router comes up. Each run gets its own timestamped subdirectory:

```
sim-router -config configs/hidden-node.yaml -record /tmp/sim-rec
# /tmp/sim-rec/20260506T120000Z/a.vhf.tx.wav
# /tmp/sim-rec/20260506T120000Z/a.vhf.rx.wav
# /tmp/sim-rec/20260506T120000Z/b.vhf.tx.wav   ...
```

For each port `<node>.<port>` you get two files:

- `*.tx.wav` — exactly what the TNC keyed onto the air.
- `*.rx.wav` - what the TNC heard: its radio's receive audio, hiss,
  signals, collisions and all (silence while its own radio transmits, or
  while a closed squelch is shut).

All `.rx.wav` files for a single run share a clock — they're sample-aligned,
so loading them into Audacity as separate tracks shows you exactly which
station a receiver was hearing at any moment.

**`sim-web`** — pass `-record DIR` to enable the feature; a Record
checkbox appears next to Start/Stop. Toggle it any time:

- **Off → on while running** — opens a fresh session immediately.
- **On → off while running** — closes the current session (WAV headers
  are patched on close so the files are valid).
- **Toggled while stopped** — recording is "armed"; it will start when
  the next Start/Apply &amp; restart brings the router up.

The toggle survives Apply &amp; restart, so you can edit the topology and
keep recording across the restart with one click.

**Disk usage**: about 96 KB/s per stream. A 6-node mesh with two streams per
port is about 1 MB/s, 3.5 GB/hour. WAV size is capped by a 32-bit
chunk-size field: a mono file fills at about 12.4 hours and a stereo
composite at about 6.2. A full file stops growing (with a warning) and
stays valid; there is no rotation.

Recording never holds up the simulation: each file has its own writer and
a 10 s buffer. If the disk can't keep up, that file stops (with a warning)
rather than stalling the audio or growing gaps that would break its
alignment with the others.

**Crash safety**: WAV headers are patched on `Close`. If the process is
killed without a clean shutdown, the file is still readable as raw PCM
but its data-chunk size will say zero — most players will refuse to
play it. Stop the router cleanly (or click Record off) before pulling
the plug.

### Composite recording — true on-air timeline

The per-port `*.tx.wav` files above are written straight from each TNC
as it *bursts* its TX audio (samoyed can emit a 500 ms frame onto its
UDP socket in a few wall-clock ms), so they faithfully carry the
modulated waveform but are **not** a real-time timeline — you can't line
two of them up and trust the gaps.

A **composite recording** fixes that. It captures a chosen set of
transmitters into a *single*, sample-aligned, multi-channel WAV — one
transmitter per channel — and paces every channel through one real-time
clock (the same mechanism that feeds audio into the receivers). For the
canonical two-station setup that's a **stereo** file with one station's
transmitted audio in each ear. The result is an accurate timeline of
what was on the air: overlapping transmissions overlap in time,
inter-burst gaps are real silence, and both channels share one start
time. Drop it into Audacity and you can see at a glance who was keying
when — ideal for inspecting hidden-node collisions (put the two hidden
stations in left/right).

The audio is each TNC's transmit audio, before its radio and the channel:
what each station put on the air, not what any particular receiver heard.

**`sim-router`** — pass `-composite a.vhf,b.vhf` together with `-record DIR`
(the base dir for the output). Recording starts as soon as the router
comes up and stops on a clean shutdown:

```
sim-router -config configs/hidden-node.yaml -record /tmp/sim-rec \
  -composite a.vhf,c.vhf
# /tmp/sim-rec/composite-20260604T120000.000Z.wav  (stereo: a.vhf left, c.vhf right)
```

List more than two ports for a multi-track WAV (one channel each, in the
order given).

**`sim-web`** — with `-record DIR` set, a **Composite recording** panel
appears on the control page: pick the Left-ear and Right-ear
transmitters, click **Start recording**, then **Stop recording** when
done, and **Download .wav**.

#### HTTP API (for external software)

Drive it from a test harness or any HTTP client. All endpoints live
under `/api/record/composite/` and need `sim-web` started with
`-record DIR` and the router running.

| Method & path | Body | Effect |
|---|---|---|
| `POST /api/record/composite/start` | `{"ports":["a.vhf","c.vhf"]}` (optional) | Start a recording. Each port is one channel, in order (index 0 = left). Omit the body to default to the topology's first two ports. |
| `POST /api/record/composite/stop` | — | Finalise the file (patches the WAV header) and return its status. |
| `GET /api/record/composite/status` | — | Current state: `available`, `active`, `channels`, `path`, `duration_s`, and `download_url` once a file is complete. |
| `GET /api/record/composite/download` | — | Stream the most recently completed composite WAV as an attachment. |

A typical external run: `POST .../start` → exercise your stations over
KISS → `POST .../stop` → `GET .../download`. The `composite` block is
also included in `GET /api/status`.

## The FM channel

Each port is an FM radio. Its transmitter frequency-modulates the TNC's
audio; each receiver adds its own noise to the carriers that reach it,
filters to its IF bandwidth and demodulates with a limiter and
discriminator. Nothing decides who captures, or what a collision sounds
like: that is what the receiver does with the signals in front of it.

The model is a Go port of
[M0LTE.FmChannel](https://github.com/M0LTE/M0LTE.FmChannel), the FM link
pdn-soundmodem measures its modes through, made to run continuously with
several carriers per receiver. Tests hold it to that package's output. The
radios come from Tait's TM8100/TM8200 datasheets, and against measurement
it reproduces a real radio's open-squelch levels (with the hiss level set
to radio1's, a data signal and an unmodulated carrier land within 1 and 6
dB of what radio1 measured), Tait's sensitivity and its co-channel
rejection. It reaches 20 dB SINAD 3 to 6 dB
sooner than a real Tait does. The details, and what isn't modelled, are in
[docs/fm-channel.md](docs/fm-channel.md).

## Known limitations (samoyed-side, expected to be fixed upstream)

This is a gap in the current samoyed build that affects what you can
test against; it will likely land in samoyed soon and we'll bump the
pin then. Tracker issues:
[net-sim#1](https://github.com/packet-net/net-sim/issues/1) /
[net-sim#2](https://github.com/packet-net/net-sim/issues/2).

- **No IL2P+CRC (a.k.a. IL2Pc) support.** Samoyed implements the IL2P
  v0.6 base form — header + payload, Reed-Solomon FEC, no trailing 2-byte
  CRC. The `fec` field on `il2p` modems toggles RS strength (`strong` →
  `IL2PTX 1`, `weak` → `IL2PTX 0`); it does *not* enable the spec's
  optional trailing-CRC variant. If your application requires IL2Pc on
  the wire, this rig won't reproduce it yet. (The field used to be
  called `crc`, which was misleading — renamed to `fec` to match what
  it actually does.)
**KISS ACKMODE — now supported.** Previously samoyed's KISS layer refused
the XKISS ACKMODE opcode and dropped the frame; the pinned samoyed build now
implements G8BPQ extended-KISS ACKMODE (command nibble `0x0C`). Send a data
frame with two leading id bytes (`C0 xC aa bb <frame> C0`) and the TNC echoes
those two bytes back (`C0 xC aa bb C0`) once the frame has actually been
transmitted — so a host (some BPQ configurations, certain `ax25d` setups) can
start FRACK from the real on-air moment instead of from hand-off. The id bytes
are echoed verbatim. (The implementation currently lives on a samoyed fork,
[M0LTE/samoyed](https://github.com/M0LTE/samoyed/tree/feat/ackmode); net-sim
pins that build and will switch back to upstream samoyed once ACKMODE lands
there. XKISS poll mode and checksum mode remain unimplemented.) An end-to-end
round-trip test lives in `internal/tnc/ackmode_test.go`.

## What's not in v1

- BPQ / XRouter integration (works fine — point them at the KISS ports —
  but no demos shipped).
- Hot reload of topology (restart the router).
- Playback / injection of recorded WAVs back into the router (recording
  to WAV is supported — see "Recording runs" below).
- BER / FER reporting beyond the basic frame counters demonstrable from
  KISS sniffing.
- SSB, multipath, Doppler and fading, transmitter rise time, receiver AGC.
- Modem modes beyond what samoyed and pdn-soundmodem support.

## Layout

```
cmd/sim-router/            - CLI entrypoint
cmd/sim-web/               - integrated web UI; embeds the router
internal/config/           - YAML parsing + validation (strict)
internal/tnc/              - per-port samoyed / direwolf / pdn-soundmodem process management
                             + the modem-mode → config-directive translation
internal/audio/            - PCM types, WAV recorder, audio tap
internal/fm/               - the FM channel: modulator, receiver, radios, link budget
internal/router/           - topology, audio routing, the radios per port
tools/fmref/               - makes internal/fm's reference vectors from M0LTE.FmChannel
configs/                   - demo topology YAMLs
install.sh                 - curl | sudo bash installer
NOTES-audio-io.md          - Phase 1 findings (essential reading if you
                             want to understand why we use stdin + UDP)
docs/fm-channel.md         - the FM channel model, its calibration and limits
```
