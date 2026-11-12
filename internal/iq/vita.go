// Package iq records a FlexRadio DAX IQ stream to a WAV file for later
// analysis. It holds the parts that don't need the radio: VITA-49 packet
// parsing, sample decoding and the WAV writer.
package iq

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Packet is one VITA-49 packet as sent by a FlexRadio.
type Packet struct {
	Type        uint8 // 1: IF data with stream ID, 3: extension data with stream ID
	Count       uint8 // 4-bit packet counter, for loss detection
	StreamID    uint32
	HasClass    bool
	OUI         uint32
	InfoClass   uint16
	PacketClass uint16
	TSI, TSF    uint8 // timestamp kinds (0: none)
	IntTS       uint32
	FracTS      uint64
	Payload     []byte
}

// ParseVITA parses a VITA-49 packet. Payload aliases b.
func ParseVITA(b []byte) (Packet, error) {
	var p Packet
	if len(b) < 8 {
		return p, fmt.Errorf("packet too short (%d bytes)", len(b))
	}
	w0 := binary.BigEndian.Uint32(b)
	p.Type = uint8(w0 >> 28)
	p.HasClass = w0&(1<<27) != 0
	trailer := w0&(1<<26) != 0
	p.TSI = uint8(w0>>22) & 3
	p.TSF = uint8(w0>>20) & 3
	p.Count = uint8(w0>>16) & 0xF
	size := int(w0&0xFFFF) * 4
	if size > len(b) || size < 4 {
		return p, fmt.Errorf("packet size %d does not match %d bytes received", size, len(b))
	}
	b = b[:size]
	off := 4
	if p.Type == 1 || p.Type == 3 || p.Type == 4 || p.Type == 5 {
		if off+4 > len(b) {
			return p, fmt.Errorf("truncated stream ID")
		}
		p.StreamID = binary.BigEndian.Uint32(b[off:])
		off += 4
	}
	if p.HasClass {
		if off+8 > len(b) {
			return p, fmt.Errorf("truncated class ID")
		}
		p.OUI = binary.BigEndian.Uint32(b[off:]) & 0xFFFFFF
		c := binary.BigEndian.Uint32(b[off+4:])
		p.InfoClass, p.PacketClass = uint16(c>>16), uint16(c)
		off += 8
	}
	if p.TSI != 0 {
		if off+4 > len(b) {
			return p, fmt.Errorf("truncated timestamp")
		}
		p.IntTS = binary.BigEndian.Uint32(b[off:])
		off += 4
	}
	if p.TSF != 0 {
		if off+8 > len(b) {
			return p, fmt.Errorf("truncated fractional timestamp")
		}
		p.FracTS = binary.BigEndian.Uint64(b[off:])
		off += 8
	}
	end := len(b)
	if trailer {
		end -= 4
	}
	if end < off {
		return p, fmt.Errorf("no room for payload")
	}
	p.Payload = b[off:end]
	return p, nil
}

// Order is the byte order of the float32 samples in a payload. The SmartSDR
// documentation doesn't pin it down, so it is detected from the data.
type Order int

const (
	OrderUnknown Order = iota
	OrderBig
	OrderLittle
)

func (o Order) String() string {
	switch o {
	case OrderBig:
		return "big-endian"
	case OrderLittle:
		return "little-endian"
	}
	return "unknown"
}

// DetectOrder decides the byte order of a payload of float32 samples. IQ
// samples are normalised to about ±1, so the right order gives small finite
// values and the wrong one gives huge or non-finite ones. It returns
// OrderUnknown when the payload can't tell (all zeros, or neither fits).
func DetectOrder(payload []byte) Order {
	ok := func(order binary.ByteOrder) (sane bool, nonzero bool) {
		for i := 0; i+4 <= len(payload); i += 4 {
			v := math.Float32frombits(order.Uint32(payload[i:]))
			if v != 0 {
				nonzero = true
			}
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) || math.Abs(float64(v)) > 1e3 {
				return false, nonzero
			}
			// A float whose bytes were swapped is usually tiny but not zero.
			if v != 0 && math.Abs(float64(v)) < 1e-30 {
				return false, nonzero
			}
		}
		return true, nonzero
	}
	be, nz := ok(binary.BigEndian)
	if !nz {
		return OrderUnknown
	}
	le, _ := ok(binary.LittleEndian)
	switch {
	case be && !le:
		return OrderBig
	case le && !be:
		return OrderLittle
	case be && le:
		return OrderBig // network order is the VITA-49 default
	}
	return OrderUnknown
}

// Decode converts a payload of float32 samples to dst, which it returns.
func Decode(dst []float32, payload []byte, o Order) []float32 {
	var order binary.ByteOrder = binary.BigEndian
	if o == OrderLittle {
		order = binary.LittleEndian
	}
	dst = dst[:0]
	for i := 0; i+4 <= len(payload); i += 4 {
		dst = append(dst, math.Float32frombits(order.Uint32(payload[i:])))
	}
	return dst
}

// CountGap is the number of packets missing between two 4-bit counters.
func CountGap(prev, cur uint8) int {
	return int((cur - prev - 1) & 0xF)
}
