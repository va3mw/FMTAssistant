package flex

import (
	"bufio"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/va3mw/FMTAssistant/internal/logx"
)

// Slice mirrors the radio's slice status.
type Slice struct {
	Index        int    `json:"index"`
	Letter       string `json:"letter"`
	InUse        bool   `json:"in_use"`
	FreqHz       int64  `json:"freq_hz"`
	Mode         string `json:"mode"`
	ClientHandle uint32 `json:"client_handle"`
	RITOn        bool   `json:"rit_on"`
	RITFreq      int    `json:"rit_freq"`
	DiguOffset   int    `json:"digu_offset"`
}

// Station is a GUI client (SmartSDR, Maestro, ...) connected to the radio.
type Station struct {
	Handle   uint32 `json:"handle"`
	ClientID string `json:"client_id"`
	Program  string `json:"program"`
	Name     string `json:"name"`
}

// Snapshot is a consistent copy of the radio state.
type Snapshot struct {
	Connected    bool      `json:"connected"`
	IP           string    `json:"ip"`
	Model        string    `json:"model"`
	Nickname     string    `json:"nickname"`
	Stations     []Station `json:"stations"`
	BoundStation string    `json:"bound_station"`
	BoundHandle  uint32    `json:"bound_handle"`
	Slices       []Slice   `json:"slices"`
}

// SliceByLetter finds an in-use slice owned by the bound station.
func (s *Snapshot) SliceByLetter(letter string) (Slice, bool) {
	if s.BoundHandle == 0 {
		return Slice{}, false
	}
	for _, sl := range s.Slices {
		if sl.InUse && sl.ClientHandle == s.BoundHandle && strings.EqualFold(sl.Letter, letter) {
			return sl, true
		}
	}
	return Slice{}, false
}

type replyFn func(code uint32, msg string)

// Radio is a non-GUI client of the SmartSDR TCP API (port 4992). It only
// reads slice state and tunes slices; it never transmits.
type Radio struct {
	log      *logx.Source
	onUpdate func()
	notifyCh chan struct{}

	// OnSliceFreq (optional) is told about every slice frequency change.
	OnSliceFreq func(letter string, hz int64)

	mu          sync.Mutex
	conn        net.Conn
	connected   bool
	ip, model   string
	nickname    string
	seq         uint32
	pending     map[uint32]replyFn
	myHandle    uint32
	slices      map[int]*Slice
	stations    map[uint32]*Station
	wantStation string
	boundHandle uint32
	boundName   string
	lastRx      time.Time
	stop        chan struct{}
}

func NewRadio(l *logx.Logger, onUpdate func()) *Radio {
	r := &Radio{log: l.Src("radio"), onUpdate: onUpdate, notifyCh: make(chan struct{}, 1)}
	go r.notifyLoop()
	return r
}

func (r *Radio) notifyLoop() {
	for range r.notifyCh {
		if r.onUpdate != nil {
			r.onUpdate()
		}
	}
}

// Connect starts a connection loop that reconnects until Disconnect is called.
func (r *Radio) Connect(ip, model, station string) {
	r.Disconnect()
	r.mu.Lock()
	r.ip, r.model, r.wantStation = ip, model, station
	stop := make(chan struct{})
	r.stop = stop
	r.mu.Unlock()
	go r.loop(ip, stop)
}

func (r *Radio) Disconnect() {
	r.mu.Lock()
	if r.stop != nil {
		close(r.stop)
		r.stop = nil
	}
	if r.conn != nil {
		r.conn.Close()
	}
	r.mu.Unlock()
}

// SetStation selects which GUI station's slices we control, and binds to it.
func (r *Radio) SetStation(name string) {
	r.mu.Lock()
	r.wantStation = name
	r.boundHandle, r.boundName = 0, ""
	r.mu.Unlock()
	r.tryBind()
	r.notify()
}

