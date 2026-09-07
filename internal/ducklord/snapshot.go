package ducklord

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
)

const (
	snapshotMagic      = "DWLDSNP\x00"
	snapshotVersion    = uint16(1)
	MaxSnapshotPayload = 4 << 20
)

type TerminalSnapshot struct {
	InstanceID string
	SessionID  string
	SavedAt    time.Time
	Payload    []byte
}

// TerminalRenderState is a safe, parsed presentation snapshot. Text must have
// terminal control sequences removed before construction; raw PTY bytes are
// deliberately not part of this format.
type TerminalRenderState struct {
	Text              string         `json:"text,omitempty"`
	Truncated         bool           `json:"truncated,omitempty"`
	RuntimeGeneration uint64         `json:"runtime_generation,omitempty"`
	OutputOffset      uint64         `json:"output_offset,omitempty"`
	ResumeCursorValid bool           `json:"resume_cursor_valid,omitempty"`
	Framebuffer       *TerminalState `json:"framebuffer,omitempty"`
}

func EncodeTerminalRenderState(state TerminalRenderState) ([]byte, error) {
	if err := validateTerminalRenderText(state.Text); err != nil {
		return nil, err
	}
	if state.Framebuffer != nil {
		state.Text = ""
	}
	for {
		payload, err := json.Marshal(state)
		if err != nil {
			return nil, err
		}
		if len(payload) <= MaxSnapshotPayload {
			return payload, nil
		}
		if state.Framebuffer != nil && len(state.Framebuffer.Scrollback) > 0 {
			drop := len(state.Framebuffer.Scrollback) / 2
			if drop < 1 {
				drop = 1
			}
			state.Framebuffer.Scrollback = state.Framebuffer.Scrollback[drop:]
			state.Truncated = true
			continue
		}
		newline := strings.IndexByte(state.Text, '\n')
		if newline < 0 {
			return nil, fmt.Errorf("terminal render state current line exceeds 4 MiB")
		}
		state.Text = state.Text[newline+1:]
		state.Truncated = true
	}
}

func DecodeTerminalRenderState(payload []byte) (TerminalRenderState, error) {
	if len(payload) > MaxSnapshotPayload {
		return TerminalRenderState{}, fmt.Errorf("terminal render state exceeds 4 MiB")
	}
	if err := validateSnapshotStructuralBudget(payload); err != nil {
		return TerminalRenderState{}, err
	}
	var state TerminalRenderState
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return TerminalRenderState{}, fmt.Errorf("decode terminal render state: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return TerminalRenderState{}, fmt.Errorf("decode terminal render state: trailing data")
	}
	if err := validateTerminalRenderText(state.Text); err != nil {
		return TerminalRenderState{}, err
	}
	if state.Framebuffer != nil {
		if _, ok := NewTerminalFromState(*state.Framebuffer, DefaultTerminalScrollback); !ok {
			return TerminalRenderState{}, fmt.Errorf("terminal framebuffer is invalid")
		}
	}
	return state, nil
}

func validateSnapshotStructuralBudget(payload []byte) error {
	type container struct {
		delim    json.Delim
		elements int
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	stack := make([]container, 0, 8)
	totalArrayElements := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("decode terminal render state: %w", err)
		}
		delim, isDelim := token.(json.Delim)
		isClose := isDelim && (delim == ']' || delim == '}')
		if len(stack) > 0 && stack[len(stack)-1].delim == '[' && !isClose {
			stack[len(stack)-1].elements++
			totalArrayElements++
			if stack[len(stack)-1].elements > MaxTerminalRetainedCells || totalArrayElements > MaxTerminalRetainedCells+2*DefaultTerminalScrollback+1_000 {
				return fmt.Errorf("terminal render state exceeds structural budget")
			}
		}
		if !isDelim {
			continue
		}
		switch delim {
		case '[', '{':
			stack = append(stack, container{delim: delim})
		case ']', '}':
			if len(stack) == 0 {
				return fmt.Errorf("decode terminal render state: unbalanced JSON")
			}
			stack = stack[:len(stack)-1]
		}
	}
}

func validateTerminalRenderText(text string) error {
	for _, r := range text {
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f {
			return fmt.Errorf("terminal render state contains a control character")
		}
	}
	return nil
}

type SnapshotStore struct{ Root string }

func DefaultSnapshotRoot() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".ducklord", "sessions")
}

func (s SnapshotStore) path(instanceID, sessionID string) (string, error) {
	instance, err := model.ParseInstanceID(instanceID)
	if err != nil {
		return "", err
	}
	session, err := model.ParseSessionID(sessionID)
	if err != nil {
		return "", err
	}
	root := s.Root
	if root == "" {
		root = DefaultSnapshotRoot()
	}
	return filepath.Join(root, string(instance), string(session)+".snapshot"), nil
}

