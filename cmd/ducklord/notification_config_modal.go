package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/hackerduck/duckway/internal/ducklord"
)

var notificationConfigClasses = []ducklord.NotificationClass{
	ducklord.NotificationCompleted, ducklord.NotificationFailed,
	ducklord.NotificationActionNeeded, ducklord.NotificationAttention,
}

var notificationConfigLevels = []ducklord.NotificationLevel{
	ducklord.NotificationOff, ducklord.NotificationIndicator,
	ducklord.NotificationSound, ducklord.NotificationSystem,
}

func (s *tuiState) beginNotificationConfig(scope, host string) {
	if s.cfgPath == "" {
		s.outputErr = "notification defaults require a Ducklord config file"
		return
	}
	if scope != "host" && scope != "global" {
		s.outputErr = "invalid notification settings scope"
		return
	}
	base, err := os.ReadFile(s.cfgPath)
	if err != nil {
		s.outputErr = "cannot read Ducklord config: " + sanitizeTerminalText(err.Error())
		return
	}
	draft, err := ducklord.LoadConfig(s.cfgPath)
	if err != nil {
		s.outputErr = "cannot load Ducklord config: " + sanitizeTerminalText(err.Error())
		return
	}
	checked, err := os.ReadFile(s.cfgPath)
	if err != nil || !bytes.Equal(base, checked) {
		s.outputErr = "config changed while opening settings; retry"
		return
	}
	if scope == "host" {
		if _, ok := draft.Client(host); !ok {
			s.outputErr = "Host was removed; reopen Host actions"
			return
		}
	}
	s.notificationConfigMode, s.notificationConfigStep = true, "list"
	s.notificationConfigScope, s.notificationConfigHost = scope, host
	s.notificationConfigIndex, s.notificationConfigChoice = 0, 0
	s.notificationConfigPath, s.notificationConfigErr = "", ""
	s.notificationConfigDraft, s.notificationConfigBase = draft, base
}

func (s *tuiState) closeNotificationConfig() {
	s.notificationConfigMode, s.notificationConfigStep = false, ""
	s.notificationConfigDraft, s.notificationConfigBase = nil, nil
	s.notificationConfigErr, s.notificationConfigPath = "", ""
}

func (s *tuiState) notificationConfigRows() []string {
	rows := make([]string, 0, 9)
	for _, class := range notificationConfigClasses {
		value := "inherit global"
		if s.notificationConfigScope == "global" {
			value = string(ducklord.NotificationIndicator)
			if configured := s.notificationConfigDraft.NotificationLevels[class]; configured != "" {
				value = string(configured)
			}
		} else if host, ok := s.notificationConfigDraft.Client(s.notificationConfigHost); ok {
			if configured := host.NotificationLevels[class]; configured != "" {
				value = string(configured)
			}
		}
		rows = append(rows, fmt.Sprintf("%s  %s", notificationClassLabel(class), value))
	}
	if s.notificationConfigScope == "global" {
		for _, class := range notificationConfigClasses {
			path := s.notificationConfigDraft.NotificationSounds[class]
			if path == "" {
				path = "terminal bell"
			}
			rows = append(rows, fmt.Sprintf("%s sound  %s", notificationClassLabel(class), path))
		}
		threshold := s.notificationConfigDraft.FocusThreshold()
		rows = append(rows, "Other-Project focus threshold  "+string(threshold))
	}
	return rows
}

