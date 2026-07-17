package queries

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/models"
)

func TestClientUpdateLeaseRespectsConcurrencyAndIsIdempotent(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, id := range []string{"a", "b"} {
		if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash, update_policy) VALUES (?, ?, ?, 'managed')`, id, id, "hash-"+id); err != nil {
			t.Fatal(err)
		}
	}
	q := NewClientUpdateQueries(db)
	capabilities, _ := json.Marshal([]string{controlplane.CapabilityManagedUpdate})
	for _, id := range []string{"a", "b"} {
		if err := q.UpsertRuntime(&models.ClientRuntimeStatus{ClientID: id, Version: "old", OS: "linux", Arch: "amd64", InstallWritable: true, Capabilities: string(capabilities), Components: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	artifacts, _ := json.Marshal([]controlplane.Artifact{{OS: "linux", Arch: "amd64", Binary: "duckway-client-linux-amd64", SHA256: string(make([]byte, 64)), Size: 2 << 20}})
	rollout, err := q.CreateRollout("new", string(artifacts), 1, 0, 50)
	if err != nil {
		t.Fatal(err)
	}

	first, err := q.LeaseJob("a", "old", "lease-a", 10*time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first lease = %+v, %v", first, err)
	}
	repeated, err := q.LeaseJob("a", "old", "different-token", 10*time.Minute)
	if err != nil || repeated == nil || repeated.ID != first.ID || repeated.LeaseToken != first.LeaseToken || repeated.Attempts != 1 {
		t.Fatalf("repeated lease = %+v, %v", repeated, err)
	}
	blocked, err := q.LeaseJob("b", "old", "lease-b", 10*time.Minute)
	if !errors.Is(err, ErrNoUpdateJob) || blocked != nil {
		t.Fatalf("concurrency should block b, got %+v, %v", blocked, err)
	}
	if err := q.UpdateJobStatus("a", first.ID, "lease-a", controlplane.JobRunning, "old", ""); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateJobStatus("a", first.ID, "lease-a", controlplane.JobHealthy, "wrong", ""); err == nil {
		t.Fatal("healthy with wrong version was accepted")
	}
	if err := q.UpdateJobStatus("a", first.ID, "lease-a", controlplane.JobHealthy, "new", ""); err != nil {
		t.Fatal(err)
	}
	if err := q.UpdateJobStatus("a", first.ID, "lease-a", controlplane.JobHealthy, "new", ""); err != nil {
		t.Fatalf("repeated healthy status should be idempotent: %v", err)
	}
	second, err := q.LeaseJob("b", "old", "lease-b", 10*time.Minute)
	if err != nil || second == nil || second.ClientID != "b" {
		t.Fatalf("second lease = %+v, %v", second, err)
	}
	if rollout.ID == "" {
		t.Fatal("rollout ID is empty")
	}
}

func TestClientManualPolicySkipsQueuedRolloutJob(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`INSERT INTO clients (id, name, token_hash, update_policy) VALUES ('a','a','hash-a','managed')`); err != nil {
		t.Fatal(err)
	}
	clients := NewClientQueries(db)
	q := NewClientUpdateQueries(db)
	if err := q.UpsertRuntime(&models.ClientRuntimeStatus{ClientID: "a", Version: "old", OS: "linux", Arch: "amd64", InstallWritable: true, Capabilities: `["managed_update_v1"]`, Components: `{}`}); err != nil {
		t.Fatal(err)
	}
	rollout, err := q.CreateRollout("v", `[]`, 1, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	client, _ := clients.GetByID("a")
	client.UpdatePolicy = "manual"
	if err := clients.Update(client); err != nil {
		t.Fatal(err)
	}
	jobs, err := q.ListJobs(rollout.ID)
	if err != nil || len(jobs) != 1 || jobs[0].Status != "skipped_manual" {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
}

func TestClientUpdateConcurrentLeaseHonorsLimit(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := NewClientUpdateQueries(db)
	const clients = 12
	for i := 0; i < clients; i++ {
		id := fmt.Sprintf("client-%02d", i)
		if _, err := db.Exec(`INSERT INTO clients (id,name,token_hash,update_policy) VALUES (?,?,?,'managed')`, id, id, "hash-"+id); err != nil {
			t.Fatal(err)
		}
		if err := q.UpsertRuntime(&models.ClientRuntimeStatus{ClientID: id, Version: "old", OS: "linux", Arch: "amd64", InstallWritable: true, Capabilities: `["managed_update_v1"]`, Components: `{}`}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.CreateRollout("new", `[]`, 3, 0, 50); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan *models.ClientUpdateJob, clients)
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("client-%02d", i)
			job, err := q.LeaseJob(id, "old", "lease-"+id, 10*time.Minute)
			if err != nil && !errors.Is(err, ErrNoUpdateJob) {
				errs <- err
			}
			if job != nil {
				results <- job
			}
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	leased := 0
	for range results {
		leased++
	}
	if leased != 3 {
		t.Fatalf("leased=%d want 3", leased)
	}
}
