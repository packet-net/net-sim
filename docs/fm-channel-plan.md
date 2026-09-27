# Plan: a physical FM channel model, and a 48 kHz router

Status: proposed, 2026-09-27. Nothing here is built yet.

## Summary

net-sim's receiver model is an approximation that gets the capture effect roughly right and most of an open-squelch FM receiver backwards. This plan replaces it with real frequency modulation: each transmitter's audio is frequency-modulated onto a carrier, each receiver gets every carrier at its received RF level plus its own noise floor, and a limiter, discriminator and the radio's receive path turn that back into audio. Quieting, the threshold cliff, clicks, open-squelch hiss, capture and collision beat notes then come out of the physics instead of being asserted.

The model already exists, in C#, as [M0LTE.FmChannel](https://github.com/M0LTE/M0LTE.FmChannel), which pdn-soundmodem measures its FM modes through. It handles one burst on one carrier at a stated carrier-to-noise ratio. net-sim needs it continuous, with several carriers per receiver and an idle channel, so the work is to take that model's stages and run them as a stream.

Before that, the router moves from 44.1 kHz to 48 kHz. It is a small change on its own and makes everything after it simpler.

## What is wrong with the current model

Measured on radio1's discriminator (pdn `docs/dev/carrier-sense.md` and `OpenSquelchFmReceiver`), against net-sim with `default_noise_db: 45` and a 10 dB link:

| | radio1 | net-sim today |
|---|---|---|
| Idle, squelch open | -15.9 dBFS hiss | -45 dBFS hiss |
| Far end sending data | -25 dBFS, below idle | about -13 dBFS, far above idle |
| Far end keyed, unmodulated | -55 dBFS | -45 dBFS, full hiss |

1. Path loss scales the audio. On FM the audio level is set by deviation; a weaker carrier changes the noise, not the loudness, until it nears threshold.
2. Idle is quieter than traffic. On an open-squelch radio the hiss is louder than the data, so arriving traffic is a fall in level. Anything energy-based (DCD, carrier sense) sees the opposite in net-sim.
3. Quieting follows the audio peak of each 10 ms block, not the presence of a carrier, so an unmodulated carrier (pdn's TXDELAY) is heard as idle hiss instead of near silence, and quieting bottoms out at a fixed -60 dBFS.
4. Degradation is smooth Gaussian noise on a ratio of audio peaks. Real FM holds up to a threshold and then collapses into clicks, with the wanted audio shrinking as well. There is no RF carrier-to-noise ratio anywhere.
5. The noise is flat to 22 kHz. Real discriminator noise rises with frequency up to half the IF bandwidth and is then shaped by the radio's audio path; radio1's idle hiss falls 37 dB across the band and quiets unevenly (about 1 dB at the bottom, 28 dB at 9 to 10 kHz).
6. Capture is a hard switch at `capture_db`, with collisions rendered by a separate rule.

The pdn findings I reported with noise switched on (a station held off for over 150 s, c4fsk misses) were taken through this model and should not be treated as evidence about pdn until they are repeated through the new one.

## Step 1: run the router at 48 kHz

Recommended, and as its own release before the FM work.

Why:

- pdn-soundmodem runs at 48 kHz (all its DSP rates divide it), so the pdn backend's resampler and its extra filter stage go away entirely.
- The calibration data is at 48 kHz: radio1's captures and the `OpenSquelchFmReceiver` spectrum run to 24 kHz. At 44.1 kHz the top 2 kHz of that table can't be represented.
- M0LTE.FmChannel picks its IF rate as the audio rate times a power of two, and pdn's FM ladders run at 12 and 48 kHz, so 48 kHz keeps net-sim's numbers directly comparable with pdn's.
- The USB codecs on real packet stations (CM108 and friends) run natively at 48 kHz.
- A 10 ms block becomes 480 samples, still a whole number.

Cost: about 9 % more audio processing everywhere, and a Dire Wolf or samoyed demodulator doing 9 % more work. The IF processing in step 2 runs oversampled anyway (96 to 192 kHz), so the router rate doesn't limit the FM model either way.

Changes:

- `audio.SampleRate` and `BlockSamples` to 48000 and 480; everything else derives from them (queues, pacing, WAV headers, composite, tap).
- `ARATE 48000` in the samoyed and Dire Wolf configs (both support it on stdin and UDP; they currently rely on a 44.1 kHz default).
- pdn backend: drop the resampler, keep the FIFO cushion and burst padding.
- Browser audio player in `map.html` (hard-coded 44100), comments, README.
- Checks: the full test suite, the pdn and Dire Wolf end-to-end tests, and samoyed's ACKMODE round trip in the Docker image (samoyed isn't installed on the dev box).

