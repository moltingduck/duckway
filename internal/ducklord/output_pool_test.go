package ducklord

import (
	"context"
	"errors"
	"fmt"
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

type leaseBoundOutputResource struct {
	mu     sync.Mutex
	pool   *OutputPool
	key    OutputKey
	lease  uint64
	text   string
	closed atomic.Int32
}

func (r *leaseBoundOutputResource) BindOutputLease(pool *OutputPool, key OutputKey, lease uint64) error {
	r.mu.Lock()
	r.pool, r.key, r.lease = pool, key, lease
	r.mu.Unlock()
	return nil
}

func (r *leaseBoundOutputResource) append(text string) error {
	r.mu.Lock()
	pool, key, lease := r.pool, r.key, r.lease
	r.mu.Unlock()
	if pool == nil {
		return errors.New("resource is not lease-bound")
	}
	return pool.WithLease(key, lease, func(resource OutputResource) {
		if resource != r {
			return
		}
		r.mu.Lock()
		r.text += text
		r.mu.Unlock()
	})
}

func (r *leaseBoundOutputResource) Snapshot(context.Context) error { return nil }
func (r *leaseBoundOutputResource) Close() error                   { r.closed.Add(1); return nil }

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

func (r *fakeOutputResource) BindOutputLease(*OutputPool, OutputKey, uint64) error { return nil }
func (r *fakeOutputResource) ValidateOutputCommit() error                          { return nil }

func (r *statefulOutputResource) BindOutputLease(*OutputPool, OutputKey, uint64) error {
	return nil
}
func (r *statefulOutputResource) ValidateOutputCommit() error { return nil }

func (r *leaseBoundOutputResource) ValidateOutputCommit() error { return nil }

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
	open := func(_ context.Context, key OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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
		_, err := pool.Activate(context.Background(), outputKey("C"), outputRevision(), func(_ context.Context, _ OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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
	if _, err := pool.Activate(context.Background(), outputKey("D"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return victim, nil
	}); err != nil {
		t.Fatal(err)
	}
	destination := &fakeOutputResource{}
	if _, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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

func TestOutputPoolBindsBackgroundReaderBeforeCommitReturns(t *testing.T) {
	pool, _ := NewOutputPool(1)
	resource := &leaseBoundOutputResource{}
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return resource, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := resource.append("accepted"); err != nil {
		t.Fatal(err)
	}
	resource.mu.Lock()
	defer resource.mu.Unlock()
	if resource.pool != pool || resource.key != outputKey("A") || resource.lease != activation.Lease || resource.text != "accepted" {
		t.Fatalf("resource binding pool=%p key=%+v lease=%d text=%q activation=%+v", resource.pool, resource.key, resource.lease, resource.text, activation)
	}
}

func TestOutputPoolRejectsResourceAlreadyOwnedByAnotherEntry(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resource := &leaseBoundOutputResource{}
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return resource, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return resource, nil
	}); !errors.Is(err, ErrOutputResourceOwned) {
		t.Fatalf("duplicate resource error=%v", err)
	}
	if err := resource.append("still-owned-by-A"); err != nil {
		t.Fatal(err)
	}
	status := pool.Status()
	if status.Count != 1 || status.Active == nil || status.Active.SessionID != "A" {
		t.Fatalf("duplicate binding changed pool: %+v", status)
	}
}

func TestOutputPoolRejectsReplacementResourceAlreadyOwnedBySameEntry(t *testing.T) {
	pool, _ := NewOutputPool(1)
	resource := &leaseBoundOutputResource{}
	first, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return resource, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := pool.Status()
	if _, err := pool.ReplaceDesired(context.Background(), outputKey("A"), OutputRevision{RuntimeGeneration: 2}, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return resource, nil
	}); !errors.Is(err, ErrOutputResourceOwned) {
		t.Fatalf("same-entry alias error=%v", err)
	}
	if resource.closed.Load() != 0 {
		t.Fatalf("committed alias was closed %d times", resource.closed.Load())
	}
	if err := pool.WithLease(outputKey("A"), first.Lease, func(OutputResource) {}); err != nil {
		t.Fatalf("old lease was not preserved: %v", err)
	}
	after := pool.Status()
	if !reflect.DeepEqual(before, after) || after.Connected != 1 {
		t.Fatalf("replacement alias changed pool: before=%+v after=%+v", before, after)
	}
}

func TestOutputPoolDisconnectLeasePreservesDesiredAndFencesStaleReader(t *testing.T) {
	pool, _ := NewOutputPool(1)
	first := &fakeOutputResource{}
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return first, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pool.DisconnectLease(outputKey("A"), activation.Lease) {
		t.Fatal("current reader was not disconnected")
	}
	status := pool.Status()
	if status.Count != 1 || status.Connected != 0 || status.Active == nil || status.Active.SessionID != "A" || len(status.Desired) != 1 {
		t.Fatalf("disconnected status=%+v", status)
	}
	replacement := &fakeOutputResource{}
	_, err = pool.RestoreDesired(context.Background(), outputKey("A"), OutputRevision{RuntimeGeneration: 2}, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return replacement, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pool.DisconnectLease(outputKey("A"), activation.Lease) {
		t.Fatal("stale reader disconnected replacement")
	}
	if got := pool.Status(); got.Connected != 1 {
		t.Fatalf("replacement status=%+v", got)
	}
}

func TestOutputPoolCloseDuringBlockedOpen(t *testing.T) {
	pool, _ := NewOutputPool(1)
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(ctx context.Context, _ OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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

func TestOutputPoolCanceledOpenCannotCommitReturnedResource(t *testing.T) {
	pool, _ := NewOutputPool(1)
	ctx, cancel := context.WithCancel(context.Background())
	closeFailure := errors.New("close failed")
	resource := &fakeOutputResource{closeErr: closeFailure}
	_, err := pool.Activate(ctx, outputKey("A"), outputRevision(), func(openCtx context.Context, _ OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
		cancel()
		<-openCtx.Done()
		return resource, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("activation err=%v", err)
	}
	if !errors.Is(err, closeFailure) {
		t.Fatalf("activation hid cleanup failure: %v", err)
	}
	if resource.closed.Load() != 1 {
		t.Fatalf("canceled resource close count=%d", resource.closed.Load())
	}
	status := pool.Status()
	if status.Count != 0 || status.Active != nil || len(status.Desired) != 0 {
		t.Fatalf("canceled activation committed: %+v", status)
	}
}

func TestOutputPoolConcurrentExistingActivationDoesNotRace(t *testing.T) {
	pool, _ := NewOutputPool(2)
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
			got, activateErr := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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

func TestOutputPoolEnforcesPerDaemonObserverConnectionBudgetAcrossAliases(t *testing.T) {
	pool, err := NewOutputPool(20)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var first OutputKey
	for i := 0; i < maxStableOutputObserversPerDaemon+1; i++ {
		key := OutputKey{ClientKey: fmt.Sprintf("alias-%d", i%2), InstanceID: "instance", SessionID: fmt.Sprintf("S%05d", i)}
		if i == 0 {
			first = key
		}
		activation, activateErr := pool.Activate(context.Background(), key, outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
			return &fakeOutputResource{}, nil
		})
		if activateErr != nil {
			t.Fatal(activateErr)
		}
		if i == maxStableOutputObserversPerDaemon && (activation.Evicted == nil || *activation.Evicted != first) {
			t.Fatalf("per-host overflow evicted=%v want=%v", activation.Evicted, first)
		}
	}
	status := pool.Status()
	if status.Count != maxStableOutputObserversPerDaemon || status.Connected != maxStableOutputObserversPerDaemon {
		t.Fatalf("status=%+v", status)
	}
}

func TestOutputPoolWithLeaseSerializesTerminalMutation(t *testing.T) {
	pool, _ := NewOutputPool(1)
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	firstEntered := make(chan struct{})
	statusPassed := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	done := make(chan error, 2)
	go func() {
		done <- pool.WithLease(outputKey("A"), activation.Lease, func(OutputResource) {
			close(firstEntered)
			_ = pool.Status()
			close(statusPassed)
			<-releaseFirst
		})
	}()
	<-firstEntered
	select {
	case <-statusPassed:
	case <-time.After(time.Second):
		t.Fatal("terminal mutation deadlocked while re-entering pool status")
	}
	go func() {
		done <- pool.WithLease(outputKey("A"), activation.Lease, func(OutputResource) { close(secondEntered) })
	}()
	select {
	case <-secondEntered:
		t.Fatal("two terminal mutations overlapped")
	default:
	}
	close(releaseFirst)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-secondEntered:
	default:
		t.Fatal("second mutation never ran")
	}
}

func TestOutputPoolDisconnectRestorePreservesDesiredOrder(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resources := map[string]*fakeOutputResource{}
	open := func(_ context.Context, key OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
		_, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
			close(started)
			<-release // Deliberately model an opener slow to honor cancellation.
			return late, nil
		})
		done <- err
	}()
	<-started
	if _, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
	if _, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); err != nil {
		t.Fatalf("fresh restore: %v", err)
	}
}

func TestOutputPoolEvictionQuiescesAcceptedFramesBeforeSnapshot(t *testing.T) {
	pool, _ := NewOutputPool(1)
	victim := &statefulOutputResource{}
	activation, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
		_, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
	first, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return old, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	newRevision := OutputRevision{RuntimeGeneration: 2}
	replacement := &fakeOutputResource{}
	second, err := pool.Activate(context.Background(), outputKey("A"), newRevision, func(_ context.Context, _ OutputKey, gotRevision OutputRevision, _ uint64) (OutputResource, error) {
		if gotRevision != newRevision {
			t.Fatalf("opener revision=%+v want=%+v", gotRevision, newRevision)
		}
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

func TestOutputPoolBackgroundGenerationReplacementPreservesActiveLRU(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resources := map[string]*fakeOutputResource{}
	open := func(_ context.Context, key OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
		resource := &fakeOutputResource{}
		resources[key.SessionID] = resource
		return resource, nil
	}
	if _, err := pool.Activate(context.Background(), outputKey("B"), outputRevision(), open); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), open); err != nil {
		t.Fatal(err)
	}
	before := pool.Status()
	oldB := resources["B"]
	newRevision := OutputRevision{RuntimeGeneration: 2}
	if _, err := pool.ReplaceDesired(context.Background(), outputKey("B"), newRevision, open); err != nil {
		t.Fatal(err)
	}
	after := pool.Status()
	if !reflect.DeepEqual(after.Desired, before.Desired) || after.Active == nil || before.Active == nil || *after.Active != *before.Active {
		t.Fatalf("background replacement changed active/LRU: before=%+v after=%+v", before, after)
	}
	if oldB.closed.Load() != 1 || resources["B"] == oldB {
		t.Fatalf("old B close=%d replacement=%p old=%p", oldB.closed.Load(), resources["B"], oldB)
	}
}

func TestOutputPoolRemoveDesiredClosesOnlyExactSession(t *testing.T) {
	pool, _ := NewOutputPool(2)
	resources := map[string]*fakeOutputResource{}
	open := func(_ context.Context, key OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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

func TestOutputPoolRemoveHostDesiredForgetsOnlyReplacedInstance(t *testing.T) {
	pool, _ := NewOutputPool(3)
	defer pool.Close()
	oldA := OutputKey{ClientKey: "host", InstanceID: "old", SessionID: "A"}
	oldB := OutputKey{ClientKey: "host", InstanceID: "old", SessionID: "B"}
	current := OutputKey{ClientKey: "host", InstanceID: "new", SessionID: "C"}
	for _, key := range []OutputKey{oldA, oldB, current} {
		if _, err := pool.Activate(context.Background(), key, OutputRevision{RuntimeGeneration: 1}, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
			return &fakeOutputResource{}, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.ForgetHost("host", "old"); err != nil {
		t.Fatal(err)
	}
	if got := pool.DesiredForHost("host", "old"); len(got) != 0 {
		t.Fatalf("old instance remained desired: %#v", got)
	}
	if got := pool.DesiredForHost("host", "new"); len(got) != 1 || got[0] != current {
		t.Fatalf("current instance was disturbed: %#v", got)
	}
}

func TestOutputPoolForgetHostFencesBlockedActivation(t *testing.T) {
	pool, _ := NewOutputPool(1)
	defer pool.Close()
	key := OutputKey{ClientKey: "host", InstanceID: "retired", SessionID: "A"}
	entered, release := make(chan struct{}), make(chan struct{})
	activationDone := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), key, OutputRevision{RuntimeGeneration: 1}, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
			close(entered)
			<-release
			return &fakeOutputResource{}, nil
		})
		activationDone <- err
	}()
	<-entered
	forgetDone := make(chan error, 1)
	go func() { forgetDone <- pool.ForgetHost("host", "retired") }()
	for {
		pool.mu.Lock()
		retired := pool.retired[key.host()]
		pool.mu.Unlock()
		if retired {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	if err := <-activationDone; !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("retired activation error=%v", err)
	}
	if err := <-forgetDone; err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Activate(context.Background(), key, OutputRevision{RuntimeGeneration: 1}, func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); !errors.Is(err, ErrStaleOutputLease) {
		t.Fatalf("retired host accepted a new activation: %v", err)
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
	if _, err := pool.Activate(context.Background(), a, outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return victim, nil
	}); err != nil {
		t.Fatal(err)
	}
	destination := &fakeOutputResource{}
	activateDone := make(chan error, 1)
	go func() {
		_, err := pool.Activate(context.Background(), b, outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
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
	if _, err := pool.Activate(context.Background(), outputKey("A"), outputRevision(), func(context.Context, OutputKey, OutputRevision, uint64) (OutputResource, error) {
		return &fakeOutputResource{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.DisconnectHost("host", "instance"); err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan struct{})
	wantErr := errors.New("subscribe failed")
	_, err := pool.RestoreDesired(context.Background(), outputKey("A"), outputRevision(), func(ctx context.Context, _ OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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
		if _, err := pool.Activate(context.Background(), outputKey(name), outputRevision(), func(_ context.Context, key OutputKey, _ OutputRevision, _ uint64) (OutputResource, error) {
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