func (s SnapshotStore) Save(snapshot TerminalSnapshot) error {
	if len(snapshot.Payload) > MaxSnapshotPayload {
		return fmt.Errorf("terminal snapshot exceeds 4 MiB")
	}
	path, err := s.path(snapshot.InstanceID, snapshot.SessionID)
	if err != nil {
		return err
	}
	if snapshot.SavedAt.IsZero() {
		snapshot.SavedAt = time.Now()
	}
	encoded, err := encodeSnapshot(snapshot)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := secureSnapshotDirectory(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".snapshot-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s SnapshotStore) Load(instanceID, sessionID string) (TerminalSnapshot, error) {
	path, err := s.path(instanceID, sessionID)
	if err != nil {
		return TerminalSnapshot{}, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return TerminalSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return TerminalSnapshot{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return TerminalSnapshot{}, fmt.Errorf("terminal snapshot has unsafe file type or permissions")
	}
	if info.Size() > MaxSnapshotPayload+1024 {
		return TerminalSnapshot{}, fmt.Errorf("terminal snapshot file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(file, MaxSnapshotPayload+1025))
	if err != nil {
		return TerminalSnapshot{}, err
	}
	snapshot, err := decodeSnapshot(data)
	if err != nil {
		return TerminalSnapshot{}, err
	}
	if snapshot.InstanceID != instanceID || snapshot.SessionID != sessionID {
		return TerminalSnapshot{}, fmt.Errorf("terminal snapshot identity mismatch")
	}
	return snapshot, nil
}

func secureSnapshotDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, candidate := range []string{filepath.Dir(dir), dir} {
		info, err := os.Lstat(candidate)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("snapshot directory %s must be a private directory", candidate)
		}
	}
	return nil
}

func encodeSnapshot(snapshot TerminalSnapshot) ([]byte, error) {
	if len(snapshot.InstanceID) > 255 || len(snapshot.SessionID) > 255 || len(snapshot.Payload) > MaxSnapshotPayload {
		return nil, fmt.Errorf("invalid terminal snapshot")
	}
	var out bytes.Buffer
	out.WriteString(snapshotMagic)
	_ = binary.Write(&out, binary.BigEndian, snapshotVersion)
	_ = binary.Write(&out, binary.BigEndian, snapshot.SavedAt.UnixMilli())
	out.WriteByte(byte(len(snapshot.InstanceID)))
	out.WriteByte(byte(len(snapshot.SessionID)))
	_ = binary.Write(&out, binary.BigEndian, uint32(len(snapshot.Payload)))
	_ = binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(snapshot.Payload))
	out.WriteString(snapshot.InstanceID)
	out.WriteString(snapshot.SessionID)
	out.Write(snapshot.Payload)
	return out.Bytes(), nil
}

func decodeSnapshot(data []byte) (TerminalSnapshot, error) {
	const fixed = 8 + 2 + 8 + 1 + 1 + 4 + 4
	if len(data) < fixed || string(data[:8]) != snapshotMagic {
		return TerminalSnapshot{}, fmt.Errorf("invalid terminal snapshot header")
	}
	version := binary.BigEndian.Uint16(data[8:10])
	if version != snapshotVersion {
		return TerminalSnapshot{}, fmt.Errorf("unsupported terminal snapshot version %d", version)
	}
	timestamp := int64(binary.BigEndian.Uint64(data[10:18]))
	instanceLen, sessionLen := int(data[18]), int(data[19])
	payloadLen := int(binary.BigEndian.Uint32(data[20:24]))
	wantCRC := binary.BigEndian.Uint32(data[24:28])
	if payloadLen > MaxSnapshotPayload || instanceLen == 0 || sessionLen == 0 || fixed+instanceLen+sessionLen+payloadLen != len(data) {
		return TerminalSnapshot{}, fmt.Errorf("invalid terminal snapshot length")
	}
	identityEnd := fixed + instanceLen + sessionLen
	payload := data[identityEnd:]
	if crc32.ChecksumIEEE(payload) != wantCRC {
		return TerminalSnapshot{}, errors.New("terminal snapshot checksum mismatch")
	}
	return TerminalSnapshot{InstanceID: string(data[fixed : fixed+instanceLen]), SessionID: string(data[fixed+instanceLen : identityEnd]),
		SavedAt: time.UnixMilli(timestamp), Payload: append([]byte(nil), payload...)}, nil
}