func (r *Radio) loop(ip string, stop chan struct{}) {
	for {
		err := r.session(ip, stop)
		select {
		case <-stop:
			r.log.Info("disconnected from radio")
			return
		default:
		}
		r.log.Warn("radio connection lost: %v; retrying in 3 s", err)
		select {
		case <-stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (r *Radio) session(ip string, stop chan struct{}) error {
	r.log.Info("connecting to radio at %s:4992", ip)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, "4992"), 5*time.Second)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.conn = conn
	r.pending = map[uint32]replyFn{}
	r.slices = map[int]*Slice{}
	r.stations = map[uint32]*Station{}
	r.boundHandle, r.boundName, r.myHandle = 0, "", 0
	r.lastRx = time.Now()
	r.mu.Unlock()

	done := make(chan struct{})
	defer func() {
		close(done)
		conn.Close()
		r.mu.Lock()
		r.connected = false
		r.conn = nil
		r.boundHandle, r.boundName = 0, ""
		r.mu.Unlock()
		r.notify()
	}()

	// Watchdog: ping every 2 s, drop the link if nothing heard for 10 s.
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-stop:
				conn.Close()
				return
			case <-t.C:
				r.mu.Lock()
				silent := time.Since(r.lastRx)
				r.mu.Unlock()
				if silent > 10*time.Second {
					r.log.Error("radio silent for %v, closing connection", silent.Round(time.Second))
					conn.Close()
					return
				}
				r.Command("ping", nil)
			}
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		r.mu.Lock()
		r.lastRx = time.Now()
		r.mu.Unlock()
		r.handleLine(line)
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return fmt.Errorf("connection closed by radio")
}

func (r *Radio) handleLine(line string) {
	if line == "" {
		return
	}
	switch line[0] {
	case 'V':
		r.log.Info("radio API version %s", line[1:])
	case 'H':
		h, _ := strconv.ParseUint(line[1:], 16, 32)
		r.mu.Lock()
		r.myHandle = uint32(h)
		r.connected = true
		r.mu.Unlock()
		r.log.Info("connected, our client handle 0x%08X", h)
		for _, s := range []string{"sub client all", "sub slice all", "sub radio all"} {
			r.Command(s, nil)
		}
		r.Command("info", r.onInfo)
		r.notify()
	case 'R':
		r.handleReply(line[1:])
	case 'S':
		r.log.Traffic("<- %s", line)
		if _, body, ok := strings.Cut(line, "|"); ok {
			if r.handleStatus(body) {
				r.notify()
			}
		}
	case 'M':
		r.log.Debug("radio message: %s", line[1:])
	default:
		r.log.Traffic("<- %s", line)
	}
}

func (r *Radio) handleReply(s string) {
	parts := strings.SplitN(s, "|", 3)
	if len(parts) < 2 {
		return
	}
	seq, _ := strconv.ParseUint(parts[0], 10, 32)
	code, _ := strconv.ParseUint(parts[1], 16, 32)
	msg := ""
	if len(parts) == 3 {
		msg = parts[2]
	}
	r.mu.Lock()
	cb := r.pending[uint32(seq)]
	delete(r.pending, uint32(seq))
	r.mu.Unlock()
	if cb != nil {
		cb(uint32(code), msg)
	}
}

// onInfo takes the model name from the "info" reply, so a radio reached by
// manual IP (no discovery packet) still reports its model.
func (r *Radio) onInfo(code uint32, msg string) {
	if code != 0 {
		return
	}
	model := ParseInfo(msg)["model"]
	if model == "" {
		return
	}
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
	r.notify()
}

// ParseInfo splits an info reply: key="value",key=value,...
func ParseInfo(s string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(f), "=")
		if ok {
			m[k] = strings.Trim(v, `"`)
		}
	}
	return m
}

