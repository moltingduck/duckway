package ducklord

import (
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
	if top < 1 {
		top = 1
	}
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
	focus := WorkspaceFocusSessions
	if focused {
		focus = WorkspaceFocusTerminal
	}
	RenderDetailedSessionBodyWithOptions(out, geometry, items, selected, query, filter, focused, paneView, WorkspaceRenderOptions{Focus: focus})
}

func RenderDetailedSessionBodyWithOptions(out io.Writer, geometry DetailGeometry, items []DetailedSessionItem,
	selected SessionIdentity, query string, filter DetailFilter, focused bool,
	paneView func(SessionIdentity, int, int) WorkspacePaneView, options WorkspaceRenderOptions) {
	if out == nil {
		return
	}
	options.Theme = options.Theme.resolved()
	if geometry.List.Width > 0 && geometry.Pane.X == geometry.List.X+geometry.List.Width+1 {
		for y := geometry.Pane.Y; y < geometry.Pane.Y+geometry.Pane.Height; y++ {
			workspaceWrite(out, geometry.Pane.X-1, y, 1, "│", workspaceSGRColor(options.Theme.Separator, false))
		}
	}
	selectedIndex := -1
	for i := range items {
		if items[i].Identity == selected {
			selectedIndex = i
			break
		}
	}
	if geometry.List.Width > 0 && geometry.List.Height > 0 {
		renderDetailList(out, geometry.List, items, selectedIndex, query, filter, options)
	}
	if geometry.Pane.Width < 1 || geometry.Pane.Height < 1 {
		return
	}
	if selectedIndex < 0 {
		workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y, geometry.Pane.Width, " SESSION PREVIEW ", options.Theme.style(options.Focus == WorkspaceFocusTerminal))
		message := "No matching Sessions"
		if len(items) != 0 {
			message = "Select a Session to preview"
		}
		if geometry.Pane.Height > 1 {
			workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y+1, geometry.Pane.Width, message, "\x1b[2m")
		}
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
	marker := "◇"
	if data.ReadOnly {
		marker = "◌"
	} else if focused {
		marker = "▣"
	}
	if data.Stale {
		data.Title += " [disconnected/stale]"
	}
	workspaceWrite(out, geometry.Pane.X, geometry.Pane.Y, geometry.Pane.Width, marker+" "+data.Title, options.Theme.style(options.Focus == WorkspaceFocusTerminal))
	for row := 1; row < geometry.Pane.Height; row++ {
		line := ""
		if row-1 < len(data.Lines) {
			line = data.Lines[row-1]
		}
		workspaceWriteVT(out, geometry.Pane.X, geometry.Pane.Y+row, geometry.Pane.Width, line)
	}
}

func renderDetailList(out io.Writer, rect WorkspaceRect, items []DetailedSessionItem, selectedIndex int, query string, filter DetailFilter, renderOptions ...WorkspaceRenderOptions) {
	options := WorkspaceRenderOptions{Focus: WorkspaceFocusSessions}
	if len(renderOptions) > 0 {
		options = renderOptions[0]
	}
	options.Theme = options.Theme.resolved()
	focused := options.Focus == WorkspaceFocusSessions
	workspaceWrite(out, rect.X, rect.Y, rect.Width, " SESSIONS · "+string(filter), "\x1b[1m"+options.Theme.style(focused))
	if rect.Height < 2 {
		return
	}
	workspaceWrite(out, rect.X, rect.Y+1, rect.Width, " find › "+query, options.Theme.style(false))
	if len(items) == 0 && rect.Height >= 3 {
		workspaceWrite(out, rect.X, rect.Y+2, rect.Width, " No matching Sessions", options.Theme.style(false))
		for row := 3; row < rect.Height; row++ {
			workspaceWrite(out, rect.X, rect.Y+row, rect.Width, "", options.Theme.style(false))
		}
		return
	}
	// Narrow lists put notification time on its own line so the state and
	// writer remain readable. Keep the selected card inside the viewport.
	cardHeight, start := detailCardViewport(rect, selectedIndex, len(items))
	for row := 2; row < rect.Height; row++ {
		cardIndex := (row - 2) / cardHeight
		lineIndex := (row - 2) % cardHeight
		index := start + cardIndex
		line, color := "", options.Theme.style(false)
		if index < len(items) {
			item := items[index]
			if item.Disconnected {
				color += "\x1b[2;37m"
			}
			prefix := "  "
			if index == selectedIndex {
				prefix = "› "
				if !item.Disconnected {
					color = "\x1b[1m" + options.Theme.style(focused)
				}
			}
			line = detailRowLine(item, lineIndex, rect.Width, prefix)
		}
		workspaceWrite(out, rect.X, rect.Y+row, rect.Width, line, color)
	}
}

