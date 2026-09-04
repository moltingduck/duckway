package model

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

type InstanceID string
type SessionID string

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

func NewInstanceID() InstanceID { return InstanceID(uuid.NewString()) }

func ParseInstanceID(v string) (InstanceID, error) {
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("invalid instance id: %w", err)
	}
	return InstanceID(id.String()), nil
}

func NewSessionID() (SessionID, error) {
	b := make([]byte, 6)
	for i := range b {
		for {
			var one [1]byte
			if _, err := rand.Read(one[:]); err != nil {
				return "", fmt.Errorf("generate session id: %w", err)
			}
			if one[0] < 224 { // largest multiple of 32 below 256
				b[i] = crockford[int(one[0])%len(crockford)]
				break
			}
		}
	}
	return SessionID(b), nil
}

func ParseSessionID(v string) (SessionID, error) {
	v = strings.ToUpper(strings.TrimSpace(v))
	if len(v) != 6 {
		return "", fmt.Errorf("session id must contain six Crockford Base32 characters")
	}
	for _, r := range v {
		if !strings.ContainsRune(crockford, r) {
			return "", fmt.Errorf("invalid session id %q", v)
		}
	}
	return SessionID(v), nil
}

func ValidateHandle(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", fmt.Errorf("session handle is required")
	}
	n := 0
	for _, r := range v {
		n++
		if r == 0 || r == '\n' || r == '\r' || unicode.IsControl(r) {
			return "", fmt.Errorf("session handle contains a control character")
		}
	}
	if n > 128 {
		return "", fmt.Errorf("session handle exceeds 128 Unicode code points")
	}
	return v, nil
}

type SessionKind string

const (
	KindAgent SessionKind = "agent"
	KindShell SessionKind = "shell"
)

type SessionStatus string

const (
	StatusProvisioning SessionStatus = "provisioning"
	StatusRunning      SessionStatus = "running"
	StatusStopped      SessionStatus = "stopped"
	StatusRecovering   SessionStatus = "recovering"
	StatusDestroying   SessionStatus = "destroying"
)

type OwnerKind string

const (
	OwnerCC       OwnerKind = "cc"
	OwnerTerminal OwnerKind = "terminal"
)

type Owner struct {
	Kind OwnerKind `json:"kind"`
	ID   string    `json:"id"`
}

func (o Owner) Validate() error {
	if o.Kind != OwnerCC && o.Kind != OwnerTerminal {
		return fmt.Errorf("invalid owner kind %q", o.Kind)
	}
	if strings.TrimSpace(o.ID) == "" || strings.ContainsAny(o.ID, "\x00\r\n") {
		return fmt.Errorf("invalid owner id")
	}
	return nil
}

type TaskState string

const (
	TaskIdle     TaskState = "idle"
	TaskRunning  TaskState = "running"
	TaskReplying TaskState = "replying"
)

type AdapterState string

const (
	AdapterUnavailable AdapterState = "unavailable"
	AdapterHealthy     AdapterState = "healthy"
	AdapterUnhealthy   AdapterState = "unhealthy"
	AdapterRecovering  AdapterState = "recovering"
)

type Session struct {
	ID                SessionID
	Handle            string
	Kind              SessionKind
	AgentType         string
	CWD               string
	Shell             string
	Status            SessionStatus
	Writer            *Owner
	OwnershipEpoch    uint64
	RuntimeGeneration uint64
	TaskState         TaskState
	AdapterState      AdapterState
	RecoveryPublicKey []byte
	CreatedAtMS       int64
	UpdatedAtMS       int64
	ExitSuccess       *bool
	ExitReason        string
}

func (s Session) Validate() error {
	if _, err := ParseSessionID(string(s.ID)); err != nil {
		return err
	}
	if _, err := ValidateHandle(s.Handle); err != nil {
		return err
	}
	if s.Kind != KindAgent && s.Kind != KindShell {
		return fmt.Errorf("invalid session kind %q", s.Kind)
	}
	if s.Status != StatusProvisioning && s.Status != StatusRunning && s.Status != StatusStopped && s.Status != StatusRecovering && s.Status != StatusDestroying {
		return fmt.Errorf("invalid session status %q", s.Status)
	}
	if s.TaskState != TaskIdle && s.TaskState != TaskRunning && s.TaskState != TaskReplying {
		return fmt.Errorf("invalid task state %q", s.TaskState)
	}
	if s.AdapterState != AdapterUnavailable && s.AdapterState != AdapterHealthy && s.AdapterState != AdapterUnhealthy && s.AdapterState != AdapterRecovering {
		return fmt.Errorf("invalid adapter state %q", s.AdapterState)
	}
	if s.RuntimeGeneration == 0 {
		return fmt.Errorf("runtime generation must be positive")
	}
	if len(s.RecoveryPublicKey) != 0 && len(s.RecoveryPublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid recovery public key")
	}
	if s.Kind == KindAgent {
		if strings.TrimSpace(s.AgentType) == "" || s.Writer == nil {
			return fmt.Errorf("agent session requires agent type and writer")
		}
		if err := s.Writer.Validate(); err != nil {
			return err
		}
	} else if s.Writer != nil || s.AgentType != "" || s.AdapterState != AdapterUnavailable || s.TaskState != TaskIdle {
		return fmt.Errorf("shell session cannot have writer, agent, adapter, or task state")
	}
	return nil
}

type PendingYield struct {
	SessionID   SessionID
	Requester   Owner
	SourceEpoch uint64
	RequestID   string
	CreatedAtMS int64
}