// Command sends an API command. cb (optional) receives the reply. Failures are
// always logged, so callers rarely need a callback.
func (r *Radio) Command(cmd string, cb replyFn) {
	r.mu.Lock()
	conn := r.conn
	if conn == nil {
		r.mu.Unlock()
		if cmd != "ping" {
			r.log.Warn("not connected, dropped command: %s", cmd)
		}
		if cb != nil {
			cb(0xFFFFFFFF, "not connected")
		}
		return
	}
	r.seq++
	seq := r.seq
	quiet := cmd == "ping"
	r.pending[seq] = func(code uint32, msg string) {
		if code != 0 {
			r.log.Warn("command failed (0x%08X %s): %s", code, msg, cmd)
		} else if !quiet {
			r.log.Traffic("<- R%d ok %s", seq, msg)
		}
		if cb != nil {
			cb(code, msg)
		}
	}
	r.mu.Unlock()
	if !quiet {
		r.log.Debug("-> C%d|%s", seq, cmd)
	}
	conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if _, err := fmt.Fprintf(conn, "C%d|%s\n", seq, cmd); err != nil {
		r.log.Error("write to radio failed: %v", err)
		conn.Close()
	}
}

// handleStatus applies a status line. Returns true if state changed.
func (r *Radio) handleStatus(body string) bool {
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "slice":
		if len(fields) < 2 {
			return false
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil {
			return false
		}
		r.applySlice(idx, ParseKV(strings.Join(fields[2:], " ")))
		return true
	case "client":
		if len(fields) < 3 {
			return false
		}
		h, _ := strconv.ParseUint(strings.TrimPrefix(fields[1], "0x"), 16, 32)
		r.applyClient(uint32(h), fields[2], ParseKV(strings.Join(fields[3:], " ")))
		return true
	case "radio":
		if len(fields) < 2 || !strings.Contains(fields[1], "=") {
			return false
		}
		kv := ParseKV(strings.Join(fields[1:], " "))
		r.mu.Lock()
		if v, ok := kv["nickname"]; ok {
			r.nickname = unescape(v)
		}
		r.mu.Unlock()
		return true
	}
	return false
}

func (r *Radio) applySlice(idx int, kv map[string]string) {
	r.mu.Lock()
	s := r.slices[idx]
	if s == nil {
		s = &Slice{Index: idx}
		r.slices[idx] = s
	}
	prevHz := s.FreqHz
	for k, v := range kv {
		switch k {
		case "in_use":
			s.InUse = v == "1"
		case "RF_frequency":
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				s.FreqHz = int64(f*1e6 + 0.5)
			}
		case "mode":
			s.Mode = v
		case "client_handle":
			h, _ := strconv.ParseUint(strings.TrimPrefix(v, "0x"), 16, 32)
			s.ClientHandle = uint32(h)
		case "index_letter":
			s.Letter = v
		case "rit_on":
			s.RITOn = v == "1"
		case "rit_freq":
			s.RITFreq, _ = strconv.Atoi(v)
		case "digu_offset":
			s.DiguOffset, _ = strconv.Atoi(v)
		}
	}
	letter, hz := s.Letter, s.FreqHz
	if !s.InUse {
		delete(r.slices, idx)
	}
	cb := r.OnSliceFreq
	r.mu.Unlock()
	if cb != nil && hz != prevHz && letter != "" {
		cb(letter, hz)
	}
}

func (r *Radio) applyClient(h uint32, state string, kv map[string]string) {
	r.mu.Lock()
	if state == "disconnected" {
		if st, ok := r.stations[h]; ok {
			r.log.Info("station \"%s\" (%s) disconnected", st.Name, st.Program)
		}
		delete(r.stations, h)
		if h == r.boundHandle {
			r.log.Warn("bound station went away; waiting for it to return")
			r.boundHandle, r.boundName = 0, ""
		}
		r.mu.Unlock()
		r.tryBind()
		return
	}
	if h == r.myHandle {
		r.mu.Unlock()
		return
	}
	st := r.stations[h]
	if st == nil {
		st = &Station{Handle: h}
		r.stations[h] = st
	}
	if v, ok := kv["client_id"]; ok {
		st.ClientID = v
	}
	if v, ok := kv["program"]; ok {
		st.Program = unescape(v)
	}
	if v, ok := kv["station"]; ok {
		st.Name = unescape(v)
	}
	r.mu.Unlock()
	r.tryBind()
}

