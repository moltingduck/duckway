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
	Title string
	Lines []string // already VT-rendered and bounded by the source Terminal
	Stale bool
}

type WorkspaceRect struct{ X, Y, Width, Height int }

type WorkspaceGeometry struct {
	Projects WorkspaceRect
	Quick    WorkspaceRect
	Terminal WorkspaceRect
}

// CalculateWorkspaceGeometry reserves distinct Project, quick-list, and
// Terminal regions. Narrow terminals retain the Terminal region and hide
// columns in order, never producing negative pane dimensions.
func CalculateWorkspaceGeometry(width, height, top int) WorkspaceGeometry {
	if width < 1 || height < top {
		return WorkspaceGeometry{}
	}
	remaining := height - top + 1
	geometry := WorkspaceGeometry{Terminal: WorkspaceRect{X: 1, Y: top, Width: width, Height: remaining}}
	if width < 62 {
		return geometry
	}
	projectsWidth := 22
	if width < 90 {
		projectsWidth = 18
	}
	quickWidth := 30
	if width < 90 {
		quickWidth = 24
	}
	geometry.Projects = WorkspaceRect{X: 1, Y: top, Width: projectsWidth, Height: remaining}
	geometry.Quick = WorkspaceRect{X: projectsWidth + 2, Y: top, Width: quickWidth, Height: remaining}
	geometry.Terminal = WorkspaceRect{X: projectsWidth + quickWidth + 3, Y: top,
		Width: width - projectsWidth - quickWidth - 2, Height: remaining}
	return geometry
}

// RenderWorkspaceBody renders the Project pane, quick Session list pane, and
// one Project's tab/split tree. It is presentation-only: no PTY input, resize,
// unread mutation, or yield can occur here.
func RenderWorkspaceBody(out io.Writer, geometry WorkspaceGeometry, layout *ProjectLayout, nav *WorkspaceState,
	items []WorkspaceListItem, projectUnread func(string) bool, paneView func(SessionIdentity, int, int) WorkspacePaneView) {
	if out == nil || layout == nil || nav == nil {
		return
	}
	if geometry.Projects.Width > 0 {
		renderWorkspaceColumn(out, geometry.Projects, " PROJECTS ", func(index int) string {
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
			return prefix + project.Name + suffix
		})
	}
	if geometry.Quick.Width > 0 {
		renderWorkspaceColumn(out, geometry.Quick, " SESSIONS ", func(index int) string {
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
			return prefix + item.Name + " @" + item.Host + suffix
		})
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
		marker := " "
		if tab.ID == nav.CurrentTabID() {
			marker, active = "●", tab
		}
		tabs = append(tabs, fmt.Sprintf(" %s%d ", marker, i+1))
	}
	workspaceWrite(out, terminal.X, terminal.Y, terminal.Width, " "+project.Name+"  "+strings.Join(tabs, ""), "\x1b[1;36m")
	content := WorkspaceRect{X: terminal.X, Y: terminal.Y + 1, Width: terminal.Width, Height: terminal.Height - 1}
	if active == nil && len(project.Tabs) != 0 {
		active = &project.Tabs[0]
	}
	if active == nil || active.Root == nil {
		workspaceWrite(out, content.X, content.Y, content.Width, "No Session panes in this Project", "\x1b[2m")
		return
	}
	renderWorkspaceNode(out, active.Root, content, nav.CurrentPaneID(), paneView)
}

func renderWorkspaceColumn(out io.Writer, rect WorkspaceRect, title string, row func(int) string) {
	workspaceWrite(out, rect.X, rect.Y, rect.Width, title, "\x1b[1;34m")
	for i := 0; i < rect.Height-1; i++ {
		line := row(i)
		color := ""
		if strings.HasPrefix(line, "›") {
			color = "\x1b[1;36m"
		}
		workspaceWrite(out, rect.X, rect.Y+1+i, rect.Width, line, color)
	}
}

func renderWorkspaceNode(out io.Writer, node *SessionPane, rect WorkspaceRect, selectedPaneID string,
	view func(SessionIdentity, int, int) WorkspacePaneView) {
	if node == nil || rect.Width < 1 || rect.Height < 1 {
		return
	}
	if node.Session == nil {
		if node.Direction == SplitHorizontal {
			firstHeight := rect.Height / 2
			if firstHeight < 1 || rect.Height-firstHeight < 1 {
				renderWorkspaceNode(out, node.First, rect, selectedPaneID, view)
				return
			}
			renderWorkspaceNode(out, node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: rect.Width, Height: firstHeight}, selectedPaneID, view)
			renderWorkspaceNode(out, node.Second, WorkspaceRect{X: rect.X, Y: rect.Y + firstHeight, Width: rect.Width, Height: rect.Height - firstHeight}, selectedPaneID, view)
			return
		}
		firstWidth := rect.Width / 2
		if firstWidth < 1 || rect.Width-firstWidth < 1 {
			renderWorkspaceNode(out, node.First, rect, selectedPaneID, view)
			return
		}
		renderWorkspaceNode(out, node.First, WorkspaceRect{X: rect.X, Y: rect.Y, Width: firstWidth, Height: rect.Height}, selectedPaneID, view)
		renderWorkspaceNode(out, node.Second, WorkspaceRect{X: rect.X + firstWidth, Y: rect.Y, Width: rect.Width - firstWidth, Height: rect.Height}, selectedPaneID, view)
		return
	}
	data := WorkspacePaneView{Title: node.Session.SessionID, Stale: true}
	if view != nil {
		data = view(*node.Session, rect.Width, rect.Height-1)
	}
	if data.Title == "" {
		data.Title = node.Session.SessionID
	}
	marker, color := " ", "\x1b[2;37m"
	if node.ID == selectedPaneID {
		marker, color = "▣", "\x1b[1;36m"
	}
	if data.Stale {
		data.Title += " [stale]"
	}
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
	fmt.Fprintf(out, "\x1b[%d;%dH\x1b[0m%s%s\x1b[0m%s", y, x, color, line, strings.Repeat(" ", cells-workspaceCellWidth(line)))
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
