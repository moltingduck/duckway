package runtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

var (
	ErrRuntimeNotRecovering = errors.New("runtime is not recoverable")
	ErrRuntimeRegistered    = errors.New("runtime is already registered")
	ErrChallengeInvalid     = errors.New("registration challenge is invalid")
	ErrRecoveryProof        = errors.New("recovery proof is invalid")
	ErrRegistryClosed       = errors.New("runtime registry is closed")
)

type SessionLookup interface {
	GetSession(context.Context, model.SessionID) (model.Session, error)
}

type RuntimeConnection interface {
	Close() error
}

type RuntimeIdentity struct {
	SessionID  model.SessionID
	Generation uint64
	LeaseID    string
}

type RegistrationChallenge struct {
	ID    string
	Nonce []byte
}

type pendingChallenge struct {
	sessionID  model.SessionID
	nonce      []byte
	generation uint64
	expires    time.Time
}

type registeredRuntime struct {
	identity RuntimeIdentity
	conn     RuntimeConnection
}

type Registry struct {
	mu         sync.Mutex
	instanceID model.InstanceID
	sessions   SessionLookup
	now        func() time.Time
	challenges map[string]pendingChallenge
	live       map[model.SessionID]registeredRuntime
	closed     bool
}

func NewRegistry(instanceID model.InstanceID, sessions SessionLookup) *Registry {
	return &Registry{instanceID: instanceID, sessions: sessions, now: time.Now,
		challenges: make(map[string]pendingChallenge), live: make(map[model.SessionID]registeredRuntime)}
}

func (r *Registry) BeginRegistration(ctx context.Context, sessionID model.SessionID, generation uint64) (RegistrationChallenge, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return RegistrationChallenge{}, ErrRegistryClosed
	}
	r.mu.Unlock()
	session, err := r.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return RegistrationChallenge{}, err
	}
	if session.Status != model.StatusRecovering || session.RuntimeGeneration != generation || len(session.RecoveryPublicKey) != ed25519.PublicKeySize {
		return RegistrationChallenge{}, ErrRuntimeNotRecovering
	}
	nonce, err := model.NewRecoveryNonce()
	if err != nil {
		return RegistrationChallenge{}, err
	}
	id, err := randomID()
	if err != nil {
		return RegistrationChallenge{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RegistrationChallenge{}, ErrRegistryClosed
	}
	if _, exists := r.live[sessionID]; exists {
		return RegistrationChallenge{}, ErrRuntimeRegistered
	}
	for challengeID, challenge := range r.challenges {
		if r.now().After(challenge.expires) {
			delete(r.challenges, challengeID)
		}
	}
	r.challenges[id] = pendingChallenge{sessionID: sessionID, nonce: nonce, generation: generation, expires: r.now().Add(15 * time.Second)}
	return RegistrationChallenge{ID: id, Nonce: append([]byte(nil), nonce...)}, nil
}

func (r *Registry) CompleteRegistration(ctx context.Context, sessionID model.SessionID, generation uint64, challengeID string, proof []byte, protocolMajor, protocolMinor uint16, conn RuntimeConnection) (RuntimeIdentity, error) {
	if conn == nil {
		return RuntimeIdentity{}, ErrChallengeInvalid
	}
	succeeded := false
	defer func() {
		if !succeeded {
			_ = conn.Close()
		}
	}()
	r.mu.Lock()
	challenge, exists := r.challenges[challengeID]
	if exists {
		delete(r.challenges, challengeID) // every challenge is single-use, including failures
	}
	if !exists || challenge.sessionID != sessionID || challenge.generation != generation || r.now().After(challenge.expires) {
		r.mu.Unlock()
		return RuntimeIdentity{}, ErrChallengeInvalid
	}
	if _, exists := r.live[sessionID]; exists {
		r.mu.Unlock()
		return RuntimeIdentity{}, ErrRuntimeRegistered
	}
	r.mu.Unlock()

	session, err := r.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return RuntimeIdentity{}, err
	}
	if session.Status != model.StatusRecovering || session.RuntimeGeneration != generation {
		return RuntimeIdentity{}, ErrRuntimeNotRecovering
	}
	if !model.VerifyRecoveryProof(ed25519.PublicKey(session.RecoveryPublicKey), r.instanceID, sessionID, generation, challenge.nonce, proof, protocolMajor, protocolMinor) {
		return RuntimeIdentity{}, ErrRecoveryProof
	}
	leaseID, err := randomID()
	if err != nil {
		return RuntimeIdentity{}, err
	}
	identity := RuntimeIdentity{SessionID: sessionID, Generation: generation, LeaseID: leaseID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return RuntimeIdentity{}, ErrRegistryClosed
	}
	if _, exists := r.live[sessionID]; exists {
		return RuntimeIdentity{}, ErrRuntimeRegistered
	}
	r.live[sessionID] = registeredRuntime{identity: identity, conn: conn}
	succeeded = true
	return identity, nil
}

func (r *Registry) Disconnect(identity RuntimeIdentity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if current, ok := r.live[identity.SessionID]; ok && current.identity == identity {
		delete(r.live, identity.SessionID)
	}
}

func (r *Registry) IsCurrent(identity RuntimeIdentity) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.live[identity.SessionID]
	return ok && current.identity == identity
}

func (r *Registry) Current(sessionID model.SessionID, generation uint64) (RuntimeIdentity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.live[sessionID]
	if !ok || current.identity.Generation != generation {
		return RuntimeIdentity{}, false
	}
	return current.identity, true
}

func (r *Registry) CloseAll() {
	r.mu.Lock()
	r.closed = true
	live := r.live
	r.live = make(map[model.SessionID]registeredRuntime)
	r.challenges = make(map[string]pendingChallenge)
	r.mu.Unlock()
	for _, current := range live {
		_ = current.conn.Close()
	}
}

func randomID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}
