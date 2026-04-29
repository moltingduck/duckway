package services

import (
	"log"
	"strconv"
	"time"

	"github.com/hackerduck/duckway/internal/database/queries"
)

// ApprovalSweeper periodically marks pending approvals as "ignored" once
// they've been pending longer than the configured request TTL. Distinct
// from "rejected" (admin actively said no).
type ApprovalSweeper struct {
	approvals *queries.ApprovalQueries
	settings  *queries.SettingsQueries
	stopCh    chan struct{}
}

func NewApprovalSweeper(a *queries.ApprovalQueries, s *queries.SettingsQueries) *ApprovalSweeper {
	return &ApprovalSweeper{approvals: a, settings: s, stopCh: make(chan struct{})}
}

func (s *ApprovalSweeper) Start() {
	go s.loop()
	log.Printf("Approval sweeper started (checking every minute)")
}

func (s *ApprovalSweeper) Stop() { close(s.stopCh) }

func (s *ApprovalSweeper) loop() {
	s.sweep() // run once at startup
	t := time.NewTicker(1 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-s.stopCh:
			return
		case <-t.C:
			s.sweep()
		}
	}
}

// ttl returns the configured timeout or 60 min default. Always positive.
func (s *ApprovalSweeper) ttl() int {
	raw := s.settings.Get(queries.SettingApprovalRequestTTL)
	if raw == "" {
		return 60
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 60
	}
	return n
}

func (s *ApprovalSweeper) sweep() {
	n, err := s.approvals.MarkExpiredAsIgnored(s.ttl())
	if err != nil {
		log.Printf("[approval-sweeper] error: %v", err)
		return
	}
	if n > 0 {
		log.Printf("[approval-sweeper] marked %d pending approval(s) as ignored (TTL=%d min)", n, s.ttl())
	}
}
