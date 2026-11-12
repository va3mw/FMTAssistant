# iqrec: DAX IQ recording prototype

A command-line test bed for adding IQ recording to FMT Assistant. It records the
DAX IQ stream of the panadapter a slice is on, using the app's own radio client
(`internal/flex`), and writes:

- `<name>.wav`: 2-channel 32-bit float WAV, I on the left channel, Q on the right,
  at the DAX IQ rate. Opens in Audacity, HDSDR, SDR#, GNU Radio and so on.
- `<name>.json`: what was recorded: the IQ centre frequency, sample rate, slice,
  slice tuning error, frequency reference and lock at the start and end, VITA-49
  timestamps, lost packets (filled with silence so the timing stays right), any
  move of the panadapter, and the strongest signal found.

It never transmits. The only setting it changes is the panadapter's DAX IQ
channel, and only when the pan had none: it then takes a free channel and puts
it back to none when it stops.

## Build

```bash
GOOS=windows go build -o iqrec.exe ./cmd/iqrec
```

## Run

SmartSDR running, slice A open, then in a Command Prompt:

```bash
iqrec.exe -slice A -seconds 60 -out "C:\FMT\IQ"
```

Options: `-ip` (skip discovery), `-station` (if more than one SmartSDR station),
`-rate 24000|48000|96000|192000`, `-name`, `-note`, `-swap`, `-v` (every API line).
A log goes to `iqrec-logs` in the output folder.

## What to check

These are the open questions the prototype is meant to answer:

1. **Stream setup works from a non-GUI client.** Does `display pan set ...
   daxiq_channel=N` succeed on the GUI client's pan, and does `stream create
   type=dax_iq` return a stream ID? Run with `-v` and keep the log if not.
2. **Alongside SmartSDR DAX.** Try once with DAX closed, and once with DAX open
   and its IQ channel already on that pan. Does either side lose its stream?
3. **Packets arrive.** If it keeps saying *no IQ packets yet*, Windows Firewall
   is the likely cause: allow `iqrec.exe` on private networks.
4. **I/Q order and centre.** Tune slice A in DIGU to a known carrier (WWV, or a
   local AM station) about 1500 Hz above the dial. The *Strongest signal* line
   should show it about +1500 Hz from the slice dial, close to dial + WSJT-X's
   fMeas. If it shows on the wrong side of the IQ centre, the I/Q order is
   swapped: run with `-swap` and tell me.
5. **No lost packets.** *gaps* should be 0 on a wired LAN.
6. **Timestamps.** The summary prints the VITA TSI/TSF types; with a GPSDO
   locked, note whether TSI says GPS (2) or UTC (1).

Send me the JSON file and the log from a run or two; the WAV only if something
looks wrong.
