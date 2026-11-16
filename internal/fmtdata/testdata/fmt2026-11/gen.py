#!/usr/bin/env python3
"""Writes the FMT 2026-11-13 regression fixture as WSJT-X fmt.all files.

Run from this folder:  python3 gen.py

Where the numbers come from
- Runs: every reading the original session USED (UTC second, fMeas) is copied
  exactly from the VA3MW session. S/N is real for run 4 only; runs 1-3 get a
  plausible 36-42 dB. Each run also gets one edge reading at each end of the
  carrier (the app trims those) and a few weak, flagged CW-ID readings before
  key-down, so the analyzer has to find the carrier the same way it did on
  the night.
- Calibrations: only the summary survived (n, mean DF, SD per station), so the
  readings are synthesized to reproduce those exactly to the 3 decimals
  fmt.all carries. They come in bursts of readings at :04 to :29 of a minute,
  the way WSJT-X FreqCal on its 30 s default writes them. The before file also
  holds 96 readings on 7064 kHz (W8RKO's 40 m frequency), which made it
  "mostly non-WWV". First and last reading times match the session
  (01:42:11-02:15:29 and 03:23:11-03:31:29).
"""
import math

OFFSET = 1500


def line(sec, khz, fmeas, snr):
    """One line formatted exactly like WSJT-X lib/freqcal.f90."""
    flag = "*" if snr < 20 else " "
    return "%02d:%02d:%02d%7d%3d%6d%10.3f%10.3f%7.1f%7.1f  %s" % (
        sec // 3600, (sec // 60) % 60, sec % 60, khz, 1, OFFSET,
        fmeas, fmeas - OFFSET, -30.0, snr, flag)


def hms(s):
    h, m, x = (int(v) for v in s.split(":"))
    return h * 3600 + m * 60 + x


def exact(n, mean, sd):
    """n values with exactly this mean and sample SD (deterministic)."""
    z = [math.sin(2.399963 * (i + 1)) for i in range(n)]  # golden-angle spread
    m = sum(z) / n
    z = [v - m for v in z]
    s = math.sqrt(sum(v * v for v in z) / (n - 1))
    return [mean + sd * v / s for v in z]


def bursts(times, khz, n, mean, sd, snr=35.0):
    """Readings at 2 s spacing from each start time, n in all."""
    vals = exact(n, mean, sd)
    out, k = [], 0
    for start, count in times:
        for j in range(count):
            out.append((hms(start) + 2 * j, khz, OFFSET + vals[k], snr + (k % 5)))
            k += 1
    assert k == n, (khz, k, n)
    return out


def write(name, rows):
    rows.sort(key=lambda r: r[0])
    with open(name, "w", newline="\r\n") as f:
        for r in rows:
            f.write(line(*r) + "\n")


# ---------------------------------------------------------------- calibrations
before = []
before += bursts([("01:42:11", 13)], 2500, 13, 0.024, 0.020)
before += bursts([("01:44:04", 12), ("01:45:04", 12), ("01:46:04", 12), ("01:47:04", 12)], 5000, 48, -0.041, 0.058)
before += bursts([("01:49:04", 12), ("01:50:04", 12)], 10000, 24, -0.016, 0.310)
# The operator tuned W8RKO's 40 m frequency while fmt.all kept running.
before += bursts([("%02d:%02d:04" % divmod(m, 60), 12) for m in range(2 * 60 + 2, 2 * 60 + 10)],
                 7064, 96, 1403.2 - OFFSET, 0.08, snr=30.0)
# One last 2.5 MHz look to close the file at 02:15:29 (kept in the 2.5 MHz n).
last = before.pop(12)  # move the 13th 2.5 MHz reading to the end of the file
before.append((hms("02:15:29"), last[1], last[2], last[3]))
write("cal_before.all", before)

after = []
after += bursts([("03:23:11", 10), ("03:24:04", 11)], 2500, 21, -0.020, 0.065)
after += bursts([("03:25:04", 12), ("03:26:04", 12)], 5000, 24, -0.101, 0.058)
after += bursts([("03:27:04", 12), ("03:28:04", 12)], 10000, 24, -0.202, 0.279)
after += bursts([("03:31:07", 12)], 20000, 12, -0.004, 0.003, snr=45.0)
write("cal_after.all", after)

# ------------------------------------------------------------------------ runs
RUNS = [
    # file, kHz, key-down, edges (sec, fMeas, S/N), readings (sec, fMeas), S/N or None
    ("run1_W8RKO_40m.all", 7064, "02:34:00", [(6, 1403.230, 30.5), (29, 1403.170, 37.0)],
     [(9, 1403.180), (11, 1403.303), (13, 1403.271), (16, 1403.213), (18, 1403.282),
      (20, 1403.243), (23, 1403.154), (25, 1403.163), (27, 1403.158)], None),
    ("run2_K5CM_40m.all", 7065, "02:49:00", [(4, 1624.190, 31.0), (29, 1624.230, 38.0)],
     [(6, 1624.150), (9, 1624.204), (11, 1624.194), (13, 1624.190), (16, 1624.250),
      (18, 1624.249), (20, 1624.184), (23, 1624.211), (25, 1624.307), (27, 1624.223)], None),
    ("run3_W8RKO_80m.all", 3598, "03:04:00", [(6, 2377.360, 30.0), (29, 2377.350, 36.5)],
     [(9, 2377.398), (11, 2377.343), (13, 2377.404), (16, 2377.332), (18, 2377.341),
      (20, 2377.387), (23, 2377.368), (25, 2377.264), (27, 2377.380)], None),
    ("run4_K5CM_80m.all", 3599, "03:19:00", [(4, 1825.050, 28.0), (29, 1825.100, 41.0)],
     [(6, 1825.081), (9, 1825.032), (11, 1824.859), (13, 1824.788), (16, 1824.826),
      (18, 1824.883), (20, 1825.119), (23, 1825.083), (25, 1824.940), (27, 1825.170)],
     [32.2, 38.6, 37.6, 44.3, 45.1, 52.3, 57.2, 56.5, 51.5, 44.7]),
]
for name, khz, kd, edges, readings, snrs in RUNS:
    t0 = hms(kd)
    rows = []
    # CW identification before key-down: weak, flagged, scattered.
    for j, dt in enumerate((-56, -54, -52, -26, -24)):
        rows.append((t0 + dt, khz, readings[0][1] + (0.9, -1.4, 2.1, -0.7, 1.6)[j], 12.0 + j))
    for sec, f, snr in edges:
        rows.append((t0 + sec, khz, f, snr))
    for i, (sec, f) in enumerate(readings):
        snr = snrs[i] if snrs else 36.0 + (i * 3) % 7
        rows.append((t0 + sec, khz, f, snr))
    write(name, rows)
