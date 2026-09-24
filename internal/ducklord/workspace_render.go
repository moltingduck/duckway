package ducklord

import (
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

type WorkspaceListItem struct {
	Identity SessionIdentity
	Name     string
	Host     string
	Unread   bool
	Selected bool
}

type WorkspacePaneView struct {
	Title    string
	Lines    []string // already VT-rendered and bounded by the source Terminal
	Stale    bool
	Focused  bool
	ReadOnly bool
}

type WorkspaceRect struct{ X, Y, Width, Height int }

type VisibleWorkspacePane struct {
	Identity SessionIdentity
	Rect     WorkspaceRect
	PaneID   string
}

type WorkspaceGeometry struct {
	Projects WorkspaceRect
	Quick    WorkspaceRect
	Terminal WorkspaceRect
}

type WorkspaceColumnOffsets struct {
	Projects int
	Quick    int
}

type WorkspaceFocus string

const (
	WorkspaceFocusProjects WorkspaceFocus = "projects"
	WorkspaceFocusSessions WorkspaceFocus = "sessions"
	WorkspaceFocusTerminal WorkspaceFocus = "terminal"
)

type WorkspaceRenderOptions struct {
	Offsets      WorkspaceColumnOffsets
	Focus        WorkspaceFocus
	Theme        WorkspaceTheme
	Notes        []NoteEntry
	NoteIndex    int
	NoteOffset   int
	NoteScope    NotesScope
	NoteQuery    string
	ProjectTotal int
	SessionTotal int
}

// WorkspaceScrollbar describes the proportional thumb inside a list's row area.
// TrackX is the final pane column and TrackY starts below the title row.
type WorkspaceScrollbar struct{ TrackX, TrackY, TrackHeight, ThumbY, ThumbHeight int }

func CalculateWorkspaceScrollbar(rect WorkspaceRect, total, offset int) (WorkspaceScrollbar, bool) {
	visible := rect.Height - 1
	if rect.Width < 2 || visible < 1 || total <= visible {
		return WorkspaceScrollbar{}, false
	}
	maxOffset := total - visible
	offset = min(max(0, offset), maxOffset)
	thumb := max(1, visible*visible/total)
	start := (visible - thumb) * offset / maxOffset
	return WorkspaceScrollbar{TrackX: rect.X + rect.Width - 1, TrackY: rect.Y + 1, TrackHeight: visible, ThumbY: rect.Y + 1 + start, ThumbHeight: thumb}, true
}

// WorkspaceListOffset keeps the selected row visible while preserving the
// current scroll position whenever possible. It also clamps after a resize
// or list shrink.
func WorkspaceListOffset(offset, selected, visible, total int) int {
	if visible <= 0 || total <= visible {
		return 0
	}
	if offset < 0 {
		offset = 0
	}
	if offset > total-visible {
		offset = total - visible
	}
	if selected >= 0 && selected < total {
		if selected < offset {
			offset = selected
		} else if selected >= offset+visible {
			offset = selected - visible + 1
		}
	}
	return offset
}

// CalculateWorkspaceGeometry stacks Projects above Sessions in a left sidebar.
// Narrow or short terminals retain only the Terminal region.
func CalculateWorkspaceGeometry(width, height, top int) WorkspaceGeometry {
	if top < 1 {
		top = 1
	}
	if width < 1 || height < top {
		return WorkspaceGeometry{}
	}
	remaining := height - top + 1
	geometry := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: top, Width: width, Height: remaining}}
	if width < 62 || remaining < 5 {
		return geometry
	}
	sidebarWidth := 30
	if width < 90 {
		sidebarWidth = 24
	}
	projectsHeight := remaining / 3
	if projectsHeight < 2 {
		projectsHeight = 2
	}
	geometry.Projects = WorkspaceRect{X: 1, Y: top, Width: sidebarWidth, Height: projectsHeight}
	geometry.Quick = WorkspaceRect{X: 1, Y: top + projectsHeight, Width: sidebarWidth, Height: remaining - projectsHeight}
	geometry.Terminal = WorkspaceRect{X: sidebarWidth + 2, Y: top,
		Width: width - sidebarWidth - 1, Height: remaining}
	return geometry
}

