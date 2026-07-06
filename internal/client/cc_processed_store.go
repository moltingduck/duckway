package client

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const ccProcessedMessageLimit = 2000

type processedMessageRecord struct {
	MessageID string    `json:"message_id"`
	Handle    string    `json:"handle"`
	SeenAt    time.Time `json:"seen_at"`
}

type CCProcessedStore struct {
	path string
	mu   sync.Mutex
	data map[string]processedMessageRecord
}

func NewCCProcessedStore(configDir string) *CCProcessedStore {
	s := &CCProcessedStore{
		path: filepath.Join(configDir, "cc-processed-messages.json"),
		data: map[string]processedMessageRecord{},
	}
	s.load()
	return s
}

func (s *CCProcessedStore) load() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var records []processedMessageRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return
	}
	for _, r := range records {
		if r.MessageID != "" {
			s.data[r.MessageID] = r
		}
	}
}

func (s *CCProcessedStore) Seen(messageID string) bool {
	if messageID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.data[messageID]
	return ok
}

func (s *CCProcessedStore) Mark(messageID, handle string) error {
	if messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[messageID] = processedMessageRecord{MessageID: messageID, Handle: handle, SeenAt: time.Now().UTC()}
	s.pruneLocked()
	return s.flushLocked()
}

// MarkIfNew atomically claims a Discord message for processing. It prevents
// SSE and inbox polling paths from enqueueing the same message concurrently.
func (s *CCProcessedStore) MarkIfNew(messageID, handle string) (bool, error) {
	if messageID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.data[messageID]; ok {
		return false, nil
	}
	s.data[messageID] = processedMessageRecord{MessageID: messageID, Handle: handle, SeenAt: time.Now().UTC()}
	s.pruneLocked()
	return true, s.flushLocked()
}

func (s *CCProcessedStore) pruneLocked() {
	if len(s.data) <= ccProcessedMessageLimit {
		return
	}
	records := make([]processedMessageRecord, 0, len(s.data))
	for _, r := range s.data {
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].SeenAt.Before(records[j].SeenAt)
	})
	for _, r := range records[:len(records)-ccProcessedMessageLimit] {
		delete(s.data, r.MessageID)
	}
}

func (s *CCProcessedStore) flushLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	records := make([]processedMessageRecord, 0, len(s.data))
	for _, r := range s.data {
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].SeenAt.Before(records[j].SeenAt)
	})
	body, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(s.path, body, 0600)
}
