package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/database"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
)

type controlTestEnv struct {
	t       *testing.T
	handler *ClientUpdateHandler
	updates *queries.ClientUpdateQueries
	clients map[string]*models.Client
}

func newControlTestEnv(t *testing.T) *controlTestEnv {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clientQ := queries.NewClientQueries(db)
	clients := map[string]*models.Client{}
	for _, item := range []struct{ id, policy string }{{"client-a", "managed"}, {"client-b", "managed"}, {"client-c", "manual"}} {
		client := &models.Client{ID: item.id, ShortID: item.id, Name: item.id, TokenHash: "hash-" + item.id, CanaryEnabled: true, UpdatePolicy: item.policy}
		if err := clientQ.Create(client); err != nil {
			t.Fatal(err)
		}
		loaded, err := clientQ.GetByID(item.id)
		if err != nil {
			t.Fatal(err)
		}
		clients[item.id] = loaded
	}
	updates := queries.NewClientUpdateQueries(db)
	return &controlTestEnv{t: t, handler: NewClientUpdateHandler(updates, clientQ, t.TempDir()), updates: updates, clients: clients}
}

func (e *controlTestEnv) heartbeat(clientID string, body interface{}) (int, controlplane.HeartbeatResponse, string) {
	e.t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/client/control/heartbeat", bytes.NewReader(data))
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, e.clients[clientID]))
	rec := httptest.NewRecorder()
	e.handler.Heartbeat(rec, req)
	var response controlplane.HeartbeatResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	return rec.Code, response, rec.Body.String()
}

func validHeartbeat() controlplane.HeartbeatRequest {
	return controlplane.HeartbeatRequest{ProtocolVersion: controlplane.ProtocolVersion, Version: "v-old", OS: "linux", Arch: "amd64", BootID: "boot-1",
		InstallPath: "/home/test/.local/bin/duckway", InstallWritable: true, Capabilities: []string{controlplane.CapabilityManagedUpdate}, Components: map[string]string{"proxy": "running"}}
}

func TestClientUpdateHeartbeatLeaseIsolationAndCompletion(t *testing.T) {
	e := newControlTestEnv(t)
	if err := os.WriteFile(filepath.Join(e.handler.downloadDir, "ducklion-linux-amd64"), bytes.Repeat([]byte("d"), 2<<20), 0600); err != nil {
		t.Fatal(err)
	}
	for _, clientID := range []string{"client-a", "client-b", "client-c"} {
		if code, _, body := e.heartbeat(clientID, validHeartbeat()); code != http.StatusOK {
			t.Fatalf("initial heartbeat for %s: %d %s", clientID, code, body)
		}
	}
	artifactJSON := `[{"os":"linux","arch":"amd64","binary":"duckway-client-linux-amd64","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2000000}]`
	rollout, err := e.updates.CreateRollout("v-target", artifactJSON, 1, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	code, a, body := e.heartbeat("client-a", validHeartbeat())
	if code != 200 || a.Command == nil {
		t.Fatalf("first heartbeat code=%d response=%s", code, body)
	}
	first := a.Command
	if first.DucklionBinary != "ducklion-linux-amd64" || len(first.DucklionSHA256) != 64 || first.DucklionSize != 2<<20 {
		t.Fatalf("ducklion companion manifest = %+v", first)
	}
	code, a2, body := e.heartbeat("client-a", validHeartbeat())
	if code != 200 || a2.Command == nil || a2.Command.ID != first.ID || a2.Command.LeaseToken != first.LeaseToken {
		t.Fatalf("replayed lease code=%d response=%s", code, body)
	}
	_, b, _ := e.heartbeat("client-b", validHeartbeat())
	if b.Command != nil {
		t.Fatalf("concurrency limit leaked second command: %+v", b.Command)
	}
	_, manual, _ := e.heartbeat("client-c", validHeartbeat())
	if manual.Command != nil {
		t.Fatalf("manual client received command: %+v", manual.Command)
	}

	// Client B cannot report Client A's job even with the leaked job ID/token.
	status := controlplane.JobStatusRequest{LeaseToken: first.LeaseToken, Status: controlplane.JobRunning, RunningVersion: "v-old"}
	data, _ := json.Marshal(status)
	req := httptest.NewRequest(http.MethodPost, "/client/control/jobs/"+first.ID+"/status", bytes.NewReader(data))
	req.SetPathValue("id", first.ID)
	req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, e.clients["client-b"]))
	rec := httptest.NewRecorder()
	e.handler.ReportStatus(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-client status=%d body=%s", rec.Code, rec.Body.String())
	}

	hb := validHeartbeat()
	hb.CurrentJob = &controlplane.CurrentJob{ID: first.ID, LeaseToken: first.LeaseToken, Status: controlplane.JobRunning, RunningVersion: "v-old"}
	if code, _, body = e.heartbeat("client-a", hb); code != 200 {
		t.Fatalf("running heartbeat=%d %s", code, body)
	}
	hb.Version = "v-target"
	hb.BootID = "boot-after-restart"
	hb.CurrentJob.Status = controlplane.JobHealthy
	hb.CurrentJob.RunningVersion = "v-target"
	if code, result, body := e.heartbeat("client-a", hb); code != 200 || result.Command != nil {
		t.Fatalf("healthy heartbeat=%d response=%s", code, body)
	}
	_, next, body := e.heartbeat("client-b", validHeartbeat())
	if next.Command == nil {
		t.Fatalf("second client did not receive released slot: %s", body)
	}
	jobs, err := e.updates.ListJobs(rollout.ID)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, job := range jobs {
		statuses[job.ClientID] = job.Status
	}
	if statuses["client-a"] != "healthy" || statuses["client-c"] != "skipped_manual" {
		t.Fatalf("statuses=%v", statuses)
	}
}

func TestClientUpdateHeartbeatStrictValidation(t *testing.T) {
	e := newControlTestEnv(t)
	cases := []string{
		`{"protocol_version":1,"version":"v","os":"linux","arch":"amd64","boot_id":"b","install_path":"/x","install_writable":true,"capabilities":[],"components":{},"unknown":true}`,
		`{"protocol_version":1}{"protocol_version":1}`,
		`{"protocol_version":99,"version":"v","os":"linux","arch":"amd64","boot_id":"b","install_path":"/x","install_writable":true,"capabilities":[],"components":{}}`,
	}
	for _, body := range cases {
		req := httptest.NewRequest(http.MethodPost, "/client/control/heartbeat", bytes.NewBufferString(body))
		req = req.WithContext(context.WithValue(req.Context(), middleware.ClientKey, e.clients["client-a"]))
		rec := httptest.NewRecorder()
		e.handler.Heartbeat(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}
