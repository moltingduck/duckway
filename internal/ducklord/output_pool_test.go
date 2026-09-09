package ducklord

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

type fakeOutputResource struct {
	snapshot func(context.Context) error
	onClose  func()
	closed   atomic.Int32
}

func (r *fakeOutputResource) Snapshot(ctx context.Context) error {
	if r.snapshot != nil {
		return r.snapshot(ctx)
	}
	return nil
}

func (r *fakeOutputResource) Close() error {
	if r.closed.Add(1) == 1 && r.onClose != nil {
		r.onClose()
	}
	return nil
}

func outputKey(name string) OutputKey {
	return OutputKey{ClientKey: "host", InstanceID: "instance", SessionID: name, RuntimeGeneration: 1}
}

func TestOutputPoolHandoffRollbackAndCommitAreAtomic(t *testing.T) {
	pool, err := NewOutputPool(2)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[string]*fakeOutputResource{}
	var live atomic.Int32
	var maxLive atomic.Int32
	open := func(_ context.Context, key OutputKey, _ uint64) (OutputResource, error) {
		current := live.Add(1)
		for observed := maxLive.Load(); current > observed && !maxLive.CompareAndSwap(observed, current); observed = maxLive.Load() {
		}
		resource := &fakeOutputResource{onClose: func() { live.Add(-1) }}
		resources[key.SessionID] = resource
		return resource, nil
	}
	for _, name := range []string{"B", "A"} { // B is LRU; A is active.
		if _, err := pool.Activate(context.Background(), outputKey(name), open); err != nil {
			t.Fatal(err)
		}
	}
	before := pool.Status()
	admitted := make(chan struct{})
	release := make(chan struct{})
	failed := errors.New("destination snapshot failed")
	result := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), outputKey("C"), func(_ context.Context, _ OutputKey, _ uint64) (OutputResource, error) {
			close(admitted)
			<-release
			return nil, failed
		})
		result <- err
	}()
	<-admitted
	status := pool.Status()
	if !status.Handoff || status.Count != 2 || status.Active == nil || status.Active.SessionID != "A" {
		t.Fatalf("pool changed before handoff commit: %+v", status)
	}
	openedD := false
	if _, err := pool.Activate(context.Background(), outputKey("D"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		openedD = true
		return &fakeOutputResource{}, nil
	}); !errors.Is(err, ErrHandoffBusy) || openedD {
		t.Fatalf("second handoff err=%v opened=%v", err, openedD)
	}
	close(release)
	if err := <-result; !errors.Is(err, failed) {
		t.Fatalf("failed handoff err=%v", err)
	}
	after := pool.Status()
	if after.Handoff || after.Active == nil || *after.Active != *before.Active || !reflect.DeepEqual(after.Desired, before.Desired) {
		t.Fatalf("failed handoff was not atomic: before=%+v after=%+v", before, after)
	}
	if resources["A"].closed.Load() != 0 || resources["B"].closed.Load() != 0 {
		t.Fatalf("wrong resources closed after rollback")
	}

	activation, err := pool.Activate(context.Background(), outputKey("C"), open)
	if err != nil {
		t.Fatal(err)
	}
	if activation.Evicted == nil || activation.Evicted.SessionID != "B" {
		t.Fatalf("evicted=%+v", activation.Evicted)
	}
	status = pool.Status()
	if status.Count != 2 || status.Active == nil || status.Active.SessionID != "C" || status.Handoff {
		t.Fatalf("committed status=%+v", status)
	}
	if resources["B"].closed.Load() != 1 {
		t.Fatalf("evicted subscription close count=%d", resources["B"].closed.Load())
	}
	if live.Load() != 2 || maxLive.Load() != 3 {
		t.Fatalf("live=%d max=%d, want 2 and one temporary +1 slot", live.Load(), maxLive.Load())
	}
}

func TestOutputPoolSnapshotFailureRollsBackDestination(t *testing.T) {
	pool, _ := NewOutputPool(1)
	snapshotErr := errors.New("disk full")
	victim := &fakeOutputResource{snapshot: func(context.Context) error { return snapshotErr }}
	if _, err := pool.Activate(context.Background(), outputKey("A"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return victim, nil
	}); err != nil {
		t.Fatal(err)
	}
	destination := &fakeOutputResource{}
	if _, err := pool.Activate(context.Background(), outputKey("B"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return destination, nil
	}); !errors.Is(err, snapshotErr) {
		t.Fatalf("err=%v", err)
	}
	status := pool.Status()
	if status.Active == nil || status.Active.SessionID != "A" || status.Count != 1 {
		t.Fatalf("status=%+v", status)
	}
	if victim.closed.Load() != 0 || destination.closed.Load() != 1 {
		t.Fatalf("victim closed=%d destination closed=%d", victim.closed.Load(), destination.closed.Load())
	}
}

func TestOutputPoolLeaseFencesStaleReader(t *testing.T) {
	pool, _ := NewOutputPool(1)
	activation, err := pool.Activate(context.Background(), outputKey("A"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var applied atomic.Int32
	if err := pool.WithLease(outputKey("A"), activation.Lease+1, func(OutputResource) { applied.Add(1) }); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("stale lease err=%v", err)
	}
	if err := pool.WithLease(outputKey("A"), activation.Lease, func(OutputResource) { applied.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if applied.Load() != 1 {
		t.Fatalf("applied=%d", applied.Load())
	}
}

func TestOutputPoolCloseDuringBlockedOpen(t *testing.T) {
	pool, _ := NewOutputPool(1)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), outputKey("A"), func(ctx context.Context, _ OutputKey, _ uint64) (OutputResource, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		done <- err
	}()
	<-started
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("activation err=%v", err)
	}
}

func TestOutputPoolConcurrentExistingActivationDoesNotRace(t *testing.T) {
	pool, _ := NewOutputPool(2)
	activation, err := pool.Activate(context.Background(), outputKey("A"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, activateErr := pool.Activate(context.Background(), outputKey("A"), func(context.Context, OutputKey, uint64) (OutputResource, error) {
				t.Error("existing activation called opener")
				return nil, nil
			})
			if activateErr != nil || got.Lease != activation.Lease || !got.Existing {
				t.Errorf("activation=%+v err=%v", got, activateErr)
			}
		}()
	}
	wg.Wait()
}
