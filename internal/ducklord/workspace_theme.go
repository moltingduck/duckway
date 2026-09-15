package ducklord

import (
	"fmt"
	"strconv"
)

// WorkspaceTheme colors use #RRGGBB; empty values inherit the defaults.
type WorkspaceTheme struct {
	Separator       string `json:"separator,omitempty" yaml:"separator,omitempty"`
	Background      string `json:"background,omitempty" yaml:"background,omitempty"`
	Foreground      string `json:"foreground,omitempty" yaml:"foreground,omitempty"`
	FocusBackground string `json:"focus_background,omitempty" yaml:"focus_background,omitempty"`
	FocusForeground string `json:"focus_foreground,omitempty" yaml:"focus_foreground,omitempty"`
}

func DefaultWorkspaceTheme() WorkspaceTheme {
	return WorkspaceTheme{Separator: "#526071", Background: "#202833", Foreground: "#c5cfdb", FocusBackground: "#24536b", FocusForeground: "#ffffff"}
}

func workspaceColor(value string) (uint64, bool) {
	if len(value) != 7 || value[0] != '#' {
		return 0, false
	}
	for _, ch := range value[1:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') && (ch < 'A' || ch > 'F') {
			return 0, false
		}
	}
	n, err := strconv.ParseUint(value[1:], 16, 24)
	return n, err == nil
}

func (t WorkspaceTheme) Validate() error {
	for name, value := range map[string]string{"separator": t.Separator, "background": t.Background, "foreground": t.Foreground, "focus_background": t.FocusBackground, "focus_foreground": t.FocusForeground} {
		if value != "" {
			if _, ok := workspaceColor(value); !ok {
				return fmt.Errorf("workspace_theme.%s must be #RRGGBB", name)
			}
		}
	}
	return nil
}

func (t WorkspaceTheme) resolved() WorkspaceTheme {
	d := DefaultWorkspaceTheme()
	for _, pair := range [][2]*string{{&t.Separator, &d.Separator}, {&t.Background, &d.Background}, {&t.Foreground, &d.Foreground}, {&t.FocusBackground, &d.FocusBackground}, {&t.FocusForeground, &d.FocusForeground}} {
		if _, ok := workspaceColor(*pair[0]); !ok {
			*pair[0] = *pair[1]
		}
	}
	return t
}

func workspaceSGRColor(value string, background bool) string {
	n, ok := workspaceColor(value)
	if !ok {
		return ""
	}
	code := 38
	if background {
		code = 48
	}
	return fmt.Sprintf("\x1b[%d;2;%d;%d;%dm", code, n>>16, (n>>8)&255, n&255)
}

func (t WorkspaceTheme) style(focused bool) string {
	fg, bg := t.Foreground, t.Background
	if focused {
		fg, bg = t.FocusForeground, t.FocusBackground
	}
	return workspaceSGRColor(fg, false) + workspaceSGRColor(bg, true)
}