// WorkspaceScrollbarOffset maps the thumb's top row to its viewport offset.
func WorkspaceScrollbarOffset(bar WorkspaceScrollbar, total, thumbTopY int) int {
	visible := bar.TrackHeight
	maxOffset := max(0, total-visible)
	travel := max(0, visible-bar.ThumbHeight)
	if maxOffset == 0 || travel == 0 {
		return 0
	}
	start := min(max(0, thumbTopY-bar.TrackY), travel)
	return start * maxOffset / travel
}

// RenderWorkspaceBody renders the Project pane, quick Session list pane, and
// one Project's tab/split tree. It is presentation-only: no PTY input, resize,
// unread mutation, or yield can occur here.
func RenderWorkspaceBody(out io.Writer, geometry WorkspaceGeometry, layout *ProjectLayout, nav *WorkspaceState,
	items []WorkspaceListItem, projectUnread func(string) bool, paneView func(SessionIdentity, int, int) WorkspacePaneView, columnOffsets ...WorkspaceColumnOffsets) {
	if out == nil || layout == nil || nav == nil {
		return
	}
	offsets := WorkspaceColumnOffsets{}
	if len(columnOffsets) > 0 {
		offsets = columnOffsets[0]
	}
	RenderWorkspaceBodyWithOptions(out, geometry, layout, nav, items, projectUnread, paneView, WorkspaceRenderOptions{Offsets: offsets})
}

// RenderWorkspaceBodyWithOptions adds configurable colors and navigation focus
// without changing the PTY viewport or its one-row title offset.
func RenderWorkspaceBodyWithOptions(out io.Writer, geometry WorkspaceGeometry, layout *ProjectLayout, nav *WorkspaceState,
	items []WorkspaceListItem, projectUnread func(string) bool, paneView func(SessionIdentity, int, int) WorkspacePaneView, options WorkspaceRenderOptions) {
	if out == nil || layout == nil || nav == nil {
		return
	}
	offsets := options.Offsets
	options.Theme = options.Theme.resolved()
	if geometry.Projects.Width > 0 && geometry.Terminal.X == geometry.Projects.X+geometry.Projects.Width+1 {
		for y := geometry.Terminal.Y; y < geometry.Terminal.Y+geometry.Terminal.Height; y++ {
			workspaceWrite(out, geometry.Terminal.X-1, y, 1, "│", workspaceSGRColor(options.Theme.Separator, false))
		}
	}
	if geometry.Projects.Width > 0 {
		renderWorkspaceColumn(out, geometry.Projects, " PROJECTS ", options.Theme, options.Focus == WorkspaceFocusProjects, func(index int) string {
			index += offsets.Projects
			if index >= len(layout.Projects) {
				return ""
			}
			project := layout.Projects[index]
			prefix, suffix := "  ", ""
			if project.ID == nav.CurrentProjectID() {
				prefix = "› "
			}
			if projectUnread != nil && projectUnread(project.ID) {
				suffix = " •"
			}
			if project.ID == nav.NotificationFocusProjectID() {
				suffix += " ◎"
			}
			rowWidth := geometry.Projects.Width
			if _, overflow := CalculateWorkspaceScrollbar(geometry.Projects, options.ProjectTotal, offsets.Projects); overflow {
				rowWidth--
			}
			return workspaceMarkedRow(prefix, project.Name, suffix, rowWidth)
		}, offsets.Projects, options.ProjectTotal)
	}
	if geometry.Quick.Width > 0 {
		renderWorkspaceColumn(out, geometry.Quick, " SESSIONS ", options.Theme, options.Focus == WorkspaceFocusSessions, func(index int) string {
			index += offsets.Quick
			if index >= len(items) {
				return ""
			}
			item := items[index]
			prefix, suffix := "  ", ""
			if item.Selected {
				prefix = "› "
			}
			if item.Unread {
				suffix = " •"
			}
			rowWidth := geometry.Quick.Width
			if _, overflow := CalculateWorkspaceScrollbar(geometry.Quick, options.SessionTotal, offsets.Quick); overflow {
				rowWidth--
			}
			return workspaceMarkedRow(prefix, item.Name+" @"+item.Host, suffix, rowWidth)
		}, offsets.Quick, options.SessionTotal)
	}
	terminal := geometry.Terminal
	if terminal.Width <= 0 || terminal.Height <= 0 {
		return
	}
	project := layout.Project(nav.CurrentProjectID())
	if project == nil {
		workspaceWrite(out, terminal.X, terminal.Y, terminal.Width, "Project unavailable", "\x1b[31m")
		return
	}
	var tabs []string
	var active *TerminalTab
	for i := range project.Tabs {
		tab := &project.Tabs[i]
		if tab.ID == nav.CurrentTabID() {
			active = tab
		}
		tabs = append(tabs, WorkspaceTabLabel(*tab, i, tab.ID == nav.CurrentTabID()))
	}
	tabs = append(tabs, " [+] ")
	content := WorkspaceRect{X: terminal.X, Y: terminal.Y + 1, Width: terminal.Width, Height: terminal.Height - 1}
	if active == nil && len(project.Tabs) != 0 {
		active = &project.Tabs[0]
	}
	if active == nil || active.Root == nil {
		workspaceWrite(out, terminal.X, terminal.Y, terminal.Width, " "+project.Name+"  "+strings.Join(tabs, ""), options.Theme.style(options.Focus == WorkspaceFocusTerminal))
		if content.Height > 0 {
			workspaceWrite(out, content.X, content.Y, content.Width, "No Session panes in this Project", "\x1b[2m")
		}
		return
	}
	_, hidden := workspaceVisibleLeaves(active.Root, content)
	heading := " " + project.Name + "  " + strings.Join(tabs, "")
	if hidden > 0 {
		heading += fmt.Sprintf(" +%d hidden", hidden)
	}
	workspaceWrite(out, terminal.X, terminal.Y, terminal.Width, heading, options.Theme.style(options.Focus == WorkspaceFocusTerminal))
	renderWorkspaceNode(out, active.Root, content, nav.CurrentPaneID(), paneView, options)
}

