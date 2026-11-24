package rabbitmq

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

type fakeConfirm struct {
	done chan struct{}
	ack  bool
}

func (f fakeConfirm) Done() <-chan struct{} { return f.done }
func (f fakeConfirm) Acked() bool           { return f.ack }

func settled(ack bool) fakeConfirm {
	done := make(chan struct{})
	close(done)
	return fakeConfirm{done: done, ack: ack}
}

var pending = fakeConfirm{done: make(chan struct{})}

// A message the broker never settles must not hide the outcome of the others,
// whichever comes first, so accepted messages are not published again.
func TestSettleRecordsEverySettledConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		confirms []confirmation
		want     []error
	}{
		{"acked, then unsettled", []confirmation{settled(true), pending}, []error{nil, context.DeadlineExceeded}},
		{"unsettled, then acked", []confirmation{pending, settled(true)}, []error{context.DeadlineExceeded, nil}},
		{"refused, then acked", []confirmation{settled(false), settled(true)}, []error{errRefused, nil}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			errs := make([]error, len(tc.confirms))
			unsettled := settle(ctx, tc.confirms, errs)
			for i := range errs {
				if !errors.Is(errs[i], tc.want[i]) {
					t.Errorf("message %d: %v, want %v", i, errs[i], tc.want[i])
				}
			}
			if want := slices.Contains(tc.want, context.DeadlineExceeded); unsettled != want {
				t.Errorf("settle = %v, want %v", unsettled, want)
			}
		})
	}
}

// Release waits for an abort already under way, so the abort of one call can
// never close the socket of the next.
func TestReleaseWaitsForAStartedAbort(t *testing.T) {
	var p Publisher
	ctx, cancel := context.WithCancel(t.Context())
	release := p.abortWhenDone(ctx)
	p.mu.Lock() // the abort will block here once started
	cancel()
	time.Sleep(50 * time.Millisecond)
	var released sync.WaitGroup
	returned := make(chan struct{})
	released.Go(func() {
		release()
		close(returned)
	})
	select {
	case <-returned:
		t.Error("release returned while the abort was still running")
	case <-time.After(100 * time.Millisecond):
	}
	p.mu.Unlock()
	released.Wait()
}
