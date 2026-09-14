package main

import "github.com/hackerduck/duckway/internal/ducklord"

// tuiOutputManager keeps the legacy single-pane TUI and workspace TUI on the
// same event contract while their raw stream ownership remains exclusive.
type tuiOutputManager interface {
	Select(ducklord.TerminalSelection) uint64
	Reconnect(ducklord.TerminalSelection) uint64
	Events() <-chan ducklord.TerminalOutputEvent
	View(ducklord.TerminalOutputEvent) (ducklord.PooledTerminalView, error)
	Resize(ducklord.TerminalOutputEvent, uint16, uint16, func(uint16, uint16) (uint64, error)) (uint64, error)
	ForgetHost(string, string)
	SyncHost(string, string, bool, []ducklord.TerminalSelection)
	Close() error
}