// WorkspaceTabLabel is shared by rendering and mouse hit testing.
func WorkspaceTabLabel(tab TerminalTab, index int, active bool) string {
	marker := " "
	if active {
		marker = "●"
	}
	name := tab.Name
	if name == "" {
		name = fmt.Sprint(index + 1)
	}
	return " " + marker + name + " "
}

// WorkspaceVisibleSessions uses the same split geometry as the renderer, so
// a suppressed leaf cannot consume a raw-output subscription or receive focus.
func WorkspaceVisibleSessions(layout *ProjectLayout, nav *WorkspaceState, geometry WorkspaceGeometry) []SessionIdentity {
	panes := WorkspaceVisiblePaneRects(layout, nav, geometry)
	sessions := make([]SessionIdentity, 0, len(panes))
	for _, pane := range panes {
		sessions = append(sessions, pane.Identity)
	}
	return sessions
}

// WorkspaceVisiblePaneRects is the subscription set and size for the exact
// tab/split cells drawn by RenderWorkspaceBody. Hidden branches are absent.
func WorkspaceVisiblePaneRects(layout *ProjectLayout, nav *WorkspaceState, geometry WorkspaceGeometry) []VisibleWorkspacePane {
	if layout == nil || nav == nil {
		return nil
	}
	project := layout.Project(nav.CurrentProjectID())
	if project == nil {
		return nil
	}
	content := geometry.Terminal
	content.Y++
	content.Height--
	for _, tab := range project.Tabs {
		if tab.ID == nav.CurrentTabID() {
			return workspaceCollectVisiblePanes(tab.Root, content)
		}
	}
	return nil
}

