package main

import (
	"fmt"
	"path/filepath"
	"time"

	"github.com/hackerduck/duckway/internal/ducklord"
)

type projectFilesItem struct {
	name, destination, state, err string
}

type projectFilesBatch struct {
	at                            time.Time
	source, destination           ducklord.FileEndpoint
	sourceLabel, destinationLabel string
	names                         []string
	conflict                      string
	items                         []projectFilesItem
	err                           string
}

type projectFilesPanelGeometry struct {
	start, width, entriesStart, rows int
	left, right                      int
}

// projectFilesGeometry is the sole source for entry drawing and mouse regions.
func projectFilesGeometry(cols, rows, headerLines int) (int, int, [2]projectFilesPanelGeometry) {
	width := min(110, max(8, cols-2))
	stacked := width < 58
	var panels [2]projectFilesPanelGeometry
	if stacked {
		cw := max(3, width-6)
		entryStart := headerLines + 4
		available := max(0, rows-2-entryStart-5)
		per := available / 2
		for i := range panels {
			panels[i] = projectFilesPanelGeometry{start: headerLines + i*(3+per), width: cw, entriesStart: headerLines + i*(3+per) + 3, rows: per, left: 2, right: cw + 1}
		}
		return width, max(1, cw), panels
	}
	cw := max(3, (width-12)/2)
	entryStart := headerLines + 3
	available := max(0, rows-2-entryStart-3)
	for i := range panels {
		left := 2
		if i == 1 {
			left = cw + 4
		}
		panels[i] = projectFilesPanelGeometry{start: headerLines, width: cw, entriesStart: entryStart, rows: available, left: left, right: left + cw - 1}
	}
	return width, cw, panels
}

func projectFilesVisibleOffset(selected, visibleRows int) int {
	return max(0, selected-visibleRows+1)
}

func projectFilesPanelColor(side int, active bool) string {
	if side == 0 {
		if active {
			return "\x1b[1;38;2;34;211;238;48;2;12;38;52m"
		}
		return "\x1b[38;2;34;211;238;48;2;24;39;53m"
	}
	if active {
		return "\x1b[1;38;2;196;181;253;48;2;39;29;58m"
	}
	return "\x1b[38;2;167;139;250;48;2;31;35;56m"
}

func projectFilesStatusStyle(status string) string {
	switch status {
	case "copied", "received", "sent":
		return "\x1b[1;38;2;74;222;128m"
	case "skipped":
		return "\x1b[1;38;2;250;204;21m"
	case "failed", "cancelled":
		return "\x1b[1;38;2;248;113;113m"
	case "copying":
		return "\x1b[1;38;2;251;191;36m"
	default:
		return modalMuted
	}
}

func (s *tuiState) projectFilesProjectKey() string {
	if s.projectFiles.historyProject != "" {
		return s.projectFiles.historyProject
	}
	return "<no-project>"
}

func (s *tuiState) projectFilesRememberBatch(batch projectFilesBatch) {
	key := s.projectFilesProjectKey()
	if s.projectFilesHistory == nil {
		s.projectFilesHistory = make(map[string][]projectFilesBatch)
	}
	history := append([]projectFilesBatch{batch}, s.projectFilesHistory[key]...)
	if len(history) > 20 {
		history = history[:20]
	}
	s.projectFilesHistory[key] = history
	s.projectFiles.history = history
}

func projectFilesEndpointKey(endpoint ducklord.FileEndpoint) string {
	host := "LOCAL"
	if endpoint.Client != nil {
		client := endpoint.Client
		host = fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", client.Name, client.Host, client.User, client.SSH, client.Ducklion)
	}
	return host + "\x00" + filepath.Clean(endpoint.Path)
}

func (s *tuiState) projectFilesEndpointDirection(endpoint ducklord.FileEndpoint) string {
	if len(s.projectFiles.history) == 0 {
		return ""
	}
	b := s.projectFiles.history[0]
	key := projectFilesEndpointKey(endpoint)
	for _, item := range b.items {
		if item.state != "copied" {
			continue
		}
		if key == projectFilesEndpointKey(b.source) {
			return " · Sent"
		}
		dest := item.destination
		if dest == "" {
			dest = filepath.Join(b.destination.Path, item.name)
		}
		if key == projectFilesEndpointKey(b.destination) && filepath.Dir(filepath.Clean(dest)) == filepath.Clean(endpoint.Path) {
			return " · Received"
		}
	}
	return ""
}

func projectFilesItemRow(item projectFilesItem) string {
	word, symbol := item.state, "·"
	switch item.state {
	case "copying":
		word, symbol = "Copying", "▶"
	case "copied":
		word, symbol = "Copied", "✓"
	case "skipped":
		word, symbol = "Skipped", "!"
	case "failed":
		word, symbol = "Failed", "×"
	case "cancelled":
		word, symbol = "Cancelled", "×"
	case "not started":
		word, symbol = "Not started", "·"
	case "queued":
		word, symbol = "Queued", "·"
	}
	return fmt.Sprintf("%s %s · %s", symbol, word, item.name)
}

func (s *tuiState) projectFilesLatestMark(p projectFilesPane, entry ducklord.FileEntry) string {
	if len(s.projectFiles.history) == 0 {
		return ""
	}
	b := s.projectFiles.history[0]
	key := projectFilesEndpointKey(p.endpoint)
	if key == projectFilesEndpointKey(b.source) {
		for _, item := range b.items {
			if item.name == entry.Name && item.state == "copied" {
				return projectFilesStatusStyle("sent") + "Sent" + modalReset
			}
		}
	}
	if key == projectFilesEndpointKey(b.destination) {
		for _, item := range b.items {
			if item.state != "copied" {
				continue
			}
			dest := item.destination
			if dest == "" {
				dest = filepath.Join(b.destination.Path, item.name)
			}
			if filepath.Clean(dest) == filepath.Clean(filepath.Join(p.endpoint.Path, entry.Name)) {
				return projectFilesStatusStyle("received") + "Received" + modalReset
			}
		}
	}
	return ""
}
