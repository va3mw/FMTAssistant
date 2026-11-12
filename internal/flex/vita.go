package flex

import (
	"encoding/binary"
	"fmt"
	"math"
)

// FlexRadio's VITA-49 organizationally unique identifier, in the class ID.
const flexOUI = 0x001C2D

// VitaPacket is a decoded VITA-49 data packet as the radio sends it over UDP.
type VitaPacket struct {
	Type        uint8  // 1 = IF data with stream ID
	Count       uint8  // 4-bit packet counter, per stream
	TSI         uint8  // integer timestamp type: 0 none, 1 UTC, 2 GPS, 3 other
	TSF         uint8  // fractional timestamp type: 0 none, 1 sample count, 2 picoseconds, 3 free running
	StreamID    uint32 //
	HasClass    bool   //
	OUI         uint32 // 0x001C2D for FlexRadio
	InfoClass   uint16 //
	PacketClass uint16 // what the payload is, see DAXIQRate
	IntTS       uint32 // integer timestamp (seconds), if TSI != 0
	FracTS      uint64 // fractional timestamp, if TSF != 0
	Payload     []byte // the data words, without header or trailer
}

// ParseVita decodes a VITA-49 packet. The payload aliases b.
func ParseVita(b []byte) (VitaPacket, error) {
	var p VitaPacket
	if len(b) < 4 {
		return p, fmt.Errorf("vita: short packet (%d bytes)", len(b))
	}
	hdr := binary.BigEndian.Uint32(b)
	p.Type = uint8(hdr >> 28)
	p.HasClass = hdr&(1<<27) != 0
	trailer := hdr&(1<<26) != 0
	p.TSI = uint8(hdr>>22) & 3
	p.TSF = uint8(hdr>>20) & 3
	p.Count = uint8(hdr>>16) & 0xF
	size := int(hdr&0xFFFF) * 4
	if size > len(b) || size < 4 {
		return p, fmt.Errorf("vita: packet says %d bytes, got %d", size, len(b))
	}
	b = b[:size]
	off := 4
	need := func(n int) error {
		if off+n > len(b) {
			return fmt.Errorf("vita: header runs past the end of a %d-byte packet", len(b))
		}
		return nil
	}
	if p.Type == 1 || p.Type == 3 || p.Type == 4 || p.Type == 5 {
		if err := need(4); err != nil {
			return p, err
		}
		p.StreamID = binary.BigEndian.Uint32(b[off:])
		off += 4
	}
	if p.HasClass {
		if err := need(8); err != nil {
			return p, err
		}
		p.OUI = binary.BigEndian.Uint32(b[off:]) & 0xFFFFFF
		p.InfoClass = binary.BigEndian.Uint16(b[off+4:])
		p.PacketClass = binary.BigEndian.Uint16(b[off+6:])
		off += 8
	}
	if p.TSI != 0 {
		if err := need(4); err != nil {
			return p, err
		}
		p.IntTS = binary.BigEndian.Uint32(b[off:])
		off += 4
	}
	if p.TSF != 0 {
		if err := need(8); err != nil {
			return p, err
		}
		p.FracTS = binary.BigEndian.Uint64(b[off:])
		off += 8
	}
	end := len(b)
	if trailer {
		end -= 4
	}
	if end < off {
		return p, fmt.Errorf("vita: no room for the payload")
	}
	p.Payload = b[off:end]
	return p, nil
}

// DAXIQRate is the sample rate a FlexRadio DAX IQ packet class stands for,
// or 0 if the class isn't DAX IQ.
func DAXIQRate(packetClass uint16) int {
	switch packetClass {
	case 0x02E3:
		return 24000
	case 0x02E4:
		return 48000
	case 0x02E5:
		return 96000
	case 0x02E6:
		return 192000
	}
	return 0
}

// IQSamples decodes a DAX IQ payload: big-endian float32 pairs, the first of
// each pair taken as I. With swap, the first is taken as Q instead.
func IQSamples(payload []byte, swap bool, dst []complex64) []complex64 {
	dst = dst[:0]
	for i := 0; i+8 <= len(payload); i += 8 {
		a := math.Float32frombits(binary.BigEndian.Uint32(payload[i:]))
		b := math.Float32frombits(binary.BigEndian.Uint32(payload[i+4:]))
		if swap {
			a, b = b, a
		}
		dst = append(dst, complex(a, b))
	}
	return dst
}
