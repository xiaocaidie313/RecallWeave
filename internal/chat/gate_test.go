package chat

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitForGateUsers(t *testing.T, a *ChatService, owner uint, users int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.gate.mu.Lock()
		entry := a.gate.entries[owner]
		ready := entry != nil && entry.users == users
		a.gate.mu.Unlock()
		if ready {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("user turn did not enter wait queue")
}

func TestConversationGateSerializesReportsAndPrioritizesWaitingUsers(t *testing.T) {
	a := NewChatService(nil, nil, nil, nil)
	first, err := a.gate.acquire(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan func(), 1)
	go func() {
		release, err := a.gate.acquire(context.Background(), 7)
		if err == nil {
			acquired <- release
		}
	}()
	waitForGateUsers(t, a, 7, 1)
	if _, ok := a.TryReportLock(7); ok {
		t.Fatal("report preempted active user")
	}
	other, ok := a.TryReportLock(8)
	if !ok {
		t.Fatal("different conversation was blocked")
	}
	other()
	first()
	if release, ok := a.TryReportLock(7); ok {
		release()
		t.Fatal("report preempted queued user")
	}
	select {
	case release := <-acquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("queued user was not released")
	}
	report, ok := a.TryReportLock(7)
	if !ok {
		t.Fatal("idle conversation rejected report")
	}
	report()
	report()
	a.gate.mu.Lock()
	remaining := len(a.gate.entries)
	a.gate.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("idle gates leaked: %d", remaining)
	}
}

func TestConversationGateWaitingUserCancellationReleasesReferences(t *testing.T) {
	a := NewChatService(nil, nil, nil, nil)
	active, ok := a.TryReportLock(7)
	if !ok {
		t.Fatal("initial lock unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		release, err := a.gate.acquire(ctx, 7)
		if release != nil {
			release()
		}
		done <- err
	}()
	waitForGateUsers(t, a, 7, 1)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled user stayed blocked")
	}
	active()
	a.gate.mu.Lock()
	remaining := len(a.gate.entries)
	a.gate.mu.Unlock()
	if remaining != 0 {
		t.Fatal("cancelled user gate leaked")
	}
}

func TestConversationGateRejectsAlreadyCancelledUserWithoutTakingLock(t *testing.T) {
	a := NewChatService(nil, nil, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if release, err := a.gate.acquire(ctx, 7); release != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected cancelled acquire: %v", err)
	}
	release, ok := a.TryReportLock(7)
	if !ok {
		t.Fatal("cancelled acquire left busy gate")
	}
	release()
}
