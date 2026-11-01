package chat

import (
	"context"
	"sync"
)

type conversationEntry struct {
	busy    bool
	users   int
	refs    int
	changed chan struct{}
}

type conversationGate struct {
	mu      sync.Mutex
	entries map[uint]*conversationEntry
}

func (g *conversationGate) entry(owner uint) *conversationEntry {
	if g.entries == nil {
		g.entries = make(map[uint]*conversationEntry)
	}
	entry := g.entries[owner]
	if entry == nil {
		entry = &conversationEntry{changed: make(chan struct{})}
		g.entries[owner] = entry
	}
	return entry
}

func (g *conversationGate) cleanup(owner uint, entry *conversationEntry) {
	if entry.refs == 0 {
		delete(g.entries, owner)
	}
}

func (g *conversationGate) unlock(owner uint, entry *conversationEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			entry.busy = false
			entry.refs--
			close(entry.changed)
			entry.changed = make(chan struct{})
			g.cleanup(owner, entry)
		})
	}
}

func (g *conversationGate) acquire(ctx context.Context, owner uint) (func(), error) {
	g.mu.Lock()
	entry := g.entry(owner)
	entry.refs++
	entry.users++
	for {
		if err := ctx.Err(); err != nil {
			entry.users--
			entry.refs--
			g.cleanup(owner, entry)
			g.mu.Unlock()
			return nil, err
		}
		if !entry.busy {
			entry.users--
			entry.busy = true
			g.mu.Unlock()
			return g.unlock(owner, entry), nil
		}
		changed := entry.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		g.mu.Lock()
	}
}

// Reports yield to active or already-waiting user turns in this process.
func (a *Agent) TryReportLock(owner uint) (func(), bool) {
	if a == nil || owner == 0 {
		return nil, false
	}
	g := &a.gate
	g.mu.Lock()
	entry := g.entry(owner)
	if entry.busy || entry.users > 0 {
		g.mu.Unlock()
		return nil, false
	}
	entry.busy = true
	entry.refs++
	g.mu.Unlock()
	return g.unlock(owner, entry), true
}
