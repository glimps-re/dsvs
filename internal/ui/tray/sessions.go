package tray

import (
	"maps"
	"slices"
	"time"

	"dsvs/internal/attach"
	"dsvs/internal/hub"
	"dsvs/internal/ui/panel"
)

// discovery is how often the living sessions are listed again: the running ones are followed through
// what they push, but the hub announces a new one nowhere.
const discovery = 3 * time.Second

// follow follows every living session and tells said what they say together, each time it changes —
// and only then. It returns only once done is closed.
func follow(done <-chan struct{}, said func(panel.Mood)) {
	tally := &tally{said: said, states: map[string]attach.State{}}
	reports := make(chan report)
	followed := map[string]bool{}
	discover := func() {
		living, _ := hub.Sessions()
		for _, session := range living {
			if !followed[session.Config] {
				followed[session.Config] = true
				go pump(session.Config, reports, done)
			}
		}
	}

	tick := time.NewTicker(discovery)
	defer tick.Stop()
	discover()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			discover()
		case one := <-reports:
			if one.gone {
				delete(followed, one.config)
				tally.drop(one.config)
				continue
			}
			tally.see(one.config, one.state)
		}
	}
}

type report struct {
	config string
	state  attach.State
	gone   bool
}

// pump carries a session's states until it goes away, then says so. A session that cannot be followed
// goes away the same way: the next discovery will pick it up again.
func pump(config string, reports chan<- report, done <-chan struct{}) {
	if link, err := attach.Watch(config); err == nil {
		defer link.Close()
		for state := range link.States() {
			select {
			case reports <- report{config: config, state: state}:
			case <-done:
				return
			}
		}
	}
	select {
	case reports <- report{config: config, gone: true}:
	case <-done:
	}
}

// tally is what each session said last, and what was concluded from it. The icon starts out drawn as
// "nothing runs": as long as the conclusion does not change, said stays silent, and twenty apps
// starting together make a single drawing.
type tally struct {
	said   func(panel.Mood)
	states map[string]attach.State
	mood   panel.Mood
}

func (t *tally) see(config string, state attach.State) {
	t.states[config] = state
	t.conclude()
}

func (t *tally) drop(config string) {
	delete(t.states, config)
	t.conclude()
}

func (t *tally) conclude() {
	if mood := panel.Overall(slices.Collect(maps.Values(t.states))); mood != t.mood {
		t.mood = mood
		t.said(mood)
	}
}
