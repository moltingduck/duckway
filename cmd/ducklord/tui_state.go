package main

import (
	"context"
	"sync"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklion/protocol"
	"github.com/hackerduck/duckway/internal/ducklord"
)

type tuiState struct {
	modalMouseRegions    []modalMouseRegion
	modalMouseLines      map[int]modalMouseAction
	cfg                  *ducklord.Config
	cfgPath              string
	runner               remoteRunner
	refresh              time.Duration
	sessions             []ducklord.RemoteSession
	promoted             map[string]bool
	selected             int
	selectedGroupID      string
	dragSession          ducklord.SessionIdentity
	dragTargetGroup      string
	dragTargetSession    ducklord.SessionIdentity
	hashes               map[string]string
	selectedKey          string
	outputText           string
	outputErr            string
	localWarning         string
	notificationSink     notificationDelivery
	attentionDeliveryAt  map[string]time.Time
	notificationNow      func() time.Time
	notificationObserved map[string]map[model.NotificationCategory]uint64
	pendingBells         uint8
	resizeStatus         string
	outputForKey         string
	outputStale          bool
	outputFresh          bool
	outputReconnecting   bool
	terminal             *ducklord.Terminal
	terminalGeneration   uint64
	terminalOffset       uint64
	terminalCursorValid  bool
	activeAttachKey      string
	activeAttachFresh    bool
	pendingAttachKey     string
	snapshotStore        ducklord.SnapshotStore
	activityStore        ducklord.ActivityStateStore
	activityState        *ducklord.ActivityState
	focused              bool
	copyMode             bool
	copyRendering        bool
	frameOutput          frameOutput
	// terminalCookedState is the state captured before the TUI entered raw mode.
	// Notes editors temporarily restore it while they own the terminal.
	terminalCookedState                *termState
	copyTerminal                       *ducklord.Terminal
	newSessionMode                     bool
	newSessionClient                   string
	newSessionLine                     string
	newSessionSelected                 int
	newSessionErr                      string
	newSessionStarting                 bool
	newSessionStartGeneration          uint64
	newSessionStartInstance            string
	newSessionStartEpoch               uint64
	newSessionDiscovering              bool
	newSessionRequestID                uint64
	newSessionCancel                   context.CancelFunc
	createWorkers                      sync.WaitGroup
	newSessionStep                     string
	newSessionKind                     model.SessionKind
	newSessionAgent                    string
	newSessionCommand                  []string
	newSessionProjects                 []ducklord.RemoteProject
	newSessionProject                  ducklord.RemoteProject
	newSessionAgents                   []ducklord.RemoteAgent
	newSessionCWD                      string
	newSessionPathSuggestions          []string
	newSessionPathSelected             int
	newSessionPathRequestID            uint64
	newSessionPathCancel               context.CancelFunc
	newSessionPathBusy                 bool
	newSessionPathCompletion           string
	addClientMode                      bool
	addClientStep                      string
	addClientProvisionMode             string
	addClientModeSelected              int
	addClientLine                      string
	addClientSelected                  int
	addClientErr                       string
	addClientHosts                     []ducklord.SSHHost
	addClientRestoreFocused            bool
	addClientRestoreAttachKey          string
	addClientRestoreSession            ducklord.RemoteSession
	addClientBusy                      bool
	addClientRequestID                 uint64
	addClientCancel                    context.CancelFunc
	removeClientMode                   bool
	removeClientSelected               int
	removeClientConfirm                string
	helpMode                           bool
	helpOriginFocused                  bool
	helpOffset                         int
	helpSearchActive                   bool
	helpSearchQuery                    string
	ptyScrollOffset                    int
	shortcutMode                       bool
	shortcutStep                       string
	shortcutIndex                      int
	shortcutLine                       string
	shortcutErr                        string
	shortcutDraft                      *ducklord.Config
	notificationConfigMode             bool
	notificationConfigStep             string
	notificationConfigScope            string
	notificationConfigHost             string
	notificationConfigIndex            int
	notificationConfigChoice           int
	notificationConfigPath             string
	notificationConfigErr              string
	notificationConfigDraft            *ducklord.Config
	notificationConfigBase             []byte
	hostMenuMode                       bool
	hostMenuStep                       string
	hostMenuTarget                     string
	hostMenuIndex                      int
	hostMenuSelected                   map[string]bool
	hostMenuErr                        string
	hostMenuOldDays                    int
	hostMenuDraft                      string
	hostMenuReplaceDraft               bool
	hostMenuRequestID                  uint64
	hostResourceStatus                 protocol.HostResourceStatus
	hostSkillsIndex                    int
	hostSkillsSourceIndex              int
	hostSkillsField                    int
	hostSkillsDraftURL                 string
	hostSkillsDraftSkillID             string
	hostSkillsDraftInsecure            bool
	hostSkillsDraftTargetID            string
	hostSkillsDraftTargetPath          string
	hostSkillsSelected                 map[string]bool
	hostSkillsRemote                   []string
	hostSkillsErr                      string
	hostSkillsRepoErr                  string
	hostSkillsRepository               string
	hostSkillsPreview                  *ducklord.SkillPreview
	hostSkillsPreviewAction            string
	hostSkillsTargetIndex              int
	hostSkillsPendingID                string
	hostSkillsDraftImportPath          string
	hostSkillsDraftImportID            string
	hostSkillsBusy                     bool
	hostSkillsRequestID                uint64
	hostSkillsListRequestID            uint64
	hostSkillsCancel                   context.CancelFunc
	hostSkillsListCancel               context.CancelFunc
	hostSkillsDone                     chan hostSkillsEvent
	hostSkillsPane                     int // 0 managed repository, 1 agent tree
	hostSkillsTargetRemoteIndex        int
	hostSkillsRemoteByTarget           map[string][]string
	hostSkillsTargetExpanded           map[string]bool
	hostSkillsRenameOld                string
	hostSkillsRenameDraft              string
	hostHookInFlight                   map[string]uint64
	hostHookStatuses                   map[string]protocol.HostAgentHookStatus
	hostHookStatusRequestID            uint64
	hostHookStatusLoading              bool
	hostHookStatusErr                  string
	hostHookAgent                      string
	hostHookAction                     string
	disconnectedHosts                  map[string]bool
	hostScoped                         bool
	ownerName                          string
	listPaneWidth                      int
	autoHideList                       bool
	hostSync                           map[string]ducklord.SessionUpdate
	hostConnectionEpoch                map[string]uint64
	eventDriven                        bool
	notificationMode                   bool
	notificationIndex                  int
	notificationStaged                 map[model.NotificationCategory]bool
	notificationLevelsStaged           map[ducklord.NotificationClass]ducklord.NotificationLevel
	notificationLevelEditing           bool
	notificationLevelChoice            int
	notificationTarget                 ducklord.RemoteSession
	actionMenu                         bool
	actionIndex                        int
	actionTarget                       ducklord.RemoteSession
	actionOperation                    protocol.SessionLifecycleOperation
	actionMode                         protocol.SessionLifecycleMode
	sessionRenameMode                  bool
	sessionRenameTarget                ducklord.RemoteSession
	sessionRenameLine                  string
	sessionRenameErr                   string
	lifecycleConfirm                   protocol.SessionLifecycleOperation
	projectFiles                       projectFilesState
	projectFilesHistory                map[string][]projectFilesBatch
	lifecycleReturnToAction            bool
	lifecycleTarget                    ducklord.RemoteSession
	lifecycleMode                      protocol.SessionLifecycleMode
	lifecycleBusy                      bool
	commandPaletteMode                 bool
	commandPaletteQuery                string
	commandPaletteIndex                int
	commandPaletteAction               string
	commandPalettePreviousFocused      bool
	commandPalettePreviousAttachKey    string
	commandPalettePreviousProjectFocus bool
	commandPalettePreviousProjectID    string
	commandPalettePreviousPaneID       string
	commandPaletteWorkspaceGeneration  uint64
	commandPaletteInput                []byte
	groupMenu                          bool
	groupMenuStep                      string
	groupMenuAction                    string
	groupMenuIndex                     int
	groupMenuLine                      string
	groupMenuErr                       string
	groupMenuTarget                    string
	groupMenuSession                   ducklord.SessionIdentity
	pooledOutput                       bool
	workspacePreview                   bool
	workspaceOutput                    *ducklord.WorkspaceOutputAdapter
	workspaceNav                       *ducklord.WorkspaceState
	workspaceProjectFocus              bool
	workspaceConfigFocus               string
	workspaceConfigIdentity            ducklord.SessionIdentity
	panePrefixPending                  bool
	quickShellOrigin                   *workspacePaneIntent
	panePrefixSuffix                   string
	panePrefixDeadline                 time.Time
	panePrefixReplay                   []byte
	workspaceAttachFromProject         bool
	workspaceFocusFromProject          bool
	workspaceMouseFocus                bool
	workspacePaneMode                  bool
	workspacePaneStep                  string
	workspacePaneIndex                 int
	workspacePaneErr                   string
	workspacePaneName                  string
	workspacePaneHosts                 []string
	workspacePaneQuery                 string
	workspacePaneIntent                workspacePaneIntent
	workspacePaneSourceID              string
	workspacePaneIdentity              ducklord.SessionIdentity
	workspacePaneCandidate             ducklord.RemoteSession
	projectTransferPath                string
	projectTransferData                []byte
	projectTransfer                    ducklord.ProjectTransfer
	projectTransferPreview             ducklord.ProjectImportPreview
	projectTransferAction              string
	workspacePaneRestoreFocused        bool
	workspacePaneRestoreAttachKey      string
	workspacePaneFocusRestorePending   bool
	workspacePaneFocusRestoreLease     bool
	workspacePaneFocusRestoreKey       string
	workspacePaneFocusRestoreSession   ducklord.RemoteSession
	workspaceDragSession               ducklord.RemoteSession
	workspaceDragKind                  string
	workspaceDragProjectID             string
	workspaceDragTabID                 string
	workspaceDragMoved                 bool
	workspaceDragX                     int
	workspaceDragY                     int
	workspaceProjectOffset             int
	workspaceQuickOffset               int
	workspaceScrollbarDrag             string
	workspaceScrollbarDragGrab         int
	helpScrollbarDrag                  bool
	helpScrollbarDragGrab              int
	helpResultTotal                    int
	workspacePaneChanged               bool
	workspacePlacementFocusPending     bool
	workspacePlacementInputPending     bool
	notesEntries                       []ducklord.NoteEntry
	notesScope                         ducklord.NotesScope
	notesSessionID                     string
	notesSessionIdentity               ducklord.SessionIdentity
	notesEntryIndexes                  []int
	notesEntryScopes                   []ducklord.NotesScope
	notesEntryIdentities               []string
	notesEntryOrigins                  []string
	notesPickerMode                    string
	notesPickerChoices                 []string
	notesPickerLabels                  []string
	notesPickerIndex                   int
	notesQuery                         string
	notesSearchActive                  bool
	notesProjectID                     string
	notesPreviousProjectID             string
	notesPreviousPaneID                string
	notesPreviousProjectFocus          bool
	notesPreviousFocused               bool
	notesFocusRestorePending           bool
	notesPendingInputKey               string
	notesPendingInput                  []byte
	notesFocusRestoreReselect          bool
	notesPreviousAttachKey             string
	notesPreviousAttachValid           bool
	helpFocusRestorePending            bool
	helpPendingInputKey                string
	notesFormActive                    bool
	notesFormIndex                     int
	notesFormRawIndex                  int
	notesFormField                     int
	notesFormTitle                     string
	notesFormBody                      string
	notesFormCursor                    int
	workspaceNewSessionIntent          *workspacePaneIntent
	pendingWorkspacePlacements         []pendingWorkspacePlacement
	detailQuery                        string
	detailFilter                       ducklord.DetailFilter
	detailSearchFocused                bool
	detailReturnProjectFocus           bool
	detailSelected                     ducklord.SessionIdentity
	clearOutputFocus                   func()
	searchMode                         bool
	terminalSearchMode                 bool
	terminalSearchQuery                string
	terminalSearchText                 string
	terminalSearchSelected             int
	terminalBookmarkMode               bool
	terminalBookmarkLabel              string
	terminalToolInput                  []byte
	terminalBookmarkListMode           bool
	terminalBookmarkSelected           int
	terminalBookmarkPreviousFocus      string
	searchQuery                        string
	searchSelected                     int
	searchSelectedKey                  string
	searchRevision                     uint64
	searchActivatedKey                 string
	searchActivatedRevision            uint64
	searchActivatedGeneration          uint64
	searchErr                          string
	searchPendingRequestID             uint64
	searchPendingKey                   string
	searchPendingGeneration            uint64
	searchPendingRevision              uint64
}

const notesPendingInputLimit = 4096

func runTUI(cfg *ducklord.Config, runner remoteRunner, cfgPath string, refresh time.Duration, owner string) error {
	return runTUIWithOptions(cfg, runner, cfgPath, refresh, false, owner)
}

func runHostTUI(cfg *ducklord.Config, runner remoteRunner, refresh time.Duration, ownerFlag string) error {
	owner, err := ducklord.ResolveOwnerName(ownerFlag, cfg.Name)
	if err != nil {
		return err
	}
	if ownerRunner, ok := runner.(interface{ SetOwner(string) }); ok {
		ownerRunner.SetOwner(owner)
	}
	lock, err := ducklord.AcquireInstanceLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return runTUIWithOptions(cfg, runner, "", refresh, true, owner)
}