func (s *tuiState) renderNotificationConfigModal(out io.Writer, cols, rows int) {
	if !s.notificationConfigMode {
		return
	}
	title := "Global notification settings"
	if s.notificationConfigScope == "host" {
		title = "Host notification defaults · " + displayField(s.notificationConfigHost)
	}
	lines := []modalRenderLine{{modalTitle, "  " + title}}
	switch s.notificationConfigStep {
	case "restart":
		lines = append(lines, modalRenderLine{modalStatus, "  Saved. Restart Ducklord TUI to load settings?"},
			modalRenderLine{modalInput, "  Enter / y  restart now"},
			modalRenderLine{modalMuted, "  Esc / n    keep current settings until restart"})
	case "level":
		lines = append(lines, modalRenderLine{modalStatus, "  " + s.notificationConfigRows()[s.notificationConfigIndex]})
		choices := notificationConfigLevels
		if s.notificationConfigScope == "host" {
			lines = append(lines, modalRenderLine{modalInput, choiceLine(s.notificationConfigChoice == 0, "inherit global")})
		}
		for index, level := range choices {
			selected := s.notificationConfigChoice == index
			if s.notificationConfigScope == "host" {
				selected = s.notificationConfigChoice == index+1
			}
			lines = append(lines, modalRenderLine{modalInput, choiceLine(selected, string(level))})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ choose · Enter stage · Esc back"})
	case "path":
		lines = append(lines, modalRenderLine{modalStatus, "  " + notificationClassLabel(notificationConfigClasses[s.notificationConfigIndex-4]) + " sound file"},
			modalRenderLine{modalInput, "  path › " + s.notificationConfigPath + "▏"},
			modalRenderLine{modalMuted, "  Absolute WAV / MP3 / OGG; empty uses terminal bell"},
			modalRenderLine{modalMuted, "  Enter stage · Esc back"})
	default:
		var warnings []string
		if s.notificationConfigScope == "global" {
			for _, warning := range []string{soundSetupWarning(s.notificationConfigDraft), desktopSetupWarning(s.notificationConfigDraft, s.activity())} {
				if warning != "" {
					warnings = append(warnings, warning)
				}
			}
			if diagnostics, ok := s.notificationSink.(interface{ Warnings() []string }); ok {
				warnings = append(warnings, diagnostics.Warnings()...)
			}
		}
		options := s.notificationConfigRows()
		reserved := len(warnings)
		if s.notificationConfigErr != "" {
			reserved++
		}
		visible := max(1, rows-7-reserved)
		start := max(0, min(s.notificationConfigIndex-visible/2, len(options)-visible))
		for index := start; index < min(len(options), start+visible); index++ {
			lines = append(lines, modalRenderLine{modalInput, choiceLine(index == s.notificationConfigIndex, options[index])})
		}
		lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ select · Enter edit · s save · Esc cancel"})
		for _, warning := range warnings {
			lines = append(lines, modalRenderLine{modalDanger, "  " + warning})
		}
	}
	if s.notificationConfigErr != "" {
		lines = append(lines, modalRenderLine{modalDanger, "  " + sanitizeTerminalText(s.notificationConfigErr)})
	}
	renderModalBox(out, cols, rows, lines)
}

func choiceLine(selected bool, label string) string {
	if selected {
		return "› " + label
	}
	return "  " + label
}

func (s *tuiState) handleNotificationConfigInput(input []byte) string {
	text := string(input)
	switch s.notificationConfigStep {
	case "restart":
		if text == "y" || text == "Y" || text == "\r" || text == "\n" {
			return "restart-tui"
		}
		if text == "n" || text == "N" || text == "\x1b" || text == "\x03" {
			s.closeNotificationConfig()
			s.outputErr = "notification settings saved; restart Ducklord to load them"
		}
		return ""
	case "level":
		maxChoice := len(notificationConfigLevels) - 1
		if s.notificationConfigScope == "host" {
			maxChoice++
		}
		switch text {
		case "\x1b", "\x03":
			s.notificationConfigStep = "list"
		case "j", "\x1b[B":
			s.notificationConfigChoice = min(maxChoice, s.notificationConfigChoice+1)
		case "k", "\x1b[A":
			s.notificationConfigChoice = max(0, s.notificationConfigChoice-1)
		case "\r", "\n":
			levelIndex := s.notificationConfigChoice
			if s.notificationConfigScope == "host" {
				levelIndex--
			}
			if s.notificationConfigIndex == 8 {
				s.notificationConfigDraft.OtherProjectThreshold = notificationConfigLevels[levelIndex]
			} else {
				class := notificationConfigClasses[s.notificationConfigIndex]
				var levels map[ducklord.NotificationClass]ducklord.NotificationLevel
				if s.notificationConfigScope == "global" {
					if s.notificationConfigDraft.NotificationLevels == nil {
						s.notificationConfigDraft.NotificationLevels = make(map[ducklord.NotificationClass]ducklord.NotificationLevel)
					}
					levels = s.notificationConfigDraft.NotificationLevels
				} else {
					host := s.notificationConfigHostEntry()
					if host == nil {
						s.notificationConfigErr = "Host was removed; reopen settings"
						return ""
					}
					if host.NotificationLevels == nil {
						host.NotificationLevels = make(map[ducklord.NotificationClass]ducklord.NotificationLevel)
					}
					levels = host.NotificationLevels
				}
				if levelIndex < 0 {
					delete(levels, class)
				} else {
					levels[class] = notificationConfigLevels[levelIndex]
				}
			}
			s.notificationConfigStep, s.notificationConfigErr = "list", ""
		}
		return ""
	case "path":
		switch text {
		case "\x1b", "\x03":
			s.notificationConfigStep = "list"
		case "\x7f", "\b":
			s.notificationConfigPath = trimLastRune(s.notificationConfigPath)
		case "\r", "\n":
			path := strings.TrimSpace(s.notificationConfigPath)
			if path != "" {
				if !filepath.IsAbs(path) {
					s.notificationConfigErr = "sound path must be absolute on this Ducklord machine"
					return ""
				}
				switch strings.ToLower(filepath.Ext(path)) {
				case ".wav", ".mp3", ".ogg":
				default:
					s.notificationConfigErr = "sound must be WAV, MP3, or OGG"
					return ""
				}
				info, err := os.Lstat(path)
				if err != nil {
					s.notificationConfigErr = "cannot read sound file: " + sanitizeTerminalText(err.Error())
					return ""
				}
				if !info.Mode().IsRegular() {
					s.notificationConfigErr = "sound path must be a regular file"
					return ""
				}
				file, err := os.Open(path)
				if err != nil {
					s.notificationConfigErr = "cannot open sound file: " + sanitizeTerminalText(err.Error())
					return ""
				}
				file.Close()
			}
			if s.notificationConfigDraft.NotificationSounds == nil {
				s.notificationConfigDraft.NotificationSounds = make(map[ducklord.NotificationClass]string)
			}
			s.notificationConfigDraft.NotificationSounds[notificationConfigClasses[s.notificationConfigIndex-4]] = path
			s.notificationConfigStep, s.notificationConfigErr = "list", ""
		default:
			if utf8.Valid(input) && !strings.ContainsRune(text, '\x1b') {
				for _, r := range text {
					if !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) && len(s.notificationConfigPath)+utf8.RuneLen(r) <= 1024 {
						s.notificationConfigPath += string(r)
					}
				}
			}
		}
		return ""
	default:
		options := s.notificationConfigRows()
		switch text {
		case "\x1b", "\x03":
			s.closeNotificationConfig()
		case "j", "\x1b[B":
			s.notificationConfigIndex = min(len(options)-1, s.notificationConfigIndex+1)
		case "k", "\x1b[A":
			s.notificationConfigIndex = max(0, s.notificationConfigIndex-1)
		case "\r", "\n":
			s.notificationConfigErr = ""
			if s.notificationConfigIndex >= 4 && s.notificationConfigIndex < 8 {
				s.notificationConfigStep = "path"
				s.notificationConfigPath = s.notificationConfigDraft.NotificationSounds[notificationConfigClasses[s.notificationConfigIndex-4]]
			} else {
				s.notificationConfigStep = "level"
				s.notificationConfigChoice = s.notificationConfigCurrentChoice()
			}
		case "s":
			current, err := os.ReadFile(s.cfgPath)
			if err != nil || !bytes.Equal(current, s.notificationConfigBase) {
				s.notificationConfigErr = "config changed on disk; close and reopen settings"
				return ""
			}
			if err := ducklord.SaveConfigIfUnchanged(s.cfgPath, s.notificationConfigDraft, s.notificationConfigBase); err != nil {
				s.notificationConfigErr = err.Error()
				return ""
			}
			s.notificationConfigStep, s.notificationConfigErr = "restart", ""
		}
		return ""
	}
}