func detailCardViewport(rect WorkspaceRect, selectedIndex, itemCount int) (cardHeight, start int) {
	cardHeight = 3
	if rect.Width < 40 {
		cardHeight = 4
	}
	// Keep a partial name row visible when the viewport cannot fit a card.
	cards := max(1, (rect.Height-2)/cardHeight)
	if selectedIndex >= cards {
		start = selectedIndex - cards + 1
	}
	start = min(start, max(0, itemCount-cards))
	return cardHeight, start
}

// DetailSessionIndexAt returns the exact rendered card under a screen cell.
// Headings, search rows, empty rows, and cells outside the list return -1.
func DetailSessionIndexAt(rect WorkspaceRect, selectedIndex, itemCount, x, y int) int {
	if itemCount <= 0 || rect.Width <= 0 || rect.Height <= 2 || x < rect.X || x >= rect.X+rect.Width || y < rect.Y+2 || y >= rect.Y+rect.Height {
		return -1
	}
	cardHeight, start := detailCardViewport(rect, selectedIndex, itemCount)
	index := start + (y-rect.Y-2)/cardHeight
	if index >= itemCount {
		return -1
	}
	return index
}

func detailRowLine(item DetailedSessionItem, row, cells int, prefix string) string {
	available := max(0, cells-2)
	switch row {
	case 0:
		badge := ""
		if item.Unread {
			badge = " •"
		}
		// Split the remaining space between Session and Host, giving either
		// field unused space when the other is short.
		budget := max(0, available-2-workspaceCellWidth(badge))
		hostWidth := min(workspaceCellWidth(item.Host), budget/2)
		nameWidth := min(workspaceCellWidth(item.Name), budget-hostWidth)
		hostWidth = budget - nameWidth
		return prefix + detailField(item.Name, nameWidth) + " @" + detailField(item.Host, hostWidth) + badge
	case 1:
		projects := strings.Join(item.Projects, ", ")
		if projects == "" {
			projects = "Default Project"
		}
		sessionType := detailField(item.Type, available-5)
		return "  " + detailField(projects, available-3-workspaceCellWidth(sessionType)) + " · " + sessionType
	case 2:
		writer := item.Writer
		if writer == "" {
			writer = "none"
		}
		suffix := ""
		if cells >= 40 {
			suffix = " · " + detailNotificationTime(item)
		}
		state := detailField(item.State, 12) // includes the full disconnected label
		return "  " + state + " · " + detailField(writer, available-workspaceCellWidth(state)-3-workspaceCellWidth(suffix)) + suffix
	case 3:
		return "  " + detailNotificationTime(item)
	default:
		return ""
	}
}

func detailNotificationTime(item DetailedSessionItem) string {
	if item.LastNotification.IsZero() {
		return "never"
	}
	return item.LastNotification.Format("01-02 15:04")
}

// Truncate display fields in terminal cells, including an explicit ellipsis
// so a shortened identifier cannot be mistaken for its full value.
func detailField(value string, cells int) string {
	if cells <= 0 {
		return ""
	}
	value = workspaceTruncate(value, workspaceCellWidth(value))
	if workspaceCellWidth(value) <= cells {
		return value
	}
	return workspaceTruncate(value, cells-1) + "…"
}
