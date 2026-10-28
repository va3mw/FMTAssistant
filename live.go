package main

import (
	"path/filepath"
	"time"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/va3mw/FMTAssistant/internal/fmtdata"
)

// Live follow: while a step is active the UI asks the app to watch WSJT-X's
// fmt.all and sends every new reading to the page as a "live" event. This is
// a preview only. Capture and the analysis are unchanged and remain the
// source of the submitted answer.

const maxLiveLines = 20000

// LiveState is everything read from fmt.all since following started (or
// since the file was last replaced).
type LiveState struct {
	Step  string         `json:"step"`  // slot being measured: cal_before, run1..run4, cal_after, practice
	Path  string         `json:"path"`  // the fmt.all being followed
	Gen   int            `json:"gen"`   // bumped whenever the file starts over
	Gone  bool           `json:"gone"`  // no fmt.all at the moment
	Lines []fmtdata.Line `json:"lines"` // all readings in the current file
	Err   string         `json:"err"`
}

// LiveEvent is sent to the UI after each poll that changed something.
type LiveEvent struct {
	Step  string         `json:"step"`
	Gen   int            `json:"gen"`
	Reset bool           `json:"reset"` // drop the lines held for an older gen
	Gone  bool           `json:"gone"`
	Lines []fmtdata.Line `json:"lines"` // new readings only
	Err   string         `json:"err"`
}

type liveFollow struct {
	stop  chan struct{}
	fol   *fmtdata.Follower
	state LiveState
}

// LiveFollow starts following fmt.all for a step, switches the step label,
// or stops following when step is empty. It returns the current state.
func (a *App) LiveFollow(step string) LiveState {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if step == "" {
		if a.live != nil {
			close(a.live.stop)
			a.live = nil
			a.src.Info("live follow stopped")
		}
		return LiveState{}
	}
	if a.live != nil {
		if a.live.state.Step != step {
			a.src.Info("live follow: step %s", step)
		}
		a.live.state.Step = step
		return a.snapshotLocked()
	}
	a.live = &liveFollow{stop: make(chan struct{}), state: LiveState{Step: step}}
	a.src.Info("live follow started for %s", step)
	a.pollLiveLocked()
	go a.liveLoop(a.live)
	return a.snapshotLocked()
}

// GetLive returns what has been read so far (empty when not following).
func (a *App) GetLive() LiveState {
	a.liveMu.Lock()
	defer a.liveMu.Unlock()
	if a.live == nil {
		return LiveState{}
	}
	return a.snapshotLocked()
}

func (a *App) snapshotLocked() LiveState {
	s := a.live.state
	s.Lines = append([]fmtdata.Line(nil), s.Lines...)
	return s
}

func (a *App) liveLoop(lf *liveFollow) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-lf.stop:
			return
		case <-t.C:
		}
		a.liveMu.Lock()
		if a.live != lf { // stopped meanwhile
			a.liveMu.Unlock()
			return
		}
		a.pollLiveLocked()
		a.liveMu.Unlock()
	}
}

// pollLiveLocked reads new lines and emits an event if anything changed.
// A change of WSJT-X folder starts over on the new folder's fmt.all.
func (a *App) pollLiveLocked() {
	a.mu.Lock()
	path := filepath.Join(a.sess.WSJTXDir, "fmt.all")
	a.mu.Unlock()
	lf := a.live
	st := &lf.state
	ev := LiveEvent{Step: st.Step}
	if lf.fol == nil || lf.fol.Path != path {
		ev.Reset = lf.fol != nil
		lf.fol = &fmtdata.Follower{Path: path}
		st.Path = path
	}
	u, err := lf.fol.Poll()
	if err != nil {
		// Typically a brief sharing clash; keep what we have and try again.
		if st.Err != err.Error() {
			a.src.Warn("live follow %s: %v", path, err)
			st.Err = err.Error()
			ev.Err = st.Err
			a.emitLive(ev, st)
		}
		return
	}
	changed := ev.Reset || u.Reset || u.Gone != st.Gone || len(u.Lines) > 0 || st.Err != ""
	if ev.Reset || u.Reset {
		ev.Reset = true
		st.Gen++
		st.Lines = nil
	}
	st.Err = ""
	st.Gone = u.Gone
	st.Lines = append(st.Lines, u.Lines...)
	if len(st.Lines) > maxLiveLines {
		st.Lines = st.Lines[len(st.Lines)-maxLiveLines:]
	}
	if changed {
		ev.Gone, ev.Lines = u.Gone, u.Lines
		a.emitLive(ev, st)
	}
}

func (a *App) emitLive(ev LiveEvent, st *LiveState) {
	ev.Gen = st.Gen
	if ev.Lines == nil {
		ev.Lines = []fmtdata.Line{}
	}
	if a.ctx != nil {
		wruntime.EventsEmit(a.ctx, "live", ev)
	}
}
