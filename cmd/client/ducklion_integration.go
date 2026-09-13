package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/client"
	"github.com/hackerduck/duckway/internal/ducklion/daemon"
	"github.com/hackerduck/duckway/internal/ducklion/management"
	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
)

func runIntegrateCommand(configDir string, args []string, out io.Writer) error {
	if len(args) != 1 || args[0] != "ducklion" {
		return fmt.Errorf("usage: duckway integrate ducklion")
	}
	if _, err := client.LoadConfig(configDir); err != nil {
		return fmt.Errorf("configure Duckway before integration: %w", err)
	}
	updateLock, err := acquireUpdateLock(configDir)
	if err != nil {
		return err
	}
	defer releaseUpdateLock(updateLock)
	root := filepath.Join(configDir, "ducklion")
	lock, err := management.Acquire(root)
	if err != nil {
		return err
	}
	defer lock.Close()
	record, err := management.Read(root)
	if err != nil {
		return err
	}
	if record.Mode != management.Standalone {
		return fmt.Errorf("ducklion is not standalone-managed")
	}
	if err := validateStandaloneBinary(record.Binary); err != nil {
		return err
	}
	if _, alive := management.ReadPID(management.StandalonePID(root)); !alive {
		return fmt.Errorf("standalone Ducklion daemon is not running")
	}
	conn, err := daemon.Dial(filepath.Join(root, "ducklion.sock"), "duckway-integration")
	if err != nil {
		return err
	}
	before, err := conn.ListSessions()
	if err != nil {
		_ = conn.Close()
		return err
	}
	beforeID, err := ducklionInstanceID(conn)
	if err != nil {
		_ = conn.Close()
		return err
	}
	response, err := callDucklionManagement(root, conn, "integrate-quiesce", "management.quiesce")
	if err != nil {
		_ = conn.Close()
		return err
	}
	if response.Error != nil {
		_ = conn.Close()
		return fmt.Errorf("integration refused: %s", response.Error.Message)
	}
	// The daemon rejects new admissions from this point until its socket closes.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := management.Stop(ctx, root, management.StandalonePID(root)); err != nil {
		_, _ = callDucklionManagement(root, conn, "integrate-resume", "management.resume")
		_ = conn.Close()
		return err
	}
	_ = conn.Close()

	transition := record
	transition.Mode, transition.ManagedBy = management.Transitioning, "duckway"
	if err := management.Write(root, transition); err != nil {
		return restoreStandalone(root, record, fmt.Errorf("mark integration transition: %w", err))
	}
	exe, err := os.Executable()
	if err != nil {
		return restoreStandalone(root, record, err)
	}
	startErr := management.Start(root, exe, []string{"__ducklion_daemon"}, []string{"DUCKWAY_INTEGRATE_DUCKLION=1"}, management.IntegratedPID(root), filepath.Join(root, "daemon.log"))
	if startErr != nil {
		return restoreStandalone(root, record, fmt.Errorf("start Duckway-managed Ducklion: %w", startErr))
	}
	verifyErr := verifyIntegratedInventory(root, beforeID, before)
	if verifyErr != nil {
		return restoreStandalone(root, record, fmt.Errorf("verify Ducklion handoff: %w", verifyErr))
	}
	transition.Mode = management.Integrated
	if err := management.Write(root, transition); err != nil {
		return restoreStandalone(root, record, fmt.Errorf("commit Ducklion manager: %w", err))
	}
	fmt.Fprintln(out, "Ducklion integrated with Duckway; existing sessions and PTY supervisors were preserved")
	return nil
}

func validateStandaloneBinary(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("standalone Ducklion binary path is missing or not absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("standalone Ducklion binary is not an executable regular file")
	}
	return nil
}

func ducklionInstanceID(conn *daemon.Client) (string, error) {
	response, err := conn.Call(protocol.Request{ID: "integration-status", Type: "status"})
	if err != nil {
		return "", err
	}
	if response.Error != nil {
		return "", fmt.Errorf("ducklion status: %s", response.Error.Message)
	}
	var status struct {
		InstanceID string `json:"instance_id"`
	}
	if err := json.Unmarshal(response.Result, &status); err != nil {
		return "", err
	}
	if status.InstanceID == "" {
		return "", fmt.Errorf("ducklion instance ID is missing")
	}
	return status.InstanceID, nil
}