Recordings made before and after differ in rate; the release notes say so.

## Step 2: the FM model

### Where it sits

Today each receiving port's feeder pops one audio block per link every 10 ms, mixes them with the capture rule and adds noise. With the FM model the feeder does, per 10 ms block:

1. For each incoming link whose transmitter's carrier is on: take the popped TX audio block, run it through that transmitter's audio path (band limit, pre-emphasis, optional limiter), frequency-modulate it at the IF rate with a phase that continues across blocks, scale it to the received carrier amplitude and rotate it by the transmitter's frequency error.
2. Sum the carriers, add complex noise at the receiver's noise floor, IF-filter, limiter-discriminator, decimate, de-emphasise, band-limit to the receive path, scale to the port's receive level.
3. Hand the result to the TNC as today.

With no carrier present, step 2 runs on noise alone, and that is the open-squelch hiss. Nothing else produces it.

Carrier presence comes from the link queue: a carrier is on while its transmission has audio queued or playing, including silent blocks such as pdn's TXDELAY. The existing transmission-boundary rule (a 200 ms quiet gap on the source) decides where one keyup ends.

The transmitter's audio path is per transmitting port, and the modulation is per link (each link has its own queue timing and frequency error). All filters are streaming, with state carried across blocks.

### Drive: fixed gain, not per-burst peak scaling

FmChannel's default scales each burst so its own peak hits the stated deviation, which needs the whole burst up front. A live link can't do that, and a real transmitter doesn't either: the TNC's output level sets the deviation. net-sim uses fixed gain (full-scale TX audio = `deviation_hz`) with an optional hard limiter, which is FmChannel's `LimitAtDeviationHz` mode. So a TNC driving too hot over-deviates or clips, as it would on air.

### Configuration

Opt-in at first, so every existing config behaves exactly as today:

```yaml
channel_model: fm             # fm | simple (today's model, the default for now)
nodes:
  - id: a
    ports:
      - id: fm
        tnc: pdn
        modem: { mode: qpsk3600 }
        kiss_port: 8001
        radio:
          path: data          # data (flat, discriminator tap) | mic (emphasis, 300-3000 Hz)
          channel_khz: 12.5   # sets the IF bandwidth (8 kHz; 16 kHz at 25); or if_bandwidth_hz
          deviation_hz: 2500  # peak deviation from full-scale TX audio
          limiter_hz: 2500    # optional hard limit on deviation
          tx_power_w: 25
          noise_figure_db: 7
          site_noise: residential   # ITU-R P.372: business | residential | rural | quiet_rural
          frequency_error_hz: 150   # this transmitter's offset from channel centre
          rx_level_dbfs: -6         # receive audio level for full deviation
          squelch: open             # open, or { threshold_dbm: -118, open_ms: 30, tail_ms: 150 }
links:
  - { from: a.fm, to: b.fm, path_loss_db: 125 }   # or rx_dbm: -110
```

- Sensible defaults for every `radio` key, so a port can omit the block and get a generic 12.5 kHz data-port radio.
- `path_loss_db` or `rx_dbm` replace `loss_db` under `channel_model: fm`. `loss_db` is refused there rather than silently changing meaning.
- `capture_db` and `collision_mode` are refused under `fm`: capture and collisions are no longer rules.
- `noise_db` and `default_noise_db` are refused under `fm`: the noise floor comes from `noise_figure_db` and `site_noise` through the link budget.
- `squelch_open_ms` moves into the `squelch` block. A closed squelch outputs silence until a carrier clears the threshold, opens after `open_ms`, and adds a burst of hiss (the squelch tail) for `tail_ms` after the carrier drops, as real radios do.
- An optional `rx_response` table (dB per kHz band) lets a port reproduce a measured radio, such as radio1's receive path.
- `/api/status` and the event stream report each receiver's carriers with their received level and carrier-to-noise ratio, instead of the current mixer decision.

### Port or reuse M0LTE.FmChannel

This is the main decision for you. The pdn csproj notes that the model was pulled into a package precisely so there would be one definition, and "a channel model with two copies is two sets of numbers waiting to disagree".

