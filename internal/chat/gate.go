package chat

import (
	"context"
	"sync"
)

type conversationEntry struct {
	busy    bool          //	当前用户对应的锁是否被占用---业务锁
	users   int           //	正在等待锁的用户聊天请求数量
	refs    int           //	正在持有或等待该锁的请求总数
	changed chan struct{} //	状态变化通知
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
	// 没有人在等待或者使用锁
	if entry.refs == 0 {
		delete(g.entries, owner)
	}
}

// 使用的锁引用减少， 等待锁的不变
func (g *conversationGate) unlock(owner uint, entry *conversationEntry) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			entry.busy = false
			entry.refs--
			close(entry.changed)                //通知其他等待的
			entry.changed = make(chan struct{}) // 重置通知通道
			g.cleanup(owner, entry)
		})
	}
}

func (g *conversationGate) acquire(ctx context.Context, owner uint) (func(), error) {
	g.mu.Lock()
	entry := g.entry(owner)
	entry.refs++  // 先持有锁/等待用锁 有意向用锁
	entry.users++ // 先排队
	for {
		// 外部中断
		if err := ctx.Err(); err != nil {
			entry.users--
			entry.refs--
			g.cleanup(owner, entry)
			g.mu.Unlock()
			return nil, err
		}
		// 锁空闲
		if !entry.busy {
			entry.users--
			entry.busy = true // 设置业务锁占用
			g.mu.Unlock()
			return g.unlock(owner, entry), nil
		}
		//锁被占用， 等待锁状态变化
		changed := entry.changed
		g.mu.Unlock()
		select {
		case <-changed: //等待释放
		case <-ctx.Done():
		}
		g.mu.Lock()
	}
}

// Reports yield to active or already-waiting user turns in this process.
func (a *ChatService) TryReportLock(owner uint) (func(), bool) {
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