func (s *tuiState) notificationConfigCurrentChoice() int {
	if s.notificationConfigIndex == 8 {
		for index, level := range notificationConfigLevels {
			if level == s.notificationConfigDraft.FocusThreshold() {
				return index
			}
		}
		return len(notificationConfigLevels) - 1
	}
	class := notificationConfigClasses[s.notificationConfigIndex]
	var configured ducklord.NotificationLevel
	if s.notificationConfigScope == "global" {
		configured = s.notificationConfigDraft.NotificationLevels[class]
	} else if host, ok := s.notificationConfigDraft.Client(s.notificationConfigHost); ok {
		configured = host.NotificationLevels[class]
	}
	if s.notificationConfigScope == "host" && configured == "" {
		return 0
	}
	for index, level := range notificationConfigLevels {
		if configured == level {
			if s.notificationConfigScope == "host" {
				return index + 1
			}
			return index
		}
	}
	if s.notificationConfigScope == "host" {
		return 0
	}
	return 1 // global default is indicator
}

func (s *tuiState) notificationConfigHostEntry() *ducklord.Client {
	if s.notificationConfigDraft == nil {
		return nil
	}
	for index := range s.notificationConfigDraft.Clients {
		if s.notificationConfigDraft.Clients[index].Name == s.notificationConfigHost {
			return &s.notificationConfigDraft.Clients[index]
		}
	}
	return nil
}
