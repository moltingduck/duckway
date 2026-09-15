package ducklord

import "testing"

func TestPanePrefixConfig(t *testing.T) {
	for _, key := range []string{"x", "enter", "ctrl-m", "ctrl-c", "ctrl-i", "ctrl-j", "ctrl-]"} {
		c := &Config{Shortcuts: map[string]string{"pane_prefix": key}}
		if err := c.normalize(); err == nil {
			t.Errorf("unsafe/conflicting prefix accepted: %s", key)
		}
	}
	c := &Config{Shortcuts: map[string]string{"pane_prefix": "ctrl-a"}}
	if err := c.normalize(); err != nil {
		t.Fatal(err)
	}
}
