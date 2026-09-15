package main

import (
	"errors"
	"reflect"
	"testing"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"github.com/hackerduck/duckway/internal/ducklord"
)

func TestCreateShellRevalidatesDefaultBeforeStarting(t *testing.T) {
	project := ducklord.RemoteProject{Path: "/home/remote", Source: "path"}
	for _, failed := range []bool{false, true} {
		runner := fakeRunner{agents: []ducklord.RemoteAgent{{Type: "shell", Command: []string{"/usr/bin/fish"}}}}
		if failed {
			runner.agentsErr = errors.New("account lookup unavailable")
		}
		state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: runner,
			newSessionMode: true, newSessionKind: model.KindShell, newSessionStep: "handle", newSessionClient: "host", newSessionProject: project,
			newSessionCWD: project.Path, newSessionAgent: "shell", newSessionCommand: []string{"/bin/bash"}, newSessionLine: "work"}
		_, _, args, ready := createSubmit(t, state)
		if failed {
			if ready || state.newSessionStep != "project" {
				t.Fatalf("failed revalidation: ready=%v step=%q", ready, state.newSessionStep)
			}
		} else if !ready || len(args) < 2 || args[len(args)-2] != "--" || args[len(args)-1] != "/usr/bin/fish" {
			t.Fatalf("default shell was not revalidated: ready=%v args=%q", ready, args)
		}
	}
}

func TestCreateShellSkipsSelectionAndUsesHostDefault(t *testing.T) {
	for _, workspace := range []bool{false, true} {
		state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: fakeRunner{}}
		if workspace {
			state, _, _, _ = workspacePaneTestState(t)
			state.beginWorkspacePane()
			state.handleWorkspacePaneInput([]byte("\r"))
			state.handleWorkspacePaneInput([]byte("\r"))
			if state.workspaceNewSessionIntent == nil {
				t.Fatal("missing workspace placement")
			}
		}
		state.newSessionMode, state.newSessionDiscovering = true, true
		state.newSessionKind = model.KindShell
		state.newSessionLine, state.newSessionSelected = "2", 1
		generation, instance := state.hostFingerprint("host")
		state.applyCreateDiscovery(createDiscoveryEvent{kind: "agents", client: "host", generation: generation, instance: instance,
			project: ducklord.RemoteProject{Path: "/home/remote"}, agents: []ducklord.RemoteAgent{
				{Type: "bash", Command: []string{"/bin/bash"}}, {Type: "shell", Command: []string{"/usr/bin/fish"}}, {Type: "zsh", Command: []string{"/bin/zsh"}},
			}})
		if state.newSessionStep != "handle" || state.newSessionAgent != "shell" || !reflect.DeepEqual(state.newSessionCommand, []string{"/usr/bin/fish"}) {
			t.Fatalf("workspace=%v step=%q agent=%q command=%q error=%q", workspace, state.newSessionStep, state.newSessionAgent, state.newSessionCommand, state.newSessionErr)
		}
		if state.newSessionLine != "" || state.newSessionSelected != 0 {
			t.Fatalf("previous selection leaked into handle: line=%q selected=%d", state.newSessionLine, state.newSessionSelected)
		}
	}
}

func TestCreateShellDoesNotSubstituteAnotherShellWhenDefaultMissing(t *testing.T) {
	state := &tuiState{cfg: &ducklord.Config{Clients: []ducklord.Client{{Name: "host", Host: "host"}}}, runner: fakeRunner{}, newSessionMode: true, newSessionDiscovering: true, newSessionKind: model.KindShell}
	state.applyCreateDiscovery(createDiscoveryEvent{kind: "agents", client: "host", agents: []ducklord.RemoteAgent{{Type: "bash", Command: []string{"/bin/bash"}}}})
	if state.newSessionStep != "project" || state.newSessionErr == "" {
		t.Fatalf("step=%q error=%q", state.newSessionStep, state.newSessionErr)
	}
}
