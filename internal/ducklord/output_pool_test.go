package ducklord

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeOutputResource struct {
	snapshot func(context.Context) error
	onClose  func()
	closeErr error
	closed   atomic.Int32
}

type statefulOutputResource struct {
	mu              sync.Mutex
	text            string
	snapshotText    string
	snapshotEntered chan struct{}
	closed          atomic.Int32
}

func (r *statefulOutputResource) append(text string) {
	r.mu.Lock()
	r.text += text
	r.mu.Unlock()
}

func (r *statefulOutputResource) Snapshot(context.Context) error {
	if r.snapshotEntered != nil {
		close(r.snapshotEntered)
	}
	r.mu.Lock()
	r.snapshotText = r.text
	r.mu.Unlock()
	return nil
}

func (r *statefulOutputResource) Close() error { r.closed.Add(1); return nil }

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
	return r.closeErr
}

func outputKey(name string) OutputKey {
	return OutputKey{ClientKey: "host", InstanceID: "instance", SessionID: name}
}

func outputRevision() OutputRevision { return OutputRevision{RuntimeGeneration: 1} }

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
		if _, err := pool.Activate(context.Background(), outputKey(name), outputRevision(), open); err != nil {
			t.Fatal(err)
		}
	}
	before := pool.Status()
	admitted := make(chan struct{})
	release := make(chan struct{})
	failed := errors.New("destination snapshot failed")
	result := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), outputKey("C"), outputRevision(), func(_ context.Context, _ OutputKey, _ uint64) (OutputResource, error) {
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
	if _, err := pool.Activate(context.Background(), outputKey("D"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
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

	activation, err := pool.Activate(context.Background(), outputKey("C"), outputRevision(), open)
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
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return victim, nil
	}); err != nil {
		t.Fatal(err)
	}
	destination := &fakeOutputResource{}
	if _, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
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
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
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
		_, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(ctx context.Context, _ OutputKey, _ uint64) (OutputResource, error) {
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
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
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
			got, activateErr := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
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

func TestOutputPoolDisconnectRestorePreservesDesiredOrder(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resources := map[string]*fakeOutputResource{}
	open := func(_ context.Context, key OutputKey, _ uint64) (OutputResource, error) {
		resource := &fakeOutputResource{}
		resources[key.SessionID] = resource
		return resource, nil
	}
	b, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), open)
	if err != nil {
		t.Fatal(err)
	}
	a, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), open)
	if err != nil {
		t.Fatal(err)
	}
	before := pool.Status()
	if err := pool.DisconnectHost("host", "instance"); err != nil {
		t.Fatal(err)
	}
	disconnected := pool.Status()
	if disconnected.Count != 2 || disconnected.Connected != 0 || disconnected.Active == nil || disconnected.Active.SessionID != "A" || !reflect.DeepEqual(disconnected.Desired, before.Desired) {
		t.Fatalf("disconnect changed desired state: before=%+v after=%+v", before, disconnected)
	}
	if resources["A"].closed.Load() != 1 || resources["B"].closed.Load() != 1 {
		t.Fatalf("disconnect did not close both streams")
	}
	if err := pool.WithLease(outputKey("A"), a.Lease, nil); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("old A lease err=%v", err)
	}
	if err := pool.WithLease(outputKey("B"), b.Lease, nil); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("old B lease err=%v", err)
	}
	wantRestore := []OutputKey{outputKey("A"), outputKey("B")}
	if got := pool.DesiredForHost("host", "instance"); !reflect.DeepEqual(got, wantRestore) {
		t.Fatalf("restore order=%+v want=%+v", got, wantRestore)
	}
	for _, key := range wantRestore {
		if _, err := pool.RestoreDesired(context.Background(), key, outputRevision(), open); err != nil {
			t.Fatal(err)
		}
	}
	after := pool.Status()
	if after.Connected != 2 || after.Active == nil || after.Active.SessionID != "A" || !reflect.DeepEqual(after.Desired, before.Desired) {
		t.Fatalf("restore changed desired state: %+v", after)
	}
}

func TestOutputPoolDisconnectFencesDelayedRestore(t *testing.T) {
	pool, _ := NewOutputPool(1)
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.DisconnectHost("host", "instance"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	late := &fakeOutputResource{}
	done := make(chan error, 1)
	go func() {
		_, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
			close(started)
			<-release // Deliberately model an opener slow to honor cancellation.
			return late, nil
		})
		done <- err
	}()
	<-started
	if _, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); !errors.Is(err, ErrRestoreBusy) {
		t.Fatalf("parallel restore err=%v", err)
	}
	if err := pool.DisconnectHost("host", "instance"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("delayed restore err=%v", err)
	}
	if late.closed.Load() != 1 || pool.Status().Connected != 0 {
		t.Fatalf("late close=%d status=%+v", late.closed.Load(), pool.Status())
	}
	if _, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); err != nil {
		t.Fatalf("fresh restore: %v", err)
	}
}

func TestOutputPoolEvictionQuiescesAcceptedFramesBeforeSnapshot(t *testing.T) {
	pool, _ := NewOutputPool(1)
	victim := &statefulOutputResource{}
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return victim, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	applyEntered := make(chan struct{})
	releaseApply := make(chan struct{})
	applyDone := make(chan error, 1)
	go func() {
		applyDone <- pool.WithLease(outputKey("A"), activation.Lease, func(OutputResource) {
			close(applyEntered)
			<-releaseApply
			victim.append("last-frame")
		})
	}()
	<-applyEntered
	activateDone := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
			return &fakeOutputResource{}, nil
		})
		activateDone <- err
	}()
	close(releaseApply)
	if err := <-applyDone; err != nil {
		t.Fatal(err)
	}
	if err := <-activateDone; err != nil {
		t.Fatal(err)
	}
	if victim.snapshotText != "last-frame" {
		t.Fatalf("snapshot=%q", victim.snapshotText)
	}
	if err := pool.WithLease(outputKey("A"), activation.Lease, nil); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("post-eviction frame err=%v", err)
	}
}

