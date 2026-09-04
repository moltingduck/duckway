package runtime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

type fakeSessions struct{ session model.Session }

func (f *fakeSessions) GetSession(context.Context, model.SessionID) (model.Session, error) {
	return f.session, nil
}

type fakeConnection struct{}

func (*fakeConnection) Close() error { return nil }

type trackedConnection struct{ closed bool }

func (c *trackedConnection) Close() error { c.closed = true; return nil }

type blockingSessions struct {
	session model.Session
	block   chan struct{}
	entered chan struct{}
	calls   int
	mu      sync.Mutex
}

func (s *blockingSessions) GetSession(context.Context, model.SessionID) (model.Session, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call > 1 {
		close(s.entered)
		<-s.block
	}
	return s.session, nil
}

func recoveryFixture(t *testing.T) (*Registry, model.InstanceID, model.Session, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	instance := model.NewInstanceID()
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", Status: model.StatusRecovering,
		Writer: &model.Owner{Kind: model.OwnerCC, ID: "channel"}, OwnershipEpoch: 1, RuntimeGeneration: 2, TaskState: model.TaskIdle,
		AdapterState: model.AdapterRecovering, RecoveryPublicKey: publicKey}
	return NewRegistry(instance, &fakeSessions{session: session}), instance, session, privateKey
}

func TestRegistrationProofIsSingleUseAndRuntimeBound(t *testing.T) {
	registry, instance, session, privateKey := recoveryFixture(t)
	challenge, err := registry.BeginRegistration(context.Background(), session.ID, session.RuntimeGeneration)
	if err != nil {
		t.Fatal(err)
	}
	proof := model.RecoveryProof(privateKey, instance, session.ID, session.RuntimeGeneration, challenge.Nonce, 1, 0)
	identity, err := registry.CompleteRegistration(context.Background(), session.ID, session.RuntimeGeneration, challenge.ID, proof, 1, 0, &fakeConnection{})
	if err != nil || !registry.IsCurrent(identity) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	if _, err := registry.CompleteRegistration(context.Background(), session.ID, session.RuntimeGeneration, challenge.ID, proof, 1, 0, &fakeConnection{}); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("replay error=%v", err)
	}
	registry.Disconnect(RuntimeIdentity{SessionID: session.ID, Generation: session.RuntimeGeneration, LeaseID: "stale"})
	if !registry.IsCurrent(identity) {
		t.Fatal("stale disconnect removed live runtime")
	}
	registry.Disconnect(identity)
	if registry.IsCurrent(identity) {
		t.Fatal("live runtime remains after disconnect")
	}
}

func TestConcurrentRegistrationHasSingleWinner(t *testing.T) {
	registry, instance, session, privateKey := recoveryFixture(t)
	type candidate struct {
		challenge RegistrationChallenge
		proof     []byte
	}
	candidates := make([]candidate, 2)
	for i := range candidates {
		challenge, err := registry.BeginRegistration(context.Background(), session.ID, session.RuntimeGeneration)
		if err != nil {
			t.Fatal(err)
		}
		candidates[i] = candidate{challenge: challenge, proof: model.RecoveryProof(privateKey, instance, session.ID, session.RuntimeGeneration, challenge.Nonce, 1, 0)}
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, candidate := range candidates {
		candidate := candidate
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := registry.CompleteRegistration(context.Background(), session.ID, session.RuntimeGeneration, candidate.challenge.ID, candidate.proof, 1, 0, &fakeConnection{})
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("registration successes=%d, want 1", successes)
	}
}

func TestCloseAllFencesRegistrationInProgress(t *testing.T) {
	publicKey, privateKey, err := model.NewRecoveryKey()
	if err != nil {
		t.Fatal(err)
	}
	instance := model.NewInstanceID()
	session := model.Session{ID: "ABC123", Handle: "agent", Kind: model.KindAgent, AgentType: "codex", Status: model.StatusRecovering,
		Writer: &model.Owner{Kind: model.OwnerCC, ID: "channel"}, OwnershipEpoch: 1, RuntimeGeneration: 2, TaskState: model.TaskIdle,
		AdapterState: model.AdapterRecovering, RecoveryPublicKey: publicKey}
	sessions := &blockingSessions{session: session, block: make(chan struct{}), entered: make(chan struct{})}
	registry := NewRegistry(instance, sessions)
	challenge, err := registry.BeginRegistration(context.Background(), session.ID, session.RuntimeGeneration)
	if err != nil {
		t.Fatal(err)
	}
	connection := &trackedConnection{}
	result := make(chan error, 1)
	go func() {
		proof := model.RecoveryProof(privateKey, instance, session.ID, session.RuntimeGeneration, challenge.Nonce, 1, 0)
		_, err := registry.CompleteRegistration(context.Background(), session.ID, session.RuntimeGeneration, challenge.ID, proof, 1, 0, connection)
		result <- err
	}()
	<-sessions.entered
	registry.CloseAll()
	close(sessions.block)
	if err := <-result; !errors.Is(err, ErrRegistryClosed) {
		t.Fatalf("registration error=%v", err)
	}
	if !connection.closed {
		t.Fatal("losing connection was not closed")
	}
}
