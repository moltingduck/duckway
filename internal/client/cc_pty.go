package client

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	ptyRows               = 40
	ptyCols               = 120
	ptyDefaultIdleTimeout = 30 * time.Minute
)

type stopPayload struct {
	SessionID            string `json:"session_id"`
	TranscriptPath       string `json:"transcript_path"`
	LastAssistantMessage string `json:"last_assistant_message"`
}

// buildHooksSettings produces the inline --settings JSON that registers
// the Stop hook pointing at hookScript.
func buildHooksSettings(hookScript string) (string, error) {
	settings := map[string]interface{}{
		"hooks": map[string]interface{}{
			"Stop": []map[string]interface{}{
				{
					"matcher": "*",
					"hooks": []map[string]interface{}{
						{"type": "command", "command": hookScript + " stop"},
					},
				},
			},
		},
	}
	b, err := json.Marshal(settings)
	return string(b), err
}

// respondToDecQueries scans buf for the DEC/XTerm terminal queries that Ink
// (Claude Code's TUI runtime) issues at startup and returns the response bytes
// that must be written back to the PTY master. If none are found, returns nil.
func respondToDecQueries(buf []byte) []byte {
	var resp []byte
	i := 0
	for i < len(buf) {
		if buf[i] != '\x1b' {
			i++
			continue
		}
		if i+1 >= len(buf) {
			break
		}
		if buf[i+1] != '[' {
			i += 2
			continue
		}
		// CSI sequence: ESC [ <params> <final>
		// Parameter bytes: 0x30–0x3F  Final byte: 0x40–0x7E
		j := i + 2
		start := j
		for j < len(buf) && buf[j] >= 0x30 && buf[j] <= 0x3F {
			j++
		}
		if j >= len(buf) {
			break
		}
		final := buf[j]
		params := string(buf[start:j])
		j++

		switch {
		case final == 'c' && (params == "" || params == "0"):
			// DA1 primary device attributes
			resp = append(resp, "\x1b[?6c"...)
		case final == 'c' && params == ">", final == 'c' && params == ">0":
			// DA2 secondary device attributes
			resp = append(resp, "\x1b[>0;0;0c"...)
		case final == 'n' && params == "5":
			// DSR device status report — report ready
			resp = append(resp, "\x1b[0n"...)
		case final == 'n' && params == "6":
			// CPR cursor position report
			resp = append(resp, "\x1b[1;1R"...)
		case final == 't' && params == "18":
			// Window size query
			resp = append(resp, fmt.Sprintf("\x1b[8;%d;%dt", ptyRows, ptyCols)...)
		case final == 'q' && params == ">":
			// XTVERSION
			resp = append(resp, "\x1bP>|duckway 0\x1b\\"...)
		}
		i = j
	}
	return resp
}