func TestOutputPoolRuntimeGenerationReplacementIsSingleMembership(t *testing.T) {
	pool, _ := NewOutputPool(1)
	old := &fakeOutputResource{}
	first, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return old, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newRevision := OutputRevision{RuntimeGeneration: 2}
	replacement := &fakeOutputResource{}
	second, err := pool.Activate(context.Background(), outputKey("A"), newRevision, func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return replacement, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Existing || second.Lease == first.Lease || second.Revision != newRevision {
		t.Fatalf("replacement=%+v first=%+v", second, first)
	}
	status := pool.Status()
	if status.Count != 1 || status.Connected != 1 || len(status.Desired) != 1 {
		t.Fatalf("status=%+v", status)
	}
	if old.closed.Load() != 1 || replacement.closed.Load() != 0 {
		t.Fatalf("old closed=%d replacement closed=%d", old.closed.Load(), replacement.closed.Load())
	}
	if err := pool.WithLease(outputKey("A"), first.Lease, nil); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("old generation lease err=%v", err)
	}
}

func TestOutputPoolRemoveDesiredClosesOnlyExactSession(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resources := map[string]*fakeOutputResource{}
	open := func(_ context.Context, key OutputKey, _ uint64) (OutputResource, error) {
		resource := &fakeOutputResource{}
		resources[key.SessionID] = resource
		return resource, nil
	}
	for _, name := range []string{"A", "B"} {
		if _, err := pool.Activate(context.Background(), outputKey(name), outputRevision(), open); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.RemoveDesired(outputKey("A")); err != nil {
		t.Fatal(err)
	}
	status := pool.Status()
	if status.Count != 1 || status.Connected != 1 || len(status.Desired) != 1 || status.Desired[0].SessionID != "B" {
		t.Fatalf("status=%+v", status)
	}
	if resources["A"].closed.Load() != 1 || resources["B"].closed.Load() != 0 {
		t.Fatalf("A closed=%d B closed=%d", resources["A"].closed.Load(), resources["B"].closed.Load())
	}
}

func TestOutputPoolVictimHostDisconnectRollsBackCrossHostHandoff(t *testing.T) {
	pool, _ := NewOutputPool(1)
	a := OutputKey{ClientKey: "host-a", InstanceID: "instance-a", SessionID: "A"}
	b := OutputKey{ClientKey: "host-b", InstanceID: "instance-b", SessionID: "B"}
	snapshotStarted := make(chan struct{})
	victim := &fakeOutputResource{snapshot: func(ctx context.Context) error {
		close(snapshotStarted)
		<-ctx.Done()
		return ctx.Err()
	}}
	if _, err := pool.Activate(context.Background(), a, outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return victim, nil
	}); err != nil {
		t.Fatal(err)
	}
	destination := &fakeOutputResource{}
	activateDone := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), b, outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
			return destination, nil
		})
		activateDone <- err
	}()
	<-snapshotStarted
	if err := pool.DisconnectHost("host-a", "instance-a"); err != nil {
		t.Fatal(err)
	}
	if err := <-activateDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("handoff err=%v", err)
	}
	status := pool.Status()
	if status.Count != 1 || status.Connected != 0 || status.Active == nil || *status.Active != a || len(status.Desired) != 1 || status.Desired[0] != a {
		t.Fatalf("rollback/disconnect status=%+v", status)
	}
	if destination.closed.Load() != 1 || victim.closed.Load() != 1 {
		t.Fatalf("destination closed=%d victim closed=%d", destination.closed.Load(), victim.closed.Load())
	}
}

func TestOutputPoolRestoreErrorCancelsDerivedContext(t *testing.T) {
	pool, _ := NewOutputPool(1)
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.DisconnectHost("host", "instance"); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan struct{})
	wantErr := errors.New("subscribe failed")
	_, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(ctx context.Context, _ OutputKey, _ uint64) (OutputResource, error) {
		go func() {
			<-ctx.Done()
			close(workerDone)
		}()
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("restore err=%v", err)
	}
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("restore error left derived context live")
	}
}

func TestOutputPoolCloseErrorIsStableAndAllResourcesClose(t *testing.T) {
	pool, _ := NewOutputPool(2)
	wantErr := errors.New("unsubscribe failed")
	resources := map[string]*fakeOutputResource{
		"A": {closeErr: wantErr},
		"B": {},
	}
	for _, name := range []string{"A", "B"} {
		if _, err := pool.Activate(context.Background(), outputKey(name), outputRevision(), func(_ context.Context, key OutputKey, _ uint64) (OutputResource, error) {
			return resources[key.SessionID], nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("first close err=%v", err)
	}
	if err := pool.Close(); !errors.Is(err, wantErr) {
		t.Fatalf("repeated close err=%v", err)
	}
	if resources["A"].closed.Load() != 1 || resources["B"].closed.Load() != 1 {
		t.Fatalf("A close=%d B close=%d", resources["A"].closed.Load(), resources["B"].closed.Load())
	}
}
