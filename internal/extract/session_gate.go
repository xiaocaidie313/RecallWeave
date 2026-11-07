package extract

import (
	"context"
	"sync"
)

type sessionEntry struct {
	available chan struct{}
	refs      int
}

// sessionGate serializes extraction for each session in this executor. Waiting
// callers can cancel without affecting the current extraction or other sessions.
type sessionGate struct {
	mu      sync.Mutex
	entries map[uint]*sessionEntry
}

func (g *sessionGate) acquire(ctx context.Context, sessionID uint) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if g.entries == nil {
		g.entries = make(map[uint]*sessionEntry)
	}
	entry := g.entries[sessionID]
	if entry == nil {
		entry = &sessionEntry{available: make(chan struct{}, 1)}
		entry.available <- struct{}{}
		g.entries[sessionID] = entry
	}
	entry.refs++
	g.mu.Unlock()

	select {
	case <-ctx.Done():
		g.drop(sessionID, entry, false)
		return nil, ctx.Err()
	case <-entry.available:
	}
	if err := ctx.Err(); err != nil {
		g.drop(sessionID, entry, true)
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() { g.drop(sessionID, entry, true) })
	}, nil
}

func (g *sessionGate) drop(sessionID uint, entry *sessionEntry, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if held {
		entry.available <- struct{}{}
	}
	entry.refs--
	if entry.refs == 0 {
		delete(g.entries, sessionID)
	}
}
