package ducklord

import (
	"fmt"
	"io"
	"strings"
)

type DetailGeometry struct {
	List WorkspaceRect
	Pane WorkspaceRect
}

// CalculateDetailGeometry replaces the three-region workspace with a list
// and one Session pane. A very narrow terminal retains the single preview.
func CalculateDetailGeometry(width, height, top int) DetailGeometry {
	if width < 1 || height < top {
		return DetailGeometry{}
	}
	remaining := height - top + 1
	result := DetailGeometry{Pane: WorkspaceRect{X: 1, Y: top, Width: width, Height: remaining}}
	if width < 44 {
		return result
	}
	listWidth := max(22, min(52, width*2/5))
	result.List = WorkspaceRect{X: 1, Y: top, Width: listWidth, Height: remaining}
	result.Pane = WorkspaceRect{X: listWidth + 2, Y: top, Width: width - listWidth - 1, Height: remaining}
	return result
}

// RenderDetailedSessionBody is presentation-only. The caller supplies a
// filtered list and the sole selected Session's pane view; it cannot attach,
// resize a remote PTY, clear unread, or grant writer ownership.
func RenderDetailedSessionBody(out io.Writer, geometry DetailGeometry, items []DetailedSessionItem,
	selected SessionIdentity, query string, filter DetailFilter, focused bool,
	paneView func(SessionIdentity, int, int) WorkspacePaneView) {
	if out == nil {
		return
	}
	selectedIndex := -1
	for i := range items {
		if items[i].Identity == selected {
			selectedIndex = i
			break
		}
	}
	if geometry.List.Width > 0 && geometry.List.Height > 0 {
		renderDetailList(out, geometry.List, items, selectedIndex, query, filter)
	}
	if geometry.Pane.Width < 1 || geometry.Pane.Height < 1 {
		return
	}
	if selectedIndex < 0 {
		workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y, geometry.Pane.Width, " SESSION PREVIEW ", "\x1b[1;34m")
		message := "No matching Sessions"
		if len(items) != 0 {
			message = "Select a Session to preview"
		}
		workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y+1, geometry.Pane.Width, message, "\x1b[2m")
		for row := 2; row < geometry.Pane.Height; row++ {
			workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y+row, geometry.Pane.Width, "", "")
		}
		return
	}
	item := items[selectedIndex]
	data := WorkspacePaneView{Title: item.Host + "/" + item.Name, Stale: item.Disconnected, ReadOnly: true}
	if paneView != nil {
		data = paneView(item.Identity, geometry.Pane.Width, max(0, geometry.Pane.Height-1))
	}
	if data.Title == "" {
		data.Title = item.Host + "/" + item.Name
	}
	if item.Disconnected {
		data.Stale = true
		data.ReadOnly = true
	}
	marker, color := "◇", "\x1b[1;36m"
	if data.ReadOnly {
		marker, color = "◌", "\x1b[1;33m"
	} else if focused {
		marker, color = "▣", "\x1b[1;32m"
	}
	if data.Stale {
		data.Title += " [disconnected/stale]"
	}
	workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y, geometry.Pane.Width, marker+" "+data.Title, color)
	for row := 1; row < geometry.Pane.Height; row++ {
		line := ""
		if row-1 < len(data.Lines) {
			line = data.Lines[row-1]
		}
		workspaceWriteVT(out, geometry.Pane.X, geometry.Pane.Y+row, geometry.Pane.Width, line)
	}
}

func renderDetailList(out io.Writer, rect WorkspaceRect, items []DetailedSessionItem, selectedIndex int, query string, filter DetailFilter) {
	workspaceWrite(out, rect.X, rect.Y, rect.Width, " SESSIONS · "+string(filter), "\x1b[1;34m")
	if rect.Height < 2 {
		return
	}
	workspaceWrite(out, rect.X, rect.Y+1, rect.Width, " find › "+query, "\x1b[1;36m")
	if len(items) == 0 {
		workspaceWrite(out, rect.X, rect.Y+2, rect.Width, " No matching Sessions", "\x1b[2m")
		for row := 3; row < rect.Height; row++ {
			workspaceWrite(out, rect.X, rect.Y+row, rect.Width, "", "")
		}
		return
	}
	// Three rows expose all required metadata without forcing an extremely
	// wide list. Keep the selected item inside the available card viewport.
	cards := max(0, (rect.Height-2)/3)
	start := 0
	if cards > 0 && selectedIndex >= cards {
		start = selectedIndex - cards + 1
	}
	start = min(start, max(0, len(items)-cards))
	for row := 2; row < rect.Height; row++ {
		cardIndex := (row - 2) / 3
		lineIndex := (row - 2) % 3
		index := start + cardIndex
		line, color := "", ""
		if index < len(items) && (cards > 0 || cardIndex == 0) {
			item := items[index]
			if item.Disconnected {
				color = "\x1b[2;37m"
			}
			prefix := "  "
			if index == selectedIndex {
				prefix, color = "› ", "\x1b[1;36m"
			}
			switch lineIndex {
			case 0:
				badge := ""
				if item.Unread {
					badge = " •"
				}
				line = prefix + item.Name + " @" + item.Host + badge
			case 1:
				projects := strings.Join(item.Projects, ", ")
				if projects == "" {
					projects = "Default Project"
				}
				line = "  " + projects + " · " + item.Type
			case 2:
				when := "never"
				if !item.LastNotification.IsZero() {
					when = item.LastNotification.Format("01-02 15:04")
				}
				writer := item.Writer
				if writer == "" {
					writer = "none"
				}
				line = fmt.Sprintf("  %s · %s · %s", item.State, writer, when)
			}
		}
		workspaceWrite(out, rect.X, rect.Y+row, rect.Width, line, color)
	}
}
