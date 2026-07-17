package queries

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hackerduck/duckway/internal/models"
)

var (
	ErrNoUpdateJob       = errors.New("no update job available")
	ErrInvalidJobLease   = errors.New("invalid or expired job lease")
	ErrInvalidTransition = errors.New("invalid job status transition")
	ErrActiveRollout     = errors.New("an update rollout is already active")
)

type ClientUpdateQueries struct {
	db      *sql.DB
	leaseMu sync.Mutex
}

func NewClientUpdateQueries(db *sql.DB) *ClientUpdateQueries { return &ClientUpdateQueries{db: db} }

func (q *ClientUpdateQueries) UpsertRuntime(s *models.ClientRuntimeStatus) error {
	_, err := q.db.Exec(`INSERT INTO client_runtime_status
		(client_id,version,os,arch,boot_id,install_path,install_writable,capabilities,components,current_job_id,last_heartbeat_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,datetime('now'))
		ON CONFLICT(client_id) DO UPDATE SET version=excluded.version,os=excluded.os,arch=excluded.arch,
		boot_id=excluded.boot_id,install_path=excluded.install_path,install_writable=excluded.install_writable,
		capabilities=excluded.capabilities,components=excluded.components,current_job_id=excluded.current_job_id,
		last_heartbeat_at=datetime('now')`, s.ClientID, s.Version, s.OS, s.Arch, s.BootID, s.InstallPath,
		s.InstallWritable, s.Capabilities, s.Components, s.CurrentJobID)
	return err
}

func (q *ClientUpdateQueries) ListRuntime() (map[string]models.ClientRuntimeView, error) {
	rows, err := q.db.Query(`SELECT client_id,version,os,arch,boot_id,install_path,install_writable,
		capabilities,components,current_job_id,last_heartbeat_at FROM client_runtime_status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]models.ClientRuntimeView)
	for rows.Next() {
		var v models.ClientRuntimeView
		if err := rows.Scan(&v.ClientID, &v.Version, &v.OS, &v.Arch, &v.BootID, &v.InstallPath,
			&v.InstallWritable, &v.Capabilities, &v.Components, &v.CurrentJobID, &v.LastHeartbeatAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(v.Capabilities), &v.CapabilitiesList)
		_ = json.Unmarshal([]byte(v.Components), &v.ComponentsMap)
		if v.CapabilitiesList == nil {
			v.CapabilitiesList = []string{}
		}
		if v.ComponentsMap == nil {
			v.ComponentsMap = map[string]string{}
		}
		out[v.ClientID] = v
	}
	return out, rows.Err()
}

func (q *ClientUpdateQueries) CreateRollout(target, artifacts string, maxConcurrency, interval, failureThreshold int) (*models.ClientUpdateRollout, error) {
	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM client_update_rollouts WHERE status IN ('active','paused')`).Scan(&active); err != nil {
		return nil, err
	}
	if active != 0 {
		return nil, ErrActiveRollout
	}
	id := uuid.New().String()
	_, err = tx.Exec(`INSERT INTO client_update_rollouts
		(id,target_version,artifacts,status,max_concurrency,start_interval_seconds,failure_threshold_percent)
		VALUES (?,?,?,'active',?,?,?)`, id, target, artifacts, maxConcurrency, interval, failureThreshold)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrActiveRollout
		}
		return nil, err
	}
	_, err = tx.Exec(`INSERT INTO client_update_jobs (id,rollout_id,client_id,status,error,finished_at)
		SELECT lower(hex(randomblob(16))), ?, c.id,
		CASE
			WHEN c.is_active=0 THEN 'skipped_inactive'
			WHEN c.update_policy='manual' THEN 'skipped_manual'
			WHEN s.client_id IS NULL OR s.capabilities NOT LIKE '%"managed_update_v1"%' THEN 'ineligible'
			WHEN s.install_writable=0 THEN 'manual_required'
			WHEN s.version=? THEN 'up_to_date'
			ELSE 'queued'
		END,
		CASE
			WHEN c.is_active=0 THEN 'client is inactive'
			WHEN c.update_policy='manual' THEN 'manual updates only'
			WHEN s.client_id IS NULL OR s.capabilities NOT LIKE '%"managed_update_v1"%' THEN 'managed update capability not reported'
			WHEN s.install_writable=0 THEN 'install path is not writable'
			ELSE ''
		END,
		CASE WHEN c.is_active=1 AND c.update_policy='managed' AND s.client_id IS NOT NULL
			AND s.capabilities LIKE '%"managed_update_v1"%' AND s.install_writable=1 AND s.version<>?
			THEN NULL ELSE datetime('now') END
		FROM clients c LEFT JOIN client_runtime_status s ON s.client_id=c.id`, id, target, target)
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`UPDATE client_update_rollouts SET status='completed',updated_at=datetime('now')
		WHERE id=? AND NOT EXISTS (SELECT 1 FROM client_update_jobs WHERE rollout_id=? AND status NOT IN
		('healthy','failed','skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled'))`, id, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return q.GetRollout(id)
}

