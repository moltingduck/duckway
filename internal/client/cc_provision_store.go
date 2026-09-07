package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

type ccProvisionPhase string

const (
	ccProvisionReserved       ccProvisionPhase = "reserved"
	ccProvisionChannelCreated ccProvisionPhase = "channel_created"
	ccProvisionSessionCreated ccProvisionPhase = "session_created"
	ccProvisionMarkerSet      ccProvisionPhase = "marker_set"
	ccProvisionActive         ccProvisionPhase = "active"
	ccProvisionReplyDelivered ccProvisionPhase = "reply_delivered"
	ccProvisionFailed         ccProvisionPhase = "failed"
)

var errCCProvisionConflict = errors.New("discord command was replayed with different provisioning arguments")

type ccProvisionRecord struct {
	RequestID        string                   `json:"request_id"`
	ManagementHandle string                   `json:"management_handle"`
	CCID             string                   `json:"cc_id"`
	Slug             string                   `json:"slug"`
	Topic            string                   `json:"topic,omitempty"`
	CWD              string                   `json:"cwd"`
	Phase            ccProvisionPhase         `json:"phase"`
	Channel          *CreateCCChannelResult   `json:"channel,omitempty"`
	Session          *protocol.SessionSummary `json:"session,omitempty"`
	LastError        string                   `json:"last_error,omitempty"`
	UpdatedAt        time.Time                `json:"updated_at"`
}

func (r ccProvisionRecord) sameRequest(other ccProvisionRecord) bool {
	return r.ManagementHandle == other.ManagementHandle && r.CCID == other.CCID && r.Slug == other.Slug && r.Topic == other.Topic && r.CWD == other.CWD
}

type ccProvisionStore struct {
	path string
	mu   sync.Mutex
	data map[string]ccProvisionRecord
}

func newCCProvisionStore(configDir string) *ccProvisionStore {
	s := &ccProvisionStore{path: filepath.Join(configDir, "cc-provisioning.json"), data: make(map[string]ccProvisionRecord)}
	s.load()
	return s
}

func (s *ccProvisionStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var records []ccProvisionRecord
	if json.Unmarshal(raw, &records) != nil {
		return
	}
	for _, record := range records {
		if record.RequestID != "" {
			s.data[record.RequestID] = record
		}
	}
}

func (s *ccProvisionStore) Reserve(record ccProvisionRecord) (ccProvisionRecord, error) {
	if record.RequestID == "" {
		return ccProvisionRecord{}, fmt.Errorf("provision request id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.data[record.RequestID]; ok {
		if !existing.sameRequest(record) {
			return ccProvisionRecord{}, errCCProvisionConflict
		}
		return existing, nil
	}
	record.Phase = ccProvisionReserved
	record.UpdatedAt = time.Now().UTC()
	s.data[record.RequestID] = record
	return record, s.flushLocked()
}

func (s *ccProvisionStore) Save(record ccProvisionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[record.RequestID]; !ok {
		return fmt.Errorf("provision workflow %q is not reserved", record.RequestID)
	}
	record.UpdatedAt = time.Now().UTC()
	s.data[record.RequestID] = record
	return s.flushLocked()
}

func (s *ccProvisionStore) Pending() []ccProvisionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ccProvisionRecord, 0)
	for _, record := range s.data {
		if record.Phase != ccProvisionReplyDelivered && record.Phase != ccProvisionFailed {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.Before(out[j].UpdatedAt) })
	return out
}

func (s *ccProvisionStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	records := make([]ccProvisionRecord, 0, len(s.data))
	for _, record := range s.data {
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].RequestID < records[j].RequestID })
	body, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return writeFileAtomic(s.path, body, 0600)
}