func workspaceCollectVisiblePanes(node *SessionPane, rect WorkspaceRect) []VisibleWorkspacePane {
	if node == nil || rect.Width < 1 || rect.Height < 1 {
		return nil
	}
	if node.Session != nil {
		return []VisibleWorkspacePane{{Identity: *node.Session, Rect: rect, PaneID: node.ID}}
	}
	if node.Note {
		return nil
	}
	if node.Direction == SplitHorizontal {
		firstHeight := rect.Height / 2
		if firstHeight < 1 || rect.Height-firstHeight < 1 {
			return workspaceCollectVisiblePanes(node.First, rect)
		}
		first := workspaceCollectVisiblePanes(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: firstHeight})
		second := workspaceCollectVisiblePanes(node.Second, WorkspaceRect{X: rect.X, Y: rect.Y + firstHeight, Width: rect.Width, Height: rect.Height - firstHeight})
		return append(first, second...)
	}
	firstWidth := rect.Width / 2
	if firstWidth < 1 || rect.Width-firstWidth < 1 {
		return workspaceCollectVisiblePanes(node.First, rect)
	}
	first := workspaceCollectVisiblePanes(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: firstWidth, Height: rect.Height})
	second := workspaceCollectVisiblePanes(node.Second, WorkspaceRect{X: rect.X + firstWidth, Y: rect.Y, Width: rect.Width - firstWidth, Height: rect.Height})
	return append(first, second...)
}

// WorkspaceVisibleLeafRects includes Notes leaves for hit testing. Notes are
// deliberately absent from WorkspaceVisiblePaneRects, which is the PTY set.
func WorkspaceVisibleLeafRects(layout *ProjectLayout, nav *WorkspaceState, geometry WorkspaceGeometry) []VisibleWorkspacePane {
	if layout == nil || nav == nil {
		return nil
	}
	p := layout.Project(nav.CurrentProjectID())
	if p == nil {
		return nil
	}
	r := geometry.Terminal
	r.Y++
	r.Height--
	for _, tab := range p.Tabs {
		if tab.ID == nav.CurrentTabID() {
			return workspaceCollectLeaves(tab.Root, r)
		}
	}
	return nil
}
func workspaceCollectLeaves(n *SessionPane, r WorkspaceRect) []VisibleWorkspacePane {
	if n == nil || r.Width < 1 || r.Height < 1 {
		return nil
	}
	if n.Session != nil || n.Note {
		return []VisibleWorkspacePane{{Identity: func() SessionIdentity {
			if n.Session != nil {
				return *n.Session
			}
			return SessionIdentity{}
		}(), Rect: r, PaneID: n.ID}}
	}
	if n.Direction == SplitHorizontal {
		h := r.Height / 2
		if h < 1 || r.Height-h < 1 {
			return workspaceCollectLeaves(n.First, r)
		}
		return append(workspaceCollectLeaves(n.First, WorkspaceRect{r.X, r.Y, r.Width, h}), workspaceCollectLeaves(n.Second, WorkspaceRect{r.X, r.Y + h, r.Width, r.Height - h})...)
	}
	w := r.Width / 2
	if w < 1 || r.Width-w < 1 {
		return workspaceCollectLeaves(n.First, r)
	}
	return append(workspaceCollectLeaves(n.First, WorkspaceRect{r.X, r.Y, w, r.Height}), workspaceCollectLeaves(n.Second, WorkspaceRect{r.X + w, r.Y, r.Width - w, r.Height})...)
}

// WorkspaceVisiblePaneRect returns the selected leaf's actual screen cell.
// The PTY viewport starts one row below that leaf's title, not at the top of
// the overall Terminal area.
func WorkspaceVisiblePaneRect(layout *ProjectLayout, nav *WorkspaceState, geometry WorkspaceGeometry) (WorkspaceRect, bool) {
	if layout == nil || nav == nil {
		return WorkspaceRect{}, false
	}
	project := layout.Project(nav.CurrentProjectID())
	if project == nil {
		return WorkspaceRect{}, false
	}
	content := geometry.Terminal
	content.Y++
	content.Height--
	for _, tab := range project.Tabs {
		if tab.ID == nav.CurrentTabID() {
			return workspaceFindVisiblePane(tab.Root, content, nav.CurrentPaneID())
		}
	}
	return WorkspaceRect{}, false
}