func scanRollout(row interface{ Scan(...interface{}) error }, r *models.ClientUpdateRollout) error {
	return row.Scan(&r.ID, &r.TargetVersion, &r.Artifacts, &r.Status, &r.MaxConcurrency, &r.StartIntervalSeconds,
		&r.FailureThreshold, &r.NextDispatchAt, &r.CreatedAt, &r.UpdatedAt)
}

const rolloutCols = `id,target_version,artifacts,status,max_concurrency,start_interval_seconds,
	failure_threshold_percent,next_dispatch_at,created_at,updated_at`

func (q *ClientUpdateQueries) GetRollout(id string) (*models.ClientUpdateRollout, error) {
	var r models.ClientUpdateRollout
	err := scanRollout(q.db.QueryRow(`SELECT `+rolloutCols+` FROM client_update_rollouts WHERE id=?`, id), &r)
	return &r, err
}

func (q *ClientUpdateQueries) ListRollouts() ([]models.ClientUpdateRolloutSummary, error) {
	rows, err := q.db.Query(`SELECT r.` + strings.ReplaceAll(rolloutCols, ",", ",r.") + `,
		COUNT(j.id), COALESCE(SUM(CASE WHEN j.status='queued' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN j.status IN ('leased','running') THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN j.status='healthy' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN j.status='failed' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN j.status LIKE 'skipped_%' OR j.status IN ('ineligible','manual_required','up_to_date') THEN 1 ELSE 0 END),0)
		FROM client_update_rollouts r LEFT JOIN client_update_jobs j ON j.rollout_id=r.id
		GROUP BY r.id ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ClientUpdateRolloutSummary{}
	for rows.Next() {
		var s models.ClientUpdateRolloutSummary
		if err := rows.Scan(&s.ID, &s.TargetVersion, &s.Artifacts, &s.Status, &s.MaxConcurrency, &s.StartIntervalSeconds,
			&s.FailureThreshold, &s.NextDispatchAt, &s.CreatedAt, &s.UpdatedAt, &s.Total, &s.Queued,
			&s.Running, &s.Healthy, &s.Failed, &s.Skipped); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanJob(row interface{ Scan(...interface{}) error }, j *models.ClientUpdateJob) error {
	var lease sql.NullString
	err := row.Scan(&j.ID, &j.RolloutID, &j.ClientID, &j.ClientName, &j.TargetVersion, &j.Artifacts, &j.Type, &j.Status,
		&j.LeaseToken, &lease, &j.Attempts, &j.Error, &j.StartedAt, &j.FinishedAt, &j.CreatedAt, &j.UpdatedAt)
	if lease.Valid {
		j.LeaseExpiresAt = lease.String
	}
	return err
}

const jobSelect = `SELECT j.id,j.rollout_id,j.client_id,c.name,r.target_version,r.artifacts,j.type,j.status,j.lease_token,
	j.lease_expires_at,j.attempts,j.error,COALESCE(j.started_at,''),COALESCE(j.finished_at,''),j.created_at,j.updated_at
	FROM client_update_jobs j JOIN clients c ON c.id=j.client_id JOIN client_update_rollouts r ON r.id=j.rollout_id`

func (q *ClientUpdateQueries) ListJobs(rolloutID string) ([]models.ClientUpdateJob, error) {
	rows, err := q.db.Query(jobSelect+` WHERE j.rollout_id=? ORDER BY c.name`, rolloutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ClientUpdateJob{}
	for rows.Next() {
		var j models.ClientUpdateJob
		if err := scanJob(rows, &j); err != nil {
			return nil, err
		}
		j.LeaseToken = ""
		out = append(out, j)
	}
	return out, rows.Err()
}

// LeaseJob atomically enforces rollout concurrency and dispatch spacing. A valid
// existing lease is returned again so a lost heartbeat response is harmless.
func (q *ClientUpdateQueries) LeaseJob(clientID, currentVersion, leaseToken string, leaseFor time.Duration) (*models.ClientUpdateJob, error) {
	q.leaseMu.Lock()
	defer q.leaseMu.Unlock()
	tx, err := q.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	_, _ = tx.Exec(`UPDATE client_update_jobs SET status=CASE WHEN attempts>=3 THEN 'failed' ELSE 'queued' END,
		lease_token='',lease_expires_at=NULL,error=CASE WHEN attempts>=3 THEN 'lease expired' ELSE error END,updated_at=datetime('now')
		WHERE status IN ('leased','running') AND lease_expires_at < datetime('now')`)
	_, _ = tx.Exec(`UPDATE client_update_rollouts SET status='paused',updated_at=datetime('now')
		WHERE status='active' AND EXISTS (SELECT 1 FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id AND status='failed')
		AND ((SELECT COUNT(*) FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id AND status='failed')*100 /
		MAX(1,(SELECT COUNT(*) FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id
		AND status NOT IN ('skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled')))) >= failure_threshold_percent`)
	var existing models.ClientUpdateJob
	err = scanJob(tx.QueryRow(jobSelect+` WHERE j.client_id=? AND r.status='active'
		AND j.status IN ('leased','running')
		AND j.lease_expires_at >= datetime('now') ORDER BY j.created_at LIMIT 1`, clientID), &existing)
	if err == nil {
		_ = tx.Commit()
		return &existing, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	_, _ = tx.Exec(`UPDATE client_update_jobs SET status='healthy',finished_at=datetime('now'),updated_at=datetime('now')
		WHERE client_id=? AND status='queued' AND rollout_id IN
		(SELECT id FROM client_update_rollouts WHERE status='active' AND target_version=?)`, clientID, currentVersion)
	_, _ = tx.Exec(`UPDATE client_update_rollouts SET status=CASE WHEN EXISTS
		(SELECT 1 FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id AND status='failed')
		THEN 'completed_with_failures' ELSE 'completed' END,updated_at=datetime('now') WHERE status='active'
		AND NOT EXISTS (SELECT 1 FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id
			AND status NOT IN ('healthy','failed','skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled'))`)
	var id string
	err = tx.QueryRow(`SELECT j.id FROM client_update_jobs j JOIN client_update_rollouts r ON r.id=j.rollout_id
		JOIN clients c ON c.id=j.client_id JOIN client_runtime_status s ON s.client_id=j.client_id
		WHERE j.client_id=? AND j.status='queued' AND r.status='active' AND r.next_dispatch_at<=datetime('now')
		AND c.is_active=1 AND c.update_policy='managed' AND s.install_writable=1
		AND s.capabilities LIKE '%"managed_update_v1"%'
		AND (SELECT COUNT(*) FROM client_update_jobs x WHERE x.rollout_id=r.id
		AND x.status IN ('leased','running')) < r.max_concurrency
		ORDER BY r.created_at,j.created_at LIMIT 1`, clientID).Scan(&id)
	if err == sql.ErrNoRows {
		_ = tx.Commit()
		return nil, ErrNoUpdateJob
	}
	if err != nil {
		return nil, err
	}
	leaseMod := fmt.Sprintf("+%d seconds", int(leaseFor.Seconds()))
	res, err := tx.Exec(`UPDATE client_update_jobs SET status='leased',lease_token=?,
		lease_expires_at=datetime('now',?),attempts=attempts+1,started_at=COALESCE(started_at,datetime('now')),
		updated_at=datetime('now') WHERE id=? AND status='queued'`, leaseToken, leaseMod, id)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNoUpdateJob
	}
	_, err = tx.Exec(`UPDATE client_update_rollouts SET next_dispatch_at=datetime('now','+'||start_interval_seconds||' seconds'),updated_at=datetime('now')
		WHERE id=(SELECT rollout_id FROM client_update_jobs WHERE id=?)`, id)
	if err != nil {
		return nil, err
	}
	var job models.ClientUpdateJob
	if err := scanJob(tx.QueryRow(jobSelect+` WHERE j.id=?`, id), &job); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &job, nil
}

func allowedTransition(from, to string) bool {
	if from == to && (to == "running" || to == "healthy" || to == "failed") {
		return true
	}
	if to == "failed" {
		return from != "healthy" && !strings.HasPrefix(from, "skipped_")
	}
	allowed := map[string]string{"leased": "running", "running": "healthy"}
	return allowed[from] == to
}

func (q *ClientUpdateQueries) UpdateJobStatus(clientID, jobID, leaseToken, status, runningVersion, errorText string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current, target, rolloutID string
	err = tx.QueryRow(`SELECT j.status,r.target_version,j.rollout_id FROM client_update_jobs j
		JOIN client_update_rollouts r ON r.id=j.rollout_id WHERE j.id=? AND j.client_id=? AND j.lease_token=?
		AND j.lease_expires_at>=datetime('now')`, jobID, clientID, leaseToken).Scan(&current, &target, &rolloutID)
	if err == sql.ErrNoRows {
		return ErrInvalidJobLease
	}
	if err != nil {
		return err
	}
	if !allowedTransition(current, status) {
		return ErrInvalidTransition
	}
	if status == "healthy" && runningVersion != target {
		return fmt.Errorf("healthy version %q does not match target %q", runningVersion, target)
	}
	finished := "NULL"
	if status == "healthy" || status == "failed" {
		finished = "datetime('now')"
	}
	_, err = tx.Exec(`UPDATE client_update_jobs SET status=?,error=?,finished_at=`+finished+`,
		lease_expires_at=CASE WHEN ? IN ('healthy','failed') THEN lease_expires_at ELSE datetime('now','+10 minutes') END,
		updated_at=datetime('now') WHERE id=?`, status, errorText, status, jobID)
	if err != nil {
		return err
	}
	var total, healthy, failed, terminal, eligible int
	err = tx.QueryRow(`SELECT COUNT(*),SUM(status='healthy'),SUM(status='failed'),
		SUM(status IN ('healthy','failed','skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled')),
		SUM(status NOT IN ('skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled'))
		FROM client_update_jobs WHERE rollout_id=?`, rolloutID).
		Scan(&total, &healthy, &failed, &terminal, &eligible)
	if err != nil {
		return err
	}
	if terminal == total {
		_, err = tx.Exec(`UPDATE client_update_rollouts SET status=CASE WHEN ?>0 THEN 'completed_with_failures' ELSE 'completed' END,updated_at=datetime('now') WHERE id=? AND status IN ('active','paused')`, failed, rolloutID)
	} else if failed > 0 {
		_, err = tx.Exec(`UPDATE client_update_rollouts SET status='paused',updated_at=datetime('now') WHERE id=? AND status='active'
			AND (?*100/MAX(1,?))>=failure_threshold_percent`, rolloutID, failed, eligible)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (q *ClientUpdateQueries) SetRolloutStatus(id, action string) error {
	var from []string
	var to string
	switch action {
	case "pause":
		from = []string{"active"}
		to = "paused"
	case "resume":
		from = []string{"paused"}
		to = "active"
	case "cancel":
		from = []string{"active", "paused"}
		to = "cancelled"
	default:
		return ErrInvalidTransition
	}
	marks := strings.Repeat("?,", len(from))
	marks = strings.TrimSuffix(marks, ",")
	args := []interface{}{to, id}
	for _, v := range from {
		args = append(args, v)
	}
	res, err := q.db.Exec(`UPDATE client_update_rollouts SET status=?,updated_at=datetime('now') WHERE id=? AND status IN (`+marks+`)`, args...)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrInvalidTransition
	}
	if to == "cancelled" {
		_, err = q.db.Exec(`UPDATE client_update_jobs SET status='cancelled',updated_at=datetime('now') WHERE rollout_id=? AND status='queued'`, id)
	}
	return err
}

func (q *ClientUpdateQueries) RetryFailed(id string) (int64, error) {
	tx, err := q.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var active int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM client_update_rollouts WHERE id<>? AND status IN ('active','paused')`, id).Scan(&active); err != nil {
		return 0, err
	}
	if active > 0 {
		return 0, ErrActiveRollout
	}
	res, err := tx.Exec(`UPDATE client_update_jobs SET status='queued',lease_token='',lease_expires_at=NULL,
		attempts=0,error='',finished_at=NULL,updated_at=datetime('now') WHERE rollout_id=? AND status='failed'`, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return 0, ErrInvalidTransition
	}
	result, err := tx.Exec(`UPDATE client_update_rollouts SET status='active',next_dispatch_at=datetime('now'),updated_at=datetime('now')
		WHERE id=? AND status IN ('paused','completed_with_failures')`, id)
	if err != nil {
		return 0, err
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return 0, ErrInvalidTransition
	}
	return n, tx.Commit()
}

