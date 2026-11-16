package main

import (
	"fmt"
	"io"
)

func (s *tuiState) renderAddClientModal(out io.Writer, cols, rows int) {
	if !s.addClientMode {
		return
	}
	s.resetModalMouse()
	if s.addClientStep == "mode" {
		standaloneStyle, integratedStyle := "", ""
		if s.addClientModeSelected == 0 {
			standaloneStyle = modalSelected
		} else {
			integratedStyle = modalSelected
		}
		lines := []modalRenderLine{
			{modalTitle, "  Add Ducklion host · who manages Ducklion?"},
			{standaloneStyle, "  Standalone · Use Ducklion without Duckway proxy"},
			{integratedStyle, "  Duckway proxy · Duckway installs and manages Ducklion"},
			{modalMuted, "  Already installed: verify and connect; no reinstall."},
			{modalMuted, "  Missing: Standalone installs; proxy requires remote setup."},
			{modalMuted, "  Wrong mode or stopped daemon: host is not added."},
			{modalMuted, "  ↑/↓ choose   Enter continue   Esc cancel"},
		}
		s.modalChoice(1, &s.addClientModeSelected, 0, "\r")
		s.modalChoice(2, &s.addClientModeSelected, 1, "\r")
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	selected := -1
	if len(s.addClientHosts) > 0 && s.addClientLine == "" {
		selected = min(max(s.addClientSelected, 0), len(s.addClientHosts)-1)
	}
	choices := s.addClientHosts
	maxChoices := max(1, rows-7)
	start := max(0, selected-maxChoices/2)
	if len(choices) > maxChoices {
		start = min(start, len(choices)-maxChoices)
		choices = choices[start : start+maxChoices]
	}
	lines := []modalRenderLine{{modalTitle, "  Add Ducklion host · " + s.addClientProvisionMode}}
	if len(choices) == 0 {
		lines = append(lines, modalRenderLine{modalMuted, "  No SSH config hosts found"})
	}
	for index, host := range choices {
		if !s.addClientBusy {
			s.modalChoice(len(lines), &s.addClientSelected, start+index, "\r")
			a := s.modalMouseLines[len(lines)]
			a.before = func() { s.addClientLine = "" }
			s.modalMouseLines[len(lines)] = a
		}
		style, prefix := "", "  "
		if start+index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%d  %s", prefix, start+index+1, host.Name)})
	}
	status := s.addClientErr
	if status == "" {
		status = "Choose an SSH host or enter user@host"
	}
	help := "  ↑/↓ select   Enter add   Esc back"
	if s.addClientBusy {
		help = "  Connecting in background   Esc cancel"
	}
	lines = append(lines,
		modalRenderLine{modalStatus, "  " + status},
		modalRenderLine{modalInput, "  host › " + s.addClientLine},
		modalRenderLine{modalMuted, help})
	if rows < 7 {
		choice := "No SSH config hosts"
		if selected >= 0 {
			choice = fmt.Sprintf("› %d  %s", selected+1, choices[selected-start].Name)
		} else if s.addClientLine != "" {
			choice = "Custom SSH target"
		}
		lines = []modalRenderLine{{modalSelected, choice}, {modalInput, "host › " + s.addClientLine}}
		s.modalMouseLines = make(map[int]modalMouseAction)
		if selected >= 0 && !s.addClientBusy {
			s.modalChoice(0, &s.addClientSelected, selected, "\r")
		}
	}
	s.renderModalBox(out, cols, rows, lines)
}

func (s *tuiState) renderRemoveClientModal(out io.Writer, cols, rows int) {
	if !s.removeClientMode || len(s.cfg.Clients) == 0 {
		return
	}
	s.resetModalMouse()
	selected := min(max(s.removeClientSelected, 0), len(s.cfg.Clients)-1)
	client := s.cfg.Clients[selected]
	lines := []modalRenderLine{{modalTitle, "  Remove Ducklion host"}}
	if s.removeClientConfirm != "" {
		lines = append(lines,
			modalRenderLine{modalDanger, "  REMOVE HOST CONFIGURATION?"},
			modalRenderLine{modalInput, "  " + displayField(client.Name) + " · " + displayField(client.Host)},
			modalRenderLine{modalMuted, "  Remote sessions will not be destroyed"},
			modalRenderLine{modalMuted, "  Enter remove   Esc back   Ctrl+C cancel"})
		s.renderModalBox(out, cols, rows, lines)
		return
	}
	for index, candidate := range s.cfg.Clients {
		s.modalChoice(len(lines), &s.removeClientSelected, index, "\r")
		style, prefix := "", "  "
		if index == selected {
			style, prefix = modalSelected, "› "
		}
		lines = append(lines, modalRenderLine{style, fmt.Sprintf("%s%d  %s · %s", prefix, index+1, displayField(candidate.Name), displayField(candidate.Host))})
	}
	lines = append(lines, modalRenderLine{modalMuted, "  ↑/↓ select   Enter continue   Esc/Ctrl+C cancel"})
	s.renderModalBox(out, cols, rows, lines)
}