- **A. Port the stages to Go, pinned to the C# by conformance tests (recommended).** net-sim stays a plain Go build (install.sh, Docker, no .NET). A small reference tool in `tools/fmref`, using the M0LTE.FmChannel package, generates test vectors that are committed to `testdata`: noiseless outputs through each profile, which must match closely, and output-SNR, click-rate and spectrum curves against CNR, which must match statistically (the random number generators differ, so noisy outputs can't match sample for sample). A change to the C# model then shows up as a failing net-sim test once the vectors are regenerated.
- **B. Extend M0LTE.FmChannel with a streaming, multi-carrier API and call it from net-sim.** One definition in the strict sense, and pdn could use the multi-carrier part for its own capture and collision tests. But net-sim's build then needs the .NET SDK and cgo (a NativeAOT shared library) or a sidecar process on the 10 ms audio path, and install.sh either builds .NET or fetches a prebuilt library from the apt repository.

Either way, the streaming and multi-carrier logic is new code that doesn't exist in the C# today.

Licence: M0LTE.FmChannel is GPL-3.0-or-later and net-sim is AGPL-3.0. GPLv3 section 13 allows the combination, and you hold the copyright on both, so neither option is blocked.

### CPU

The IF rate is the audio rate times 2 or 4 (4 times Carson's bandwidth, as FmChannel chooses): 96 kHz for a 12.5 kHz voice channel, 192 kHz for a 25 kHz data port. A direct IF filter at 192 kHz would cost a noticeable fraction of a core per receiving port, so:

- IF and audio filters use FFT overlap-save convolution rather than direct FIRs.
- A receiver with no carrier and a closed squelch does no work. An idle open-squelch receiver can play from a long precomputed stretch of its own hiss rather than recomputing it every block.
- Target: under 5 % of one core per receiving port at 25 kHz data-port settings, measured with a benchmark, so a 20-port topology fits on the current LXC.

## Validation

The model is only worth having if it is right, so each of these is a test, and the numbers come from measurement or from the radio's published figures.

1. **Conformance with M0LTE.FmChannel** (option A): noiseless output within -40 dB of the reference, output SNR within 0.5 dB and click rate within 20 % of it across CNR 0 to 30 dB, for the data-port and mic profiles.
2. **FM physics**: threshold knee near 10 dB CNR; output SNR rising about 1 dB per dB above it; discriminator noise rising with frequency on the data path.
3. **SINAD sensitivity**: a 1 kHz tone at 60 % deviation through a Tait TM8100 narrow profile reads 12 dB SINAD at -121 dBm (Tait's published figure, which FmChannel's `TaitTm8100` carries).
4. **radio1**: with radio1's measured receive response, idle hiss is -15.9 dBFS in 0 to 14 kHz, traffic drops it by about 3 dB, the 16 to 23.5 kHz hiss collapses by about 18 dB under a carrier, and an unmodulated carrier sits near -55 dBFS. Per-kHz spectra within 3 dB of the `OpenSquelchFmReceiver` table.
5. **Capture**: decode rate against the level difference between two stations, compared with the co-channel rejection figure in the radio's specification. The discriminator alone may capture more cleanly than a real radio; if so, the plan's answer is to say so rather than invent a correction.
6. **pdn's carrier sense**: pdn's `FmShapeBusyDetector` should give the same verdicts on net-sim audio as it does on the radio1 recordings. Then repeat the hold-off and c4fsk tests from the pdn integration and report what they actually show.
7. **Regression**: every existing end-to-end test passes under `channel_model: fm` with strong links.

## Order of work

Each is one PR with one reviewer.

1. 48 kHz router (release).
2. The FM stages as a streaming Go package, with unit tests and, under option A, the reference tool and vectors. Not yet wired into the router.
3. Router and config integration behind `channel_model: fm`: carriers, link budget, frequency error, squelch, status and events.
4. Calibration against radio1 and the Tait figures, capture check, pdn detector check, example configs, a short README section. Release.
5. Later, once it has been used for a while: make `fm` the default.

Rough size: step 1 is about a day; steps 2 to 4 are several days, most of it in validation.

## Decisions needed

1. Port to Go with conformance tests (A, recommended) or extend and call M0LTE.FmChannel (B).
2. Keep `simple` as the default until the FM model is calibrated (recommended), or switch straight away.
3. Ship the 48 kHz change first as its own release (recommended).
4. Whether real bench measurements are wanted beyond radio1's existing captures, for example a SINAD or capture measurement on radio1 and radio2 with a step attenuator, which would make items 3 and 5 a comparison with your own radios rather than with datasheets.
