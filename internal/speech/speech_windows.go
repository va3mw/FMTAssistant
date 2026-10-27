// Package speech speaks prompts with the Windows SAPI voice.
package speech

import (
	"runtime"
	"sync/atomic"

	ole "github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"

	"github.com/va3mw/FMTAssistant/internal/logx"
)

const (
	svsfAsync = 1 // return immediately
	svsfPurge = 2 // cut off whatever is being said
)

type item struct {
	text   string
	volume int
}

// Speaker owns one SAPI voice on a dedicated COM thread.
type Speaker struct {
	log *logx.Source
	ch  chan item
	ok  atomic.Bool
}

func New(l *logx.Logger) *Speaker {
	s := &Speaker{log: l.Src("voice"), ch: make(chan item, 8)}
	ready := make(chan struct{})
	go s.run(ready)
	<-ready
	return s
}

// OK reports whether a SAPI voice is available.
func (s *Speaker) OK() bool { return s.ok.Load() }

// Say speaks text, interrupting anything still being spoken. volume is 0-100.
func (s *Speaker) Say(text string, volume int) {
	select {
	case s.ch <- item{text, volume}:
	default:
		s.log.Warn("voice queue full, dropped: %s", text)
	}
}

func (s *Speaker) run(ready chan struct{}) {
	runtime.LockOSThread()
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		s.log.Debug("CoInitializeEx: %v", err)
	}
	unk, err := oleutil.CreateObject("SAPI.SpVoice")
	if err != nil {
		s.log.Warn("Windows voice not available: %v", err)
		close(ready)
		return
	}
	voice, err := unk.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		s.log.Warn("Windows voice not available: %v", err)
		close(ready)
		return
	}
	s.ok.Store(true)
	close(ready)
	for it := range s.ch {
		v := min(max(it.volume, 0), 100)
		oleutil.PutProperty(voice, "Volume", v)
		if _, err := oleutil.CallMethod(voice, "Speak", it.text, svsfAsync|svsfPurge); err != nil {
			s.log.Warn("speak failed: %v", err)
		}
	}
}
