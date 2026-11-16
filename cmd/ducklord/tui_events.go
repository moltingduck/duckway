package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

var errRestartTUI = errors.New("restart ducklord TUI")

// Keep a lone ESC pending briefly so a normal arrow sequence can arrive in a
// separate read without making bare-Esc cancellation feel sluggish.
const inputEscapeAmbiguityTimeout = 25 * time.Millisecond

type remoteRunner interface {
	Sessions(context.Context, ducklord.Client, int) ([]ducklord.RemoteSession, error)
	Read(context.Context, ducklord.Client, string, int) (string, error)
	Send(context.Context, ducklord.Client, string, string) error
	Start(context.Context, ducklord.Client, []string) (string, error)
	Stop(context.Context, ducklord.Client, string) error
	Lifecycle(context.Context, ducklord.Client, string, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error)
	LifecycleSelected(context.Context, ducklord.Client, ducklord.RemoteSession, protocol.SessionLifecycleOperation, protocol.SessionLifecycleMode) (protocol.SessionLifecycleResult, error)
	Yield(context.Context, ducklord.Client, string, bool) (protocol.SessionYieldResult, error)
	YieldSelected(context.Context, ducklord.Client, ducklord.RemoteSession, bool) (protocol.SessionYieldResult, error)
	RenameSelected(context.Context, ducklord.Client, ducklord.RemoteSession, string) (protocol.SessionSummary, error)
	Projects(context.Context, ducklord.Client) ([]ducklord.RemoteProject, error)
	SuggestProjectPaths(context.Context, ducklord.Client, string) ([]string, error)
	EnsureDirectory(context.Context, ducklord.Client, string, bool) (ducklord.RemoteDirectoryStatus, error)
	AddProject(context.Context, ducklord.Client, string, string) (ducklord.RemoteProject, error)
	Agents(context.Context, ducklord.Client, string) ([]ducklord.RemoteAgent, error)
	ProbeDucklion(context.Context, ducklord.Client) (ducklord.DucklionProbe, error)
	HostLogRetention(context.Context, ducklord.Client) (int, error)
	HostResources(context.Context, ducklord.Client) (protocol.HostResourceStatus, error)
	SetHostLogRetention(context.Context, ducklord.Client, int) error
	ConfigureHostAgentHook(context.Context, ducklord.Client, string, string) (protocol.HostAgentHookConfigResult, error)
	HostAgentHookStatus(context.Context, ducklord.Client, string) (protocol.HostAgentHookStatus, error)
	InstallDucklion(context.Context, ducklord.Client, string, string) (string, error)
	Attach(ducklord.Client, string) error
	AttachStream(context.Context, ducklord.Client, string) (*ducklord.AttachSession, error)
}

type attachOutputEvent struct {
	id                int
	text              string
	runtimeGeneration uint64
	outputOffset      uint64
	startOffset       uint64
	done              bool
	err               error
}

type controlOpenEvent struct {
	id      int
	key     string
	control *ducklord.ControlSession
	err     error
}

type previewOutputEvent struct {
	id, generation uint64
	key            string
	text           string
	err            error
}

type startDoneEvent struct {
	id        int
	client    string
	sessionID string
	err       error
}

type pendingWorkspacePlacement struct {
	intent          workspacePaneIntent
	client          string
	instanceID      string
	sessionID       string
	generation      uint64
	connectionEpoch uint64
	createdAt       time.Time
}

type hostRetentionEvent struct {
	id         uint64
	epoch      uint64
	instanceID string
	host       string
	days       int
	err        error
	saved      bool
}
type hostResourceEvent struct {
	id         uint64
	epoch      uint64
	instanceID string
	host       string
	status     protocol.HostResourceStatus
	err        error
}

// publishHostResourceEvent never lets a refresh worker wait behind an older
// completion. The request, host epoch, and instance checks still decide
// whether a delivered event may affect state.
func publishHostResourceEvent(ctx context.Context, done chan<- hostResourceEvent, event hostResourceEvent) {
	select {
	case done <- event:
	case <-ctx.Done():
	default:
	}
}

type hostHookEvent struct {
	id, epoch                       uint64
	instanceID, host, agent, action string
	result                          protocol.HostAgentHookConfigResult
	err                             error
}

type hostSkillsEvent struct {
	id       uint64
	action   string
	host     string
	targetID string
	skillID  string
	preview  *ducklord.SkillPreview
	names    []string
	count    int
	err      error
}

type hostHookStatusEvent struct {
	id, epoch        uint64
	instanceID, host string
	statuses         map[string]protocol.HostAgentHookStatus
	err              error
}

func hostHookResultMessage(event hostHookEvent) string {
	if !event.result.Changed {
		return "Already in the requested state"
	}
	if event.action == "remove" {
		return "Host hook removed; agent settings were preserved"
	}
	return "Host configuration updated; activation awaits a real agent callback"
}

func hostHookStatusLine(status protocol.HostAgentHookStatus) string {
	if status.Agent != "codex" && status.Agent != "claude" {
		return "  Hook status unavailable"
	}
	installed := "not installed"
	if status.Installed {
		installed = "installed · pending activation"
		if status.Activation == "operational" {
			installed = "installed · operational (advisory)"
		}
	}
	callback := "no callback observed"
	if status.CallbackObserved {
		callback = "advisory callback observed"
		if status.CallbackUpdatedAtMS > 0 {
			callback += " " + time.UnixMilli(status.CallbackUpdatedAtMS).Local().Format("2006-01-02 15:04")
		}
	}
	return "  " + strings.ToUpper(status.Agent[:1]) + status.Agent[1:] + ": " + installed + " · " + callback
}

type createDiscoveryEvent struct {
	id, generation         uint64
	kind, client, instance string
	project                ducklord.RemoteProject
	projects               []ducklord.RemoteProject
	agents                 []ducklord.RemoteAgent
	sessionName            string
	failureStep            string
	args                   []string
	err                    error
	directory              ducklord.RemoteDirectoryStatus
	homePath               string
}

type hostSessionUpdate struct {
	update ducklord.SessionUpdate
	epoch  uint64
}

type hostWatchState struct {
	cancel context.CancelFunc
	epoch  uint64
}

type pathSuggestionEvent struct {
	id, generation          uint64
	client, instance, query string
	paths                   []string
	err                     error
}

type lifecycleDoneEvent struct {
	id              uint64
	watchEpoch      uint64
	connectionEpoch uint64
	hostGeneration  uint64
	hostInstance    string
	operation       protocol.SessionLifecycleOperation
	client          string
	result          protocol.SessionLifecycleResult
	err             error
}

type addClientDoneEvent struct {
	id         uint64
	progress   bool
	status     string
	client     ducklord.Client
	probe      ducklord.DucklionProbe
	installed  bool
	installErr error
	err        error
}

type resizeRequest struct {
	id     int
	rows   uint16
	cols   uint16
	resize func(uint16, uint16) (uint64, error)
}

type resizeDoneEvent struct {
	id         int
	rows, cols uint16
	err        error
	barrier    uint64
}

const maxResizeBufferedBytes = 256 << 10
