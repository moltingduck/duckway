#!/usr/bin/env bash
# Credential-free release gate for docs/cc-terminal-pty-handoff-spec.md §196.
# Every command is intentionally selected by name: deleting or renaming a
# required scenario makes this gate fail instead of silently reducing coverage.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
cd "$PROJECT_DIR"

GO_TEST=(go test -count=1)
if [ "${DUCKLION_E2E_RACE:-0}" = "1" ]; then
  GO_TEST+=( -race )
fi

run_required() {
  local label="$1"
  local package="$2"
  shift 2
  local tests=("$@")
  local test_name listed regex
  echo "[ducklion-core-e2e] $label"
  for test_name in "${tests[@]}"; do
    listed="$(go test "$package" -list "^${test_name}$")"
    if ! grep -Fxq "$test_name" <<<"$listed"; then
      echo "[ducklion-core-e2e] MISSING required test: $package $test_name" >&2
      return 1
    fi
  done
  regex="^($(IFS='|'; echo "${tests[*]}"))$"
  "${GO_TEST[@]}" "$package" -run "$regex" -v
}

run_required "stdio bridge attach and mutation response-loss replay" ./internal/ducklord \
  TestRunnerAttachUsesMultiplexedBridgeForOutputAndInput \
  TestRunnerStartReplaysCommittedMutationAfterBridgeDisconnect \
  TestRunnerYieldTransfersCCSessionAndWaitsThroughBridge

run_required "process-wide raw-output transactional handoff" ./internal/ducklord \
  TestOutputPoolHandoffRollbackAndCommitAreAtomic \
  TestOutputPoolSnapshotFailureRollsBackDestination \
  TestOutputPoolLeaseFencesStaleReader \
  TestOutputPoolCloseDuringBlockedOpen \
  TestOutputPoolDisconnectRestorePreservesDesiredOrder \
  TestOutputPoolDisconnectFencesDelayedRestore \
  TestOutputPoolEvictionQuiescesAcceptedFramesBeforeSnapshot \
  TestOutputPoolRuntimeGenerationReplacementIsSingleMembership \
  TestOutputPoolVictimHostDisconnectRollsBackCrossHostHandoff

run_required "ownership fencing, bidirectional yield, recovery, shell sharing and lifecycle" ./internal/ducklion/daemon \
  TestDuckwayCCCreatesAgentWithCCInitialOwner \
  TestDucklordInputAndResizeAreOwnerFenced \
  TestTerminalYieldTransfersCCSessionAndSynchronizesSupervisor \
  TestSupervisorRecoveryRegistrationIsConnectionBound \
  TestSupervisorRecoveryRejectsWrongPrivateKey \
  TestManagedPTYPersistsAcrossDaemonRestart \
  TestShellConcurrentDucklordAttachmentsAreWritable \
  TestShellLifecycleRestartEndAndDestroy \
  TestCanonicalLifecycleWaitResumesAfterDucklionRestart \
  TestCanonicalLifecycleRestartPreservesBindingAndAdvancesGeneration \
  TestCanonicalLifecycleForceRestartCancelsAndFencesOldRuntimeEvent

run_required "durable Discord bind, ownership rejection, yield and restart delivery barriers" ./internal/client \
  TestDiscordBindExistingDucklionSessionE2E \
  TestDiscordOwnershipRejectionVerticalE2E \
  TestDiscordYieldCommandUsesDurableDucklionBindingE2E \
  TestDiscordRestartWaitsForFinalEventDeliveryE2E \
  TestDiscordForceRestartDeliversCancellationBeforeReplacementE2E

run_required "Discord gateway admission and durable per-channel inbox lanes" ./internal/server/services \
  TestDiscordGatewayResumeReplayE2E \
  TestDiscordClientCommandUsesDurableInboxE2E

run_required "inbox snowflake deduplication, claims, FIFO and reclaim" ./internal/database/queries \
  TestInboxAdmissionDeduplicatesEventKey \
  TestInboxClaimEnforcesLaneFIFOAndToken \
  TestInboxExpiredLeaseIsReclaimedBeforeLaterLaneItem

run_required "migration backup and forward compatibility" ./internal/ducklion/store \
  TestOpenMigratesV1DatabaseAndCreatesBackup \
  TestOpenRejectsNewerSchemaWithoutChangingIt

echo "[ducklion-core-e2e] PASS"
