package flex

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FMTAssistant/internal/iq"
	"github.com/va3mw/FMTAssistant/internal/logx"
)

// DAX IQ: the radio streams a panadapter's IQ as VITA-49 UDP packets to the
// port a client names with "client udpport". The stream is centred on the
// panadapter, so the slice being measured must sit inside its bandwidth.
// Receiving IQ changes nothing on the air; the app still never transmits.

// applyPan reads "display pan 0x40000000 center=7.064 bandwidth=0.2
// daxiq_channel=1 client_handle=0x..." or "... removed".
func (r *Radio) applyPan(id string, kv map[string]string) {
	r.mu.Lock()
	if _, gone := kv["removed"]; gone {
		delete(r.pans, id)
		r.mu.Unlock()
		return
	}
	p := r.pans[id]
	if p == nil {
		p = &Pan{ID: id}
		r.pans[id] = p
	}
	for k, v := range kv {
		switch k {
		case "center":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				p.CenterHz = int64(f*1e6 + 0.5)
			}
		case "bandwidth":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				p.BandwidthHz = int64(f*1e6 + 0.5)
			}
		case "daxiq_channel", "daxiq":
			p.DAXIQChannel, _ = strconv.Atoi(v)
		case "client_handle":
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			p.ClientHandle = uint32(h)
		}
	}
	cp, cb := *p, r.OnPan
	r.mu.Unlock()
	if cb != nil {
		cb(cp)
	}
}

// Pan returns a panadapter by ID.
func (r *Radio) Pan(id string) (Pan, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pans[strings.ToLower(id)]; p != nil {
		return *p, true
	}
	return Pan{}, false
}

// CommandReply sends cmd and returns the radio's reply message.
func (r *Radio) CommandReply(cmd string, timeout time.Duration) (string, error) {
	type reply struct {
		code uint32
		msg  string
	}
	ch := make(chan reply, 1)
	r.Command(cmd, func(code uint32, msg string) { ch <- reply{code, msg} })
	select {
	case rep := <-ch:
		if rep.code != 0 {
			return rep.msg, fmt.Errorf("radio refused %q (0x%08X %s)", cmd, rep.code, rep.msg)
		}
		return rep.msg, nil
	case <-time.After(timeout):
		return "", fmt.Errorf("no reply from the radio to %q", cmd)
	}
}

// SetUDPPort tells the radio where to send our streams, once per connection.
func (r *Radio) SetUDPPort(port int) error {
	r.mu.Lock()
	done := r.udpPort == port
	r.mu.Unlock()
	if done {
		return nil
	}
	if err := r.CommandWait(fmt.Sprintf("client udpport %d", port), 3*time.Second); err != nil {
		return err
	}
	r.mu.Lock()
	r.udpPort = port
	r.mu.Unlock()
	return nil
}

// SetPanDAXIQ assigns a DAX IQ channel to a panadapter.
func (r *Radio) SetPanDAXIQ(pan string, ch int) error {
	return r.CommandWait(fmt.Sprintf("display pan set %s daxiq_channel=%d", pan, ch), 3*time.Second)
}

// CenterPan moves a panadapter's centre. The slices on it stay where they are.
func (r *Radio) CenterPan(pan string, hz int64) error {
	return r.CommandWait(fmt.Sprintf("display pan set %s center=%.6f", pan, float64(hz)/1e6), 3*time.Second)
}

// StartDAXIQ asks for a DAX IQ stream on a channel and sets its rate. It
// returns the stream ID. The rate is best effort: a refusal is returned as
// rateErr while the stream still runs at the radio's rate.
func (r *Radio) StartDAXIQ(ch, rate int) (id uint32, rateErr error, err error) {
	msg, err := r.CommandReply(fmt.Sprintf("stream create type=dax_iq daxiq_channel=%d", ch), 5*time.Second)
	if err != nil {
		// SmartSDR v2 used a different form.
		var err2 error
		if msg, err2 = r.CommandReply(fmt.Sprintf("stream create daxiq=%d", ch), 5*time.Second); err2 != nil {
			return 0, nil, err
		}
	}
	id, err = ParseStreamID(msg)
	if err != nil {
		return 0, nil, err
	}
	if rate > 0 {
		rateErr = r.CommandWait(fmt.Sprintf("stream set 0x%08X daxiq_rate=%d", id, rate), 3*time.Second)
	}
	return id, rateErr, nil
}

// StopDAXIQ removes a stream.
func (r *Radio) StopDAXIQ(id uint32) error {
	return r.CommandWait(fmt.Sprintf("stream remove 0x%08X", id), 3*time.Second)
}

// ParseStreamID reads the stream ID from a "stream create" reply.
func ParseStreamID(msg string) (uint32, error) {
	s := strings.TrimSpace(msg)
	if f := strings.Fields(s); len(f) > 0 {
		s = f[0]
	}
	if _, v, ok := strings.Cut(s, "="); ok {
		s = v
	}
	v, err := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 32)
	if err != nil || v == 0 {
		return 0, fmt.Errorf("unexpected stream create reply %q", msg)
	}
	return uint32(v), nil
}

// Receiver listens for VITA-49 stream packets on one UDP port and hands
// each to the handler registered for its stream ID.
type Receiver struct {
	log  *logx.Source
	mu   sync.Mutex
	conn *net.UDPConn
	subs map[uint32]func(iq.Packet, time.Time)
	// counts of packets for streams nobody asked for, logged once each
	strays map[uint32]bool
}

func NewReceiver(l *logx.Logger) *Receiver {
	return &Receiver{log: l.Src("udp"), subs: map[uint32]func(iq.Packet, time.Time){}, strays: map[uint32]bool{}}
}

// Port opens the socket if needed and returns its port.
func (rc *Receiver) Port() (int, error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.conn != nil {
		return rc.conn.LocalAddr().(*net.UDPAddr).Port, nil
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return 0, fmt.Errorf("cannot open a UDP port for the IQ stream: %w", err)
	}
	_ = c.SetReadBuffer(8 << 20)
	rc.conn = c
	port := c.LocalAddr().(*net.UDPAddr).Port
	rc.log.Info("listening for radio streams on UDP %d", port)
	go rc.run(c)
	return port, nil
}

// Subscribe routes a stream's packets to h (nil removes it).
func (rc *Receiver) Subscribe(id uint32, h func(iq.Packet, time.Time)) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if h == nil {
		delete(rc.subs, id)
	} else {
		rc.subs[id] = h
	}
}

// Close stops listening.
func (rc *Receiver) Close() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.conn != nil {
		rc.conn.Close()
		rc.conn = nil
	}
}

func (rc *Receiver) run(c *net.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			rc.log.Debug("stream socket closed: %v", err)
			return
		}
		at := time.Now().UTC()
		p, err := iq.ParseVITA(buf[:n])
		if err != nil {
			rc.log.Debug("bad VITA packet: %v", err)
			continue
		}
		rc.mu.Lock()
		h := rc.subs[p.StreamID]
		if h == nil && !rc.strays[p.StreamID] {
			rc.strays[p.StreamID] = true
			rc.log.Debug("packets for stream 0x%08X, which nothing records", p.StreamID)
		}
		rc.mu.Unlock()
		if h != nil {
			h(p, at) // the handler copies what it keeps; buf is reused
		}
	}
}
