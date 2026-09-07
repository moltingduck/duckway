package client

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const ccProcessedMessageLimit = 2000

type processedMessageRecord struct {
	MessageID   string    `json:"message_id"`
	Handle      string    `json:"handle"`
	SeenAt      time.Time `json:"seen_at"`
	Outcome     string    `json:"outcome,omitempty"`
	Content     string    `json:"content,omitempty"`
	ReplyTo     string    `json:"reply_to,omitempty"`
	DeliveryKey string    `json:"delivery_key,omitempty"`
}

type ccPromptRejection struct {
	Content     string
	ReplyTo     string
	DeliveryKey string
}

type CCProcessedStore struct {
	path    string
	mu      sync.Mutex
	data    map[string]processedMessageRecord
	loadErr error
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
		s.loadErr = fmt.Errorf("processed-message state is corrupt: %w", err)
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
	if s.loadErr != nil {
		return true
	}
	_, ok := s.data[messageID]
	return ok
}

func (s *CCProcessedStore) Mark(messageID, handle string) error {
	if messageID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return s.loadErr
	}
	s.data[messageID] = processedMessageRecord{MessageID: messageID, Handle: handle, SeenAt: time.Now().UTC()}
	s.pruneLocked()
	return s.flushLocked()
}

// RecordPromptRejection persists the authoritative business outcome before
// attempting its Discord delivery. An inbox retry must replay this receipt,
// even if session ownership has changed in the meantime.
func (s *CCProcessedStore) RecordPromptRejection(messageID, handle, content, replyTo, deliveryKey string) error {
	if messageID == "" || handle == "" || content == "" || deliveryKey == "" {
		return os.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return s.loadErr
	}
	s.data[messageID] = processedMessageRecord{MessageID: messageID, Handle: handle, SeenAt: time.Now().UTC(), Outcome: "rejected", Content: content, ReplyTo: replyTo, DeliveryKey: deliveryKey}
	s.pruneLocked()
	return s.flushLocked()
}

func (s *CCProcessedStore) PromptRejection(messageID, handle string) (ccPromptRejection, bool, error) {
	if messageID == "" {
		return ccPromptRejection{}, false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return ccPromptRejection{}, false, s.loadErr
	}
	record, ok := s.data[messageID]
	if !ok || record.Handle != handle || record.Outcome != "rejected" || record.Content == "" || record.DeliveryKey == "" {
		return ccPromptRejection{}, false, nil
	}
	return ccPromptRejection{Content: record.Content, ReplyTo: record.ReplyTo, DeliveryKey: record.DeliveryKey}, true, nil
}

// MarkIfNew atomically claims a Discord message for processing. It prevents
// SSE and inbox polling paths from enqueueing the same message concurrently.
func (s *CCProcessedStore) MarkIfNew(messageID, handle string) (bool, error) {
	if messageID == "" {
		return true, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return false, s.loadErr
	}
	if _, ok := s.data[messageID]; ok {
		return false, nil
	}
	s.data[messageID] = processedMessageRecord{MessageID: messageID, Handle: handle, SeenAt: time.Now().UTC()}
	s.pruneLocked()
	return true, s.flushLocked()
}

func (s *CCProcessedStore) pruneLocked() {
	records := make([]processedMessageRecord, 0, len(s.data))
	for _, r := range s.data {
		if r.Outcome != "rejected" {
			records = append(records, r)
		}
	}
	if len(records) <= ccProcessedMessageLimit {
		return
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
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".cc-processed-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