func workspaceFindVisiblePane(node *SessionPane, rect WorkspaceRect, paneID string) (WorkspaceRect, bool) {
	if node == nil || rect.Width < 1 || rect.Height < 2 {
		return WorkspaceRect{}, false
	}
	if node.Session != nil {
		return rect, node.ID == paneID
	}
	if node.Note {
		return rect, node.ID == paneID
	}
	if node.Direction == SplitHorizontal {
		firstHeight := rect.Height / 2
		if firstHeight < 1 || rect.Height-firstHeight < 1 {
			return workspaceFindVisiblePane(node.First, rect, paneID)
		}
		if found, ok := workspaceFindVisiblePane(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: firstHeight}, paneID); ok {
			return found, true
		}
		return workspaceFindVisiblePane(node.Second, WorkspaceRect{X: rect.X, Y: rect.Y + firstHeight, Width: rect.Width, Height: rect.Height - firstHeight}, paneID)
	}
	firstWidth := rect.Width / 2
	if firstWidth < 1 || rect.Width-firstWidth < 1 {
		return workspaceFindVisiblePane(node.First, rect, paneID)
	}
	if found, ok := workspaceFindVisiblePane(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: firstWidth, Height: rect.Height}, paneID); ok {
		return found, true
	}
	return workspaceFindVisiblePane(node.Second, WorkspaceRect{X: rect.X + firstWidth, Y: rect.Y, Width: rect.Width - firstWidth, Height: rect.Height}, paneID)
}

func workspaceVisibleLeaves(node *SessionPane, rect WorkspaceRect) ([]SessionIdentity, int) {
	if node == nil {
		return nil, 0
	}
	if rect.Width < 1 || rect.Height < 1 {
		return nil, len(node.sessions())
	}
	if node.Session != nil {
		return []SessionIdentity{*node.Session}, 0
	}
	if node.Note {
		return nil, 0
	}
	if node.Direction == SplitHorizontal {
		firstHeight := rect.Height / 2
		if firstHeight < 1 || rect.Height-firstHeight < 1 {
			leaves, hidden := workspaceVisibleLeaves(node.First, rect)
			return leaves, hidden + len(node.Second.sessions())
		}
		first, firstHidden := workspaceVisibleLeaves(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: firstHeight})
		second, secondHidden := workspaceVisibleLeaves(node.Second, WorkspaceRect{X: rect.X, Y: rect.Y + firstHeight, Width: rect.Width, Height: rect.Height - firstHeight})
		return append(first, second...), firstHidden + secondHidden
	}
	firstWidth := rect.Width / 2
	if firstWidth < 1 || rect.Width-firstWidth < 1 {
		leaves, hidden := workspaceVisibleLeaves(node.First, rect)
		return leaves, hidden + len(node.Second.sessions())
	}
	first, firstHidden := workspaceVisibleLeaves(node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: firstWidth, Height: rect.Height})
	second, secondHidden := workspaceVisibleLeaves(node.Second, WorkspaceRect{X: rect.X + firstWidth, Y: rect.Y, Width: rect.Width - firstWidth, Height: rect.Height})
	return append(first, second...), firstHidden + secondHidden
}

func renderWorkspaceColumn(out io.Writer, rect WorkspaceRect, title string, theme WorkspaceTheme, focused bool, row func(int) string, offset, total int) {
	if rect.Height <= 0 {
		return
	}
	workspaceWrite(out, rect.X, rect.Y, rect.Width, title, "\x1b[1m"+theme.style(focused))
	for i := 0; i < rect.Height-1; i++ {
		line := row(i)
		color := theme.style(false)
		if strings.HasPrefix(line, "›") {
			color = "\x1b[1m" + theme.style(focused)
		}
		workspaceWrite(out, rect.X, rect.Y+1+i, rect.Width, line, color)
	}
	if bar, ok := CalculateWorkspaceScrollbar(rect, total, offset); ok {
		for i := 0; i < bar.TrackHeight; i++ {
			glyph, style := "│", "\x1b[38;5;240m"
			if i >= bar.ThumbY-bar.TrackY && i < bar.ThumbY-bar.TrackY+bar.ThumbHeight {
				glyph, style = "█", "\x1b[38;5;81m"
			}
			workspaceWrite(out, bar.TrackX, bar.TrackY+i, 1, glyph, style)
		}
	}
}

