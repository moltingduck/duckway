package main

import (
	"context"
	"os"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

// tuiLoop holds the state of one runTUIWithOptions invocation that used to
// live in the run function's local variables: the event channels, the pooled
// output/attach/control bookkeeping, the preview fence and the pane-prefix
// timer. All fields are owned by the single goroutine that runs the loop;
// background goroutines only communicate through the channels.
type tuiLoop struct {
	ctx    context.Context
	stop   context.CancelFunc
	cfg    *ducklord.Config
	runner remoteRunner
	state  *tuiState

	// Pooled terminal output.
	outputManager         tuiOutputManager
	workspaceOutput       *ducklord.WorkspaceOutputAdapter
	outputEvents          <-chan ducklord.TerminalOutputEvent
	workspaceRepaint      <-chan struct{}
	outputRequestID       uint64
	reconnectRequestID    uint64
	activeOutputEvent     ducklord.TerminalOutputEvent
	selectedOutputSession ducklord.RemoteSession
	lastOutputRender      time.Time

	// Host session watching.
	sessionUpdates chan hostSessionUpdate
	watchedClients map[string]hostWatchState
	watchEpoch     uint64
	watcher        interface {
		WatchSessionUpdates(context.Context, ducklord.Client) <-chan ducklord.SessionUpdate
	}
	eventDriven bool

	// Timers, signals and raw input.
	ticker          *time.Ticker
	resizeSignals   chan os.Signal
	input           chan []byte
	inputReady      chan struct{}
	inputPending    bool
	panePrefixTimer *time.Timer
	replay          workspaceInputReplay

	// Completion channels for asynchronous work.
	startDone               chan startDoneEvent
	hostRetentionDone       chan hostRetentionEvent
	hostResourceDone        chan hostResourceEvent
	hostHookDone            chan hostHookEvent
	hostHookStatusDone      chan hostHookStatusEvent
	hostSkillsDone          chan hostSkillsEvent
	projectFilesDone        chan projectFilesEvent
	createDone              chan createDiscoveryEvent
	pathSuggestionsDone     chan pathSuggestionEvent
	addClientDone           chan addClientDoneEvent
	lifecycleDone           chan lifecycleDoneEvent
	previewDone             chan previewOutputEvent
	searchActivationTimeout chan uint64
	hostRetentionCancel     context.CancelFunc
	lifecycleRequestID      uint64
	startCancel             context.CancelFunc
	startID                 int

	// Preview fence (non-pooled output).
	previewID       uint64
	previewInFlight bool
	previewQueued   bool
	previewCancel   context.CancelFunc

	// Attach / control / resize bookkeeping.
	attach                    *ducklord.AttachSession
	control                   *ducklord.ControlSession
	controlDone               <-chan error
	pendingControlSince       time.Time
	controlOpened             chan controlOpenEvent
	controlID                 int
	controlOpenCancel         context.CancelFunc
	attachCancel              context.CancelFunc
	attachID                  int
	attachCanResize           bool
	attachInitialResizeQueued bool
	attachReplayEndOffset     uint64
	pendingFramebufferResize  *resizeDoneEvent
	resizeInFlight            bool
	queuedResize              *[2]uint16
	bufferedAttach            []attachOutputEvent
	bufferedAttachBytes       int
	attachOut                 chan attachOutputEvent
	attachOutputSource        <-chan attachOutputEvent
	resizeRequests            chan resizeRequest
	resizeResults             chan resizeDoneEvent
	resizeWorkerDone          chan struct{}
}
