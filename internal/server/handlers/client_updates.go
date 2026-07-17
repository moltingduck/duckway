package handlers

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/controlplane"
	"github.com/hackerduck/duckway/internal/database/queries"
	"github.com/hackerduck/duckway/internal/models"
	"github.com/hackerduck/duckway/internal/server/middleware"
	"github.com/hackerduck/duckway/internal/version"
)

const controlBodyLimit = 64 << 10

type ClientUpdateHandler struct {
	updates     *queries.ClientUpdateQueries
	clients     *queries.ClientQueries
	downloadDir string
}

func NewClientUpdateHandler(updates *queries.ClientUpdateQueries, clients *queries.ClientQueries, downloadDir string) *ClientUpdateHandler {
	return &ClientUpdateHandler{updates: updates, clients: clients, downloadDir: downloadDir}
}

func strictJSON(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	r.Body = http.MaxBytesReader(w, r.Body, controlBodyLimit)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func validControlPlatform(osName, arch string) bool {
	return (osName == "linux" || osName == "darwin") && (arch == "amd64" || arch == "arm64")
}

func hasCapability(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (h *ClientUpdateHandler) Heartbeat(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}
	var req controlplane.HeartbeatRequest
	if err := strictJSON(w, r, &req); err != nil {
		jsonError(w, "invalid heartbeat", http.StatusBadRequest)
		return
	}
	if req.ProtocolVersion != controlplane.ProtocolVersion || strings.TrimSpace(req.BootID) == "" ||
		strings.TrimSpace(req.Version) == "" || !validControlPlatform(req.OS, req.Arch) ||
		len(req.Capabilities) > 32 || len(req.InstallPath) > 4096 {
		jsonError(w, "invalid heartbeat fields", http.StatusBadRequest)
		return
	}
	currentJobID := ""
	if req.CurrentJob != nil {
		currentJobID = req.CurrentJob.ID
		if !h.updates.JobBelongsToClient(currentJobID, client.ID) {
			jsonError(w, "current job not found", http.StatusConflict)
			return
		}
		if req.CurrentJob.Status == controlplane.JobRunning || req.CurrentJob.Status == controlplane.JobHealthy || req.CurrentJob.Status == controlplane.JobFailed {
			if err := h.updates.UpdateJobStatus(client.ID, currentJobID, req.CurrentJob.LeaseToken,
				req.CurrentJob.Status, req.CurrentJob.RunningVersion, req.CurrentJob.Error); err != nil &&
				!errors.Is(err, queries.ErrInvalidTransition) {
				if errors.Is(err, queries.ErrInvalidJobLease) {
					jsonError(w, "current job lease is invalid", http.StatusConflict)
				} else {
					jsonError(w, "update current job failed", http.StatusInternalServerError)
				}
				return
			}
		}
	}
	capabilities, _ := json.Marshal(req.Capabilities)
	components, _ := json.Marshal(req.Components)
	runtimeStatus := &models.ClientRuntimeStatus{
		ClientID: client.ID, Version: req.Version, OS: req.OS, Arch: req.Arch, BootID: req.BootID,
		InstallPath: req.InstallPath, InstallWritable: req.InstallWritable, Capabilities: string(capabilities),
		Components: string(components), CurrentJobID: currentJobID,
	}
	if err := h.updates.UpsertRuntime(runtimeStatus); err != nil {
		jsonError(w, "store heartbeat failed", http.StatusInternalServerError)
		return
	}

	response := controlplane.HeartbeatResponse{
		NextHeartbeatSeconds: 300,
		ServerTime:           time.Now().UTC().Format(time.RFC3339),
	}
	if h.updates.HasActiveRollout() {
		response.NextHeartbeatSeconds = 60
	}
	if client.UpdatePolicy == "manual" {
		_ = h.updates.SkipQueuedForManual(client.ID)
		jsonResponse(w, response)
		return
	}
	if !req.InstallWritable || !hasCapability(req.Capabilities, controlplane.CapabilityManagedUpdate) {
		jsonResponse(w, response)
		return
	}

	lease := make([]byte, 32)
	if _, err := rand.Read(lease); err != nil {
		jsonError(w, "create lease failed", http.StatusInternalServerError)
		return
	}
	job, err := h.updates.LeaseJob(client.ID, req.Version, hex.EncodeToString(lease), 10*time.Minute)
	if errors.Is(err, queries.ErrNoUpdateJob) {
		jsonResponse(w, response)
		return
	}
	if err != nil {
		jsonError(w, "lease update job failed", http.StatusInternalServerError)
		return
	}
	var artifacts []controlplane.Artifact
	_ = json.Unmarshal([]byte(job.Artifacts), &artifacts)
	for _, artifact := range artifacts {
		if artifact.OS == req.OS && artifact.Arch == req.Arch {
			response.Command = &controlplane.Command{
				ID: job.ID, Type: job.Type, TargetVersion: job.TargetVersion, Binary: artifact.Binary,
				SHA256: artifact.SHA256, Size: artifact.Size, LeaseToken: job.LeaseToken,
				LeaseExpiresAt: job.LeaseExpiresAt, Attempt: job.Attempts,
			}
			break
		}
	}
	if response.Command == nil {
		_ = h.updates.UpdateJobStatus(client.ID, job.ID, job.LeaseToken, "failed", "", "artifact_missing: no binary for client platform")
	} else {
		response.NextHeartbeatSeconds = 30
	}
	jsonResponse(w, response)
}

func (h *ClientUpdateHandler) ReportStatus(w http.ResponseWriter, r *http.Request) {
	client := middleware.GetClient(r)
	if client == nil {
		jsonError(w, "client auth required", http.StatusUnauthorized)
		return
	}
	var req controlplane.JobStatusRequest
	if err := strictJSON(w, r, &req); err != nil {
		jsonError(w, "invalid status report", http.StatusBadRequest)
		return
	}
	allowed := map[string]bool{controlplane.JobRunning: true, controlplane.JobHealthy: true, controlplane.JobFailed: true}
	if !allowed[req.Status] || req.LeaseToken == "" || len(req.Error) > 4096 {
		jsonError(w, "invalid status report", http.StatusBadRequest)
		return
	}
	err := h.updates.UpdateJobStatus(client.ID, r.PathValue("id"), req.LeaseToken, req.Status, req.RunningVersion, req.Error)
	if errors.Is(err, queries.ErrInvalidJobLease) {
		jsonError(w, "job not found", http.StatusNotFound)
		return
	}
	if errors.Is(err, queries.ErrInvalidTransition) || err != nil && strings.Contains(err.Error(), "does not match target") {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		jsonError(w, "update job status failed", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}

func recommendedClientVersion() string {
	if value := os.Getenv("DUCKWAY_CLIENT_RECOMMENDED_VERSION"); value != "" {
		return value
	}
	return version.Get()
}

func (h *ClientUpdateHandler) availableArtifacts() ([]controlplane.Artifact, error) {
	artifacts := []controlplane.Artifact{}
	for _, osName := range []string{"linux", "darwin"} {
		for _, arch := range []string{"amd64", "arm64"} {
			binary := "duckway-client-" + osName + "-" + arch
			file, err := os.Open(filepath.Join(h.downloadDir, binary))
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			hash := sha256.New()
			size, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return nil, copyErr
			}
			if closeErr != nil {
				return nil, closeErr
			}
			artifacts = append(artifacts, controlplane.Artifact{
				OS: osName, Arch: arch, Binary: binary, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size,
			})
		}
	}
	if len(artifacts) == 0 {
		return nil, errors.New("no client binaries available")
	}
	return artifacts, nil
}

func (h *ClientUpdateHandler) List(w http.ResponseWriter, r *http.Request) {
	clients, err := h.clients.List()
	if err != nil {
		jsonError(w, "list clients failed", http.StatusInternalServerError)
		return
	}
	if clients == nil {
		clients = []models.Client{}
	}
	runtimes, err := h.updates.ListRuntime()
	if err != nil {
		jsonError(w, "list runtime failed", http.StatusInternalServerError)
		return
	}
	rollouts, err := h.updates.ListRollouts()
	if err != nil {
		jsonError(w, "list rollouts failed", http.StatusInternalServerError)
		return
	}
	type rolloutRow struct {
		models.ClientUpdateRolloutSummary
		Jobs []models.ClientUpdateJob `json:"jobs"`
	}
	rolloutRows := make([]rolloutRow, 0, len(rollouts))
	for _, rollout := range rollouts {
		jobs, jobsErr := h.updates.ListJobs(rollout.ID)
		if jobsErr != nil {
			jsonError(w, "list rollout jobs failed", http.StatusInternalServerError)
			return
		}
		rollout.Artifacts = ""
		rolloutRows = append(rolloutRows, rolloutRow{ClientUpdateRolloutSummary: rollout, Jobs: jobs})
	}
	jsonResponse(w, map[string]interface{}{
		"recommended_version": recommendedClientVersion(), "clients": clients, "runtimes": runtimes, "rollouts": rolloutRows,
	})
}

func (h *ClientUpdateHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TargetVersion        string `json:"target_version"`
		MaxConcurrency       int    `json:"max_concurrency"`
		StartIntervalSeconds int    `json:"start_interval_seconds"`
		FailureThreshold     int    `json:"failure_threshold_percent"`
	}
	if err := strictJSON(w, r, &req); err != nil {
		jsonError(w, "invalid rollout", http.StatusBadRequest)
		return
	}
	if req.TargetVersion != recommendedClientVersion() {
		jsonError(w, "target_version must match the available client version", http.StatusBadRequest)
		return
	}
	if req.MaxConcurrency < 1 || req.MaxConcurrency > 100 || req.StartIntervalSeconds < 0 ||
		req.StartIntervalSeconds > 3600 || req.FailureThreshold < 1 || req.FailureThreshold > 100 {
		jsonError(w, "invalid rollout limits", http.StatusBadRequest)
		return
	}
	artifacts, err := h.availableArtifacts()
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	artifactJSON, _ := json.Marshal(artifacts)
	rollout, err := h.updates.CreateRollout(req.TargetVersion, string(artifactJSON), req.MaxConcurrency, req.StartIntervalSeconds, req.FailureThreshold)
	if errors.Is(err, queries.ErrActiveRollout) {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		jsonError(w, "create rollout failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(rollout)
}

func (h *ClientUpdateHandler) Get(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rollout, err := h.updates.GetRollout(id)
	if err != nil {
		jsonError(w, "rollout not found", http.StatusNotFound)
		return
	}
	jobs, err := h.updates.ListJobs(id)
	if err != nil {
		jsonError(w, "list rollout jobs failed", http.StatusInternalServerError)
		return
	}
	rollout.Artifacts = ""
	jsonResponse(w, map[string]interface{}{"rollout": rollout, "jobs": jobs})
}

func (h *ClientUpdateHandler) Action(w http.ResponseWriter, r *http.Request) {
	action := r.PathValue("action")
	if action == "retry-failed" {
		count, err := h.updates.RetryFailed(r.PathValue("id"))
		if err != nil {
			if errors.Is(err, queries.ErrActiveRollout) || errors.Is(err, queries.ErrInvalidTransition) {
				jsonError(w, err.Error(), http.StatusConflict)
			} else {
				jsonError(w, "retry failed", http.StatusInternalServerError)
			}
			return
		}
		jsonResponse(w, map[string]interface{}{"status": "ok", "retried": count})
		return
	}
	if err := h.updates.SetRolloutStatus(r.PathValue("id"), action); err != nil {
		if errors.Is(err, queries.ErrInvalidTransition) {
			jsonError(w, "invalid rollout transition", http.StatusConflict)
		} else {
			jsonError(w, "update rollout failed", http.StatusInternalServerError)
		}
		return
	}
	jsonResponse(w, map[string]string{"status": "ok"})
}