// Keep unread/focus markers visible even when a Project or Session label is
// wider than its column. Cropping the whole row would silently hide them.
func workspaceMarkedRow(prefix, label, suffix string, cells int) string {
	available := cells - workspaceCellWidth(prefix) - workspaceCellWidth(suffix)
	if available < 0 {
		return workspaceTruncate(prefix+suffix, cells)
	}
	return prefix + workspaceTruncate(label, available) + suffix
}

func renderWorkspaceNode(out io.Writer, node *SessionPane, rect WorkspaceRect, selectedPaneID string,
	view func(SessionIdentity, int, int) WorkspacePaneView, options WorkspaceRenderOptions) {
	if node == nil || rect.Width < 1 || rect.Height < 1 {
		return
	}
	if node.Note {
		scope := string(options.NoteScope)
		if scope == "" {
			scope = "project"
		}
		header := fmt.Sprintf(" ╔ NOTES · %s book ╗ ", strings.ToUpper(scope))
		workspaceWrite(out, rect.X, rect.Y, rect.Width, header, "\x1b[1;33m")
		// A one-row pane can only show its heading. Keep the minimal state
		// readable instead of letting the footer overwrite it.
		if rect.Height == 1 {
			return
		}
		for y := 1; y < rect.Height; y++ {
			workspaceWrite(out, rect.X, rect.Y+y, rect.Width, "", "")
		}
		visible := max(0, rect.Height-3)
		start := max(0, min(options.NoteOffset, max(0, len(options.Notes)-visible)))
		for i, n := range options.Notes[start:] {
			y := rect.Y + 1 + i
			if y >= rect.Y+rect.Height-1 {
				break
			}
			prefix := " "
			styleRow := ""
			if i+start == options.NoteIndex {
				prefix, styleRow = ">", "\x1b[7m"
			}
			workspaceWrite(out, rect.X, y, rect.Width, fmt.Sprintf("%s %s — %s", prefix, n.Title, n.Preview), styleRow)
		}
		page, pages := 0, 1
		if len(options.Notes) > 0 {
			page = 1
			if visible > 0 {
				page = start/visible + 1
			}
		}
		if visible > 0 && len(options.Notes) > 0 {
			pages = (len(options.Notes) + visible - 1) / visible
			if pages < 1 {
				pages = 1
			}
		}
		footer := fmt.Sprintf(" a add · e edit · E all · Enter copy body to system clipboard · j/k select · h/l page · g/p/s scope · / search · %d/%d ", page, pages)
		if options.NoteQuery != "" {
			footer += " · /" + options.NoteQuery
		}
		workspaceWrite(out, rect.X, rect.Y+rect.Height-1, rect.Width, footer, "\x1b[2;33m")
		return
	}
	if node.Session == nil {
		if node.Direction == SplitHorizontal {
			firstHeight := rect.Height / 2
			if firstHeight < 1 || rect.Height-firstHeight < 1 {
				renderWorkspaceNode(out, node.First, rect, selectedPaneID, view, options)
				return
			}
			renderWorkspaceNode(out, node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: firstHeight}, selectedPaneID, view, options)
			renderWorkspaceNode(out, node.Second, WorkspaceRect{X: rect.X, Y: rect.Y + firstHeight, Width: rect.Width, Height: rect.Height - firstHeight}, selectedPaneID, view, options)
			return
		}
		firstWidth := rect.Width / 2
		if firstWidth < 1 || rect.Width-firstWidth < 1 {
			renderWorkspaceNode(out, node.First, rect, selectedPaneID, view, options)
			return
		}
		renderWorkspaceNode(out, node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: firstWidth, Height: rect.Height}, selectedPaneID, view, options)
		renderWorkspaceNode(out, node.Second, WorkspaceRect{X: rect.X + firstWidth, Y: rect.Y, Width: rect.Width - firstWidth, Height: rect.Height}, selectedPaneID, view, options)
		return
	}
	data := WorkspacePaneView{Title: node.Session.SessionID, Stale: true}
	if view != nil {
		data = view(*node.Session, rect.Width, rect.Height-1)
	}
	if data.Title == "" {
		data.Title = node.Session.SessionID
	}
	marker := " "
	if data.ReadOnly {
		marker = "◌"
	}
	if node.ID == selectedPaneID {
		marker = "◇"
		if data.ReadOnly {
			marker = "◌"
		} else if data.Focused {
			marker = "▣"
		}
	}
	if data.Stale {
		data.Title += " [stale]"
	}
	color := options.Theme.style(node.ID == selectedPaneID && options.Focus == WorkspaceFocusTerminal)
	workspaceWrite(out, rect.X, rect.Y, rect.Width, marker+" "+data.Title, color)
	for row := 1; row < rect.Height; row++ {
		line := ""
		if row-1 < len(data.Lines) {
			line = data.Lines[row-1]
		}
		workspaceWriteVT(out, rect.X, rect.Y+row, rect.Width, line)
	}
}