// tryBind binds to the wanted station if it is present and not yet bound.
// With no station chosen, a single GUI station is picked automatically.
func (r *Radio) tryBind() {
	r.mu.Lock()
	if !r.connected || r.boundHandle != 0 {
		r.mu.Unlock()
		return
	}
	var gui []*Station
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			gui = append(gui, st)
		}
	}
	var pick *Station
	for _, st := range gui {
		if st.Name == r.wantStation {
			pick = st
		}
	}
	if pick == nil && r.wantStation == "" && len(gui) == 1 {
		pick = gui[0]
	}
	if pick == nil {
		r.mu.Unlock()
		return
	}
	r.boundHandle, r.boundName = pick.Handle, pick.Name
	id, name := pick.ClientID, pick.Name
	r.mu.Unlock()
	r.log.Info("binding to station \"%s\"", name)
	r.Command("client bind client_id="+id, nil)
}

func (r *Radio) notify() {
	select {
	case r.notifyCh <- struct{}{}:
	default: // an update is already pending
	}
}

func (r *Radio) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{
		Connected:    r.connected,
		IP:           r.ip,
		Model:        r.model,
		Nickname:     r.nickname,
		BoundStation: r.boundName,
		BoundHandle:  r.boundHandle,
	}
	for _, st := range r.stations {
		if st.ClientID != "" && st.Name != "" {
			s.Stations = append(s.Stations, *st)
		}
	}
	sort.Slice(s.Stations, func(i, j int) bool { return s.Stations[i].Name < s.Stations[j].Name })
	for _, sl := range r.slices {
		s.Slices = append(s.Slices, *sl)
	}
	sort.Slice(s.Slices, func(i, j int) bool { return s.Slices[i].Index < s.Slices[j].Index })
	return s
}

// CommandWait sends cmd and waits for the radio's reply.
func (r *Radio) CommandWait(cmd string, timeout time.Duration) error {
	type reply struct {
		code uint32
		msg  string
	}
	ch := make(chan reply, 1)
	r.Command(cmd, func(code uint32, msg string) { ch <- reply{code, msg} })
	select {
	case rep := <-ch:
		if rep.code != 0 {
			return fmt.Errorf("radio refused %q (0x%08X %s)", cmd, rep.code, rep.msg)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("no reply from the radio to %q", cmd)
	}
}

// TuneDigital puts a slice in DIGU with RIT off and no DIGU offset, so the
// slice frequency is the true suppressed-carrier (dial) frequency, then tunes
// it to dialHz and checks the radio reports it.
//
// A GUI client (SmartSDR, AetherSDR) may react to a mode change by re-applying
// its own DIGU offset and frequency, so the mode change is allowed to settle
// before tuning, and the tune is retried once if something put it back.
//
// The radio does not echo our own changes back as status, so the result is
// checked by re-subscribing, which makes the radio resend the slice's state.
func (r *Radio) TuneDigital(slice int, dialHz int64) error {
	if err := r.CommandWait(fmt.Sprintf("slice set %d mode=DIGU", slice), 3*time.Second); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)
	var got Slice
	for attempt := 0; attempt < 2; attempt++ {
		if err := r.CommandWait(fmt.Sprintf("slice set %d digu_offset=0 rit_on=0", slice), 3*time.Second); err != nil {
			return err
		}
		if err := r.CommandWait(fmt.Sprintf("slice tune %d %.6f", slice, float64(dialHz)/1e6), 3*time.Second); err != nil {
			return err
		}
		time.Sleep(700 * time.Millisecond)
		if err := r.CommandWait("sub slice all", 3*time.Second); err != nil {
			return err
		}
		r.mu.Lock()
		if s := r.slices[slice]; s != nil {
			got = *s
		}
		r.mu.Unlock()
		r.notify()
		if got.FreqHz == dialHz && got.Mode == "DIGU" && got.DiguOffset == 0 && !got.RITOn {
			return nil
		}
		r.log.Warn("slice %d reads %d Hz %s offset %d after tuning to %d Hz; retrying", slice, got.FreqHz, got.Mode, got.DiguOffset, dialHz)
	}
	return fmt.Errorf("the radio reports %.3f kHz %s (DIGU offset %d) instead of %.3f kHz DIGU; another program may be changing it",
		float64(got.FreqHz)/1000, got.Mode, got.DiguOffset, float64(dialHz)/1000)
}
