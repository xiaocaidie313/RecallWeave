package extract

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
)

func TestSessionGateSerializesSameSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate sessionGate
		release, err := gate.acquire(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		acquired := make(chan func(), 1)
		go func() {
			next, err := gate.acquire(context.Background(), 7)
			if err != nil {
				t.Error(err)
				return
			}
			acquired <- next
		}()
		synctest.Wait()
		select {
		case next := <-acquired:
			next()
			t.Fatal("second extraction acquired a session still held by the first")
		default:
		}
		release()
		next := <-acquired
		next()
		if len(gate.entries) != 0 {
			t.Fatalf("finished extraction left session entries: %d", len(gate.entries))
		}
	})
}

func TestSessionGateAllowsDifferentSessions(t *testing.T) {
	var gate sessionGate
	first, err := gate.acquire(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	second, err := gate.acquire(context.Background(), 8)
	if err != nil {
		t.Fatal(err)
	}
	second()
	first()
	if len(gate.entries) != 0 {
		t.Fatalf("finished sessions left entries: %d", len(gate.entries))
	}
}

func TestSessionGateCanceledWaiterDoesNotReleaseHolder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var gate sessionGate
		release, err := gate.acquire(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		canceled := make(chan error, 1)
		go func() {
			next, err := gate.acquire(ctx, 7)
			if next != nil {
				next()
				t.Error("canceled waiter returned a release function")
			}
			canceled <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter did not return cancellation: %v", err)
		}
		if entry := gate.entries[7]; entry == nil || entry.refs != 1 || len(entry.available) != 0 {
			t.Fatalf("canceled waiter changed the holder or leaked its reference: %+v", entry)
		}
		release()
		if len(gate.entries) != 0 {
			t.Fatalf("canceled waiter left session entries: %d", len(gate.entries))
		}
		next, err := gate.acquire(context.Background(), 7)
		if err != nil {
			t.Fatal(err)
		}
		next()
	})
}

func TestSessionGateRejectsAlreadyCanceledContext(t *testing.T) {
	var gate sessionGate
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	release, err := gate.acquire(ctx, 7)
	if release != nil || !errors.Is(err, context.Canceled) || len(gate.entries) != 0 {
		t.Fatalf("canceled call reached the gate: release=%v err=%v entries=%d", release != nil, err, len(gate.entries))
	}
}

func TestSessionGateReleaseIsIdempotent(t *testing.T) {
	var gate sessionGate
	release, err := gate.acquire(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	release()
	next, err := gate.acquire(context.Background(), 7)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if entry := gate.entries[7]; entry == nil || entry.refs != 1 || len(entry.available) != 0 {
		t.Fatalf("second release changed a new holder: %+v", entry)
	}
	next()
	if len(gate.entries) != 0 {
		t.Fatalf("repeated release left session entries: %d", len(gate.entries))
	}
}