type handoffSession struct {
	ID, Channel, Management string
	Generation, Epoch       uint64
	Writer                  *model.Owner
}

func handoffInventory(sessions []protocol.SessionSummary) []handoffSession {
	result := make([]handoffSession, 0, len(sessions))
	for _, session := range sessions {
		result = append(result, handoffSession{ID: session.SessionID, Channel: session.ChannelHandle,
			Management: session.ManagementHandle, Generation: session.RuntimeGeneration,
			Epoch: session.OwnershipEpoch, Writer: session.Writer})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func verifyIntegratedInventory(root, instanceID string, before []protocol.SessionSummary) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := daemon.Dial(filepath.Join(root, "ducklion.sock"), "duckway-integration")
		if err == nil {
			currentID, idErr := ducklionInstanceID(conn)
			after, listErr := conn.ListSessions()
			runtimeResponse, runtimeErr := callDucklionManagement(root, conn, "integration-runtimes", "management.runtime_status")
			_ = conn.Close()
			if idErr == nil && listErr == nil && runtimeErr == nil && runtimeResponse.Error == nil && currentID == instanceID && reflect.DeepEqual(handoffInventory(before), handoffInventory(after)) {
				var active map[string]uint64
				if err := json.Unmarshal(runtimeResponse.Result, &active); err == nil {
					ready := true
					for _, session := range before {
						if session.Status == model.StatusRunning && active[session.SessionID] != session.RuntimeGeneration {
							ready = false
						}
					}
					if ready {
						return nil
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("session inventory or instance identity changed during handoff")
}

func restoreStandalone(root string, record management.Record, cause error) error {
	if _, alive := management.ReadPID(management.IntegratedPID(root)); alive {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		stopErr := management.Stop(ctx, root, management.IntegratedPID(root))
		cancel()
		if stopErr != nil {
			return fmt.Errorf("%w; could not stop new daemon: %v", cause, stopErr)
		}
	}
	if err := management.Write(root, record); err != nil {
		return fmt.Errorf("%w; could not restore standalone marker: %v", cause, err)
	}
	if err := management.Start(root, record.Binary, []string{"daemon"}, nil, management.StandalonePID(root), filepath.Join(root, "standalone.log")); err != nil {
		return fmt.Errorf("%w; could not restart standalone daemon: %v", cause, err)
	}
	return cause
}

func waitForDucklionIdle(configDir string) error {
	root := filepath.Join(configDir, "ducklion")
	if _, alive := management.ReadPID(management.IntegratedPID(root)); !alive {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		conn, err := daemon.Dial(filepath.Join(root, "ducklion.sock"), "duckway-integration")
		if err != nil {
			return err
		}
		response, callErr := callDucklionManagement(root, conn, "restart-quiesce", "management.quiesce")
		_ = conn.Close()
		if callErr != nil {
			return callErr
		}
		if response.Error == nil {
			return nil
		}
		if response.Error.Code != protocol.ErrTaskActive {
			return fmt.Errorf("ducklion quiesce: %s", response.Error.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func callDucklionManagement(root string, conn *daemon.Client, id, operation string) (protocol.Response, error) {
	body, err := management.RequestBody(root)
	if err != nil {
		return protocol.Response{}, err
	}
	return conn.Call(protocol.Request{ID: id, Type: operation, InstanceID: conn.InstanceID(), Body: body})
}

func resumeDucklion(root string) error {
	conn, err := daemon.Dial(filepath.Join(root, "ducklion.sock"), "duckway-integration")
	if err != nil {
		return err
	}
	defer conn.Close()
	response, err := callDucklionManagement(root, conn, "restart-resume", "management.resume")
	if err != nil {
		return err
	}
	if response.Error != nil {
		return fmt.Errorf("resume Ducklion: %s", response.Error.Message)
	}
	return nil
}