func (q *ClientUpdateQueries) SkipQueuedForManual(clientID string) error {
	tx, err := q.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE client_update_jobs SET status='skipped_manual',finished_at=datetime('now'),updated_at=datetime('now') WHERE client_id=? AND status='queued'`, clientID); err != nil {
		return err
	}
	if _, err = tx.Exec(`UPDATE client_update_rollouts SET status=CASE WHEN EXISTS
		(SELECT 1 FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id AND status='failed')
		THEN 'completed_with_failures' ELSE 'completed' END,updated_at=datetime('now') WHERE status='active'
		AND NOT EXISTS (SELECT 1 FROM client_update_jobs WHERE rollout_id=client_update_rollouts.id
			AND status NOT IN ('healthy','failed','skipped_manual','skipped_inactive','ineligible','manual_required','up_to_date','cancelled'))`); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *ClientUpdateQueries) HasActiveRollout() bool {
	var n int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM client_update_rollouts WHERE status='active'`).Scan(&n)
	return n > 0
}

func (q *ClientUpdateQueries) JobBelongsToClient(jobID, clientID string) bool {
	if jobID == "" {
		return true
	}
	var n int
	_ = q.db.QueryRow(`SELECT COUNT(*) FROM client_update_jobs WHERE id=? AND client_id=?`, jobID, clientID).Scan(&n)
	return n == 1
}