func workspaceWrite(out io.Writer, x, y, cells int, line, color string) {
	if cells <= 0 || x <= 0 || y <= 0 {
		return
	}
	line = workspaceTruncate(line, cells)
	fmt.Fprintf(out, "\x1b[%d;%dH\x1b[0m%s%s%s\x1b[0m", y, x, color, line, strings.Repeat(" ", cells-workspaceCellWidth(line)))
}

// Only SGR styling from a trusted Terminal renderer is carried into a Session
// pane. Cursor movement, OSC, and other control sequences can never escape a
// split cell or overwrite neighboring panes.
func workspaceWriteVT(out io.Writer, x, y, cells int, line string) {
	if cells <= 0 || x <= 0 || y <= 0 {
		return
	}
	var b strings.Builder
	used := 0
	for len(line) > 0 {
		if line[0] == '\x1b' {
			if len(line) >= 2 && line[1] == '[' {
				end := 2
				for end < len(line) && end < 64 && (line[end] < 0x40 || line[end] > 0x7e) {
					end++
				}
				if end < len(line) && line[end] == 'm' && safeWorkspaceSGR(line[2:end]) {
					b.WriteString(line[:end+1])
				}
				if end < len(line) {
					line = line[end+1:]
				} else {
					line = ""
				}
				continue
			}
			if len(line) >= 2 && line[1] == ']' {
				end := 2
				for end < len(line) && line[end] != '\a' && (line[end] != '\x1b' || end+1 >= len(line) || line[end+1] != '\\') {
					end++
				}
				if end >= len(line) {
					line = ""
				} else if line[end] == '\a' {
					line = line[end+1:]
				} else {
					line = line[end+2:]
				}
				continue
			}
			if len(line) >= 2 {
				line = line[2:]
			} else {
				line = ""
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(line)
		line = line[size:]
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		w := workspaceRuneWidth(r)
		if used+w > cells {
			break
		}
		b.WriteRune(r)
		used += w
	}
	fmt.Fprintf(out, "\x1b[%d;%dH\x1b[0m%s\x1b[0m%s", y, x, b.String(), strings.Repeat(" ", cells-used))
}

func safeWorkspaceSGR(params string) bool {
	for _, r := range params {
		if r < '0' || r > '9' {
			if r != ';' {
				return false
			}
		}
	}
	return true
}

func workspaceCellWidth(text string) int {
	used := 0
	for _, r := range text {
		used += workspaceRuneWidth(r)
	}
	return used
}

func workspaceRuneWidth(r rune) int {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return 0
	}
	if kind := width.LookupRune(r).Kind(); kind == width.EastAsianWide || kind == width.EastAsianFullwidth {
		return 2
	}
	return 1
}

func workspaceTruncate(text string, cells int) string {
	if cells <= 0 {
		return ""
	}
	used := 0
	var b strings.Builder
	for _, r := range text {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		w := workspaceRuneWidth(r)
		if used+w > cells {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
}
