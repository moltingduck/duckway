package supervisor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hackerduck/duckway/internal/ducklion/model"
	"golang.org/x/sys/unix"
)

const RetainedOutputCapacity = 1 << 20
const RetainedOutputGenerationLimit = 32

type RetainedOutputSnapshot struct {
	SessionID   model.SessionID
	Generation  uint64
	StartOffset uint64
	EndOffset   uint64
	UpdatedAt   time.Time
	Data        []byte
}

type retainedOutputMetadata struct {
	Version     int             `json:"version"`
	SessionID   model.SessionID `json:"session_id"`
	Generation  uint64          `json:"runtime_generation"`
	StartOffset uint64          `json:"start_offset"`
	EndOffset   uint64          `json:"end_offset"`
	UpdatedAtMS int64           `json:"updated_at_ms"`
}

type RetainedOutput struct {
	path     string
	metaPath string
	file     *os.File
	metadata retainedOutputMetadata
	capacity int64
}

func RetainedOutputPath(sessionDir string, generation uint64) string {
	return filepath.Join(sessionDir, "output."+strconv.FormatUint(generation, 10)+".log")
}

func OpenRetainedOutput(sessionDir string, sessionID model.SessionID, generation uint64) (*RetainedOutput, error) {
	if _, err := model.ParseSessionID(string(sessionID)); err != nil || generation == 0 {
		return nil, fmt.Errorf("valid session and generation are required")
	}
	path := RetainedOutputPath(sessionDir, generation)
	file, err := openRegularNoFollow(path, unix.O_CREAT|unix.O_RDWR|unix.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open retained PTY output: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	metadata := retainedOutputMetadata{Version: 1, SessionID: sessionID, Generation: generation, EndOffset: uint64(info.Size()), UpdatedAtMS: time.Now().UnixMilli()}
	metaPath := path + ".json"
	if saved, readErr := readRetainedMetadata(metaPath); readErr == nil && saved.SessionID == sessionID && saved.Generation == generation && saved.EndOffset >= saved.StartOffset && saved.EndOffset-saved.StartOffset == uint64(info.Size()) {
		metadata = saved
	}
	output := &RetainedOutput{path: path, metaPath: metaPath, file: file, metadata: metadata, capacity: RetainedOutputCapacity}
	if err := pruneRetainedOutputGenerations(sessionDir, generation); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("prune retained PTY output: %w", err)
	}
	if info.Size() > output.capacity {
		if err := output.compact(); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return output, nil
}

func pruneRetainedOutputGenerations(sessionDir string, current uint64) error {
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return err
	}
	generations := make([]uint64, 0, len(entries))
	for _, entry := range entries {
		generation, ok := ParseRetainedOutputGeneration(entry.Name())
		if ok && entry.Type()&os.ModeSymlink == 0 && !entry.IsDir() {
			generations = append(generations, generation)
		}
	}
	if len(generations) <= RetainedOutputGenerationLimit {
		return nil
	}
	slices.Sort(generations)
	remove := len(generations) - RetainedOutputGenerationLimit
	for _, generation := range generations {
		if remove == 0 {
			break
		}
		if generation == current {
			continue
		}
		path := RetainedOutputPath(sessionDir, generation)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(path + ".json"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		remove--
	}
	return nil
}

func (o *RetainedOutput) Write(data []byte) error {
	if o == nil || o.file == nil || len(data) == 0 {
		return nil
	}
	n, err := o.file.Write(data)
	o.metadata.EndOffset += uint64(n)
	o.metadata.UpdatedAtMS = time.Now().UnixMilli()
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	info, err := o.file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > o.capacity {
		return o.compact()
	}
	return nil
}

func (o *RetainedOutput) Close() error {
	if o == nil || o.file == nil {
		return nil
	}
	o.metadata.UpdatedAtMS = time.Now().UnixMilli()
	syncErr := o.file.Sync()
	metaErr := error(nil)
	if syncErr == nil {
		metaErr = writeRetainedMetadata(o.metaPath, o.metadata)
	}
	closeErr := o.file.Close()
	o.file = nil
	return errors.Join(metaErr, syncErr, closeErr)
}

// RetainedOutputArtifactInfo securely stats a raw output artifact even when
// its metadata sidecar is absent or corrupt. Cleanup uses it only as a TTL
// fallback; callers must not use its mtime as a replay offset authority.
func RetainedOutputArtifactInfo(sessionDir string, generation uint64) (os.FileInfo, error) {
	return RetainedArtifactInfo(RetainedOutputPath(sessionDir, generation))
}

// RetainedArtifactInfo applies the same no-follow, regular-file and ownership
// checks to a cleanup artifact whose exact path came from a private session
// directory listing.
func RetainedArtifactInfo(path string) (os.FileInfo, error) {
	file, err := openRegularNoFollow(path, unix.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func (o *RetainedOutput) compact() error {
	info, err := o.file.Stat()
	if err != nil || info.Size() <= o.capacity {
		return err
	}
	keep := o.capacity
	data := make([]byte, keep)
	if _, err := o.file.ReadAt(data, info.Size()-keep); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".output-compact-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if chmodErr := tmp.Chmod(0600); chmodErr != nil {
		err = chmodErr
	} else {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	oldFile := o.file
	if err := oldFile.Close(); err != nil {
		return err
	}
	o.file = nil
	if err := os.Rename(tmpPath, o.path); err != nil {
		// The old path is still present when rename fails. Reopen it so callers
		// can either retry or close the store without using a stale descriptor.
		o.file, _ = openRegularNoFollow(o.path, unix.O_RDWR|unix.O_APPEND, 0600)
		return err
	}
	o.metadata.StartOffset = o.metadata.EndOffset - uint64(keep)
	o.file, err = openRegularNoFollow(o.path, unix.O_RDWR|unix.O_APPEND, 0600)
	return err
}

func ReadRetainedOutput(sessionDir string, sessionID model.SessionID, generation uint64) (RetainedOutputSnapshot, error) {
	path := RetainedOutputPath(sessionDir, generation)
	snapshot, err := RetainedOutputInfo(sessionDir, sessionID, generation)
	if err != nil {
		return RetainedOutputSnapshot{}, err
	}
	file, err := openRegularNoFollow(path, unix.O_RDONLY, 0)
	if err != nil {
		return RetainedOutputSnapshot{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 0 || info.Size() > RetainedOutputCapacity {
		if err == nil {
			err = fmt.Errorf("retained PTY output exceeds capacity")
		}
		return RetainedOutputSnapshot{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, RetainedOutputCapacity+1))
	if err != nil {
		return RetainedOutputSnapshot{}, fmt.Errorf("read retained PTY output: %w", err)
	}
	if len(data) != int(info.Size()) {
		return RetainedOutputSnapshot{}, fmt.Errorf("read retained PTY output: size changed during read")
	}
	snapshot.Data = data
	return snapshot, nil
}

func RetainedOutputInfo(sessionDir string, sessionID model.SessionID, generation uint64) (RetainedOutputSnapshot, error) {
	path := RetainedOutputPath(sessionDir, generation)
	file, err := openRegularNoFollow(path, unix.O_RDONLY, 0)
	if err != nil {
		return RetainedOutputSnapshot{}, err
	}
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr != nil || info.Size() < 0 || info.Size() > RetainedOutputCapacity {
		if statErr == nil {
			statErr = fmt.Errorf("retained PTY output exceeds capacity")
		}
		return RetainedOutputSnapshot{}, statErr
	}
	metadata, err := readRetainedMetadata(path + ".json")
	if err != nil || metadata.SessionID != sessionID || metadata.Generation != generation || metadata.EndOffset < metadata.StartOffset || metadata.EndOffset-metadata.StartOffset != uint64(info.Size()) {
		return RetainedOutputSnapshot{}, fmt.Errorf("retained PTY output metadata is invalid")
	}
	return RetainedOutputSnapshot{SessionID: sessionID, Generation: generation, StartOffset: metadata.StartOffset, EndOffset: metadata.EndOffset,
		UpdatedAt: time.UnixMilli(metadata.UpdatedAtMS)}, nil
}

func readRetainedMetadata(path string) (retainedOutputMetadata, error) {
	file, err := openRegularNoFollow(path, unix.O_RDONLY, 0)
	if err != nil {
		return retainedOutputMetadata{}, err
	}
	defer file.Close()
	var metadata retainedOutputMetadata
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil || metadata.Version != 1 || metadata.UpdatedAtMS <= 0 {
		if err == nil {
			err = fmt.Errorf("invalid retained output metadata")
		}
		return retainedOutputMetadata{}, err
	}
	return metadata, nil
}

func writeRetainedMetadata(path string, metadata retainedOutputMetadata) error {
	data, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".output-meta-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if chmodErr := tmp.Chmod(0600); chmodErr != nil {
		err = chmodErr
	} else {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func openRegularNoFollow(path string, flags int, mode uint32) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("retained output path is not a regular file owned by the current user")
	}
	if flags&unix.O_CREAT != 0 {
		_ = unix.Fchmod(fd, 0600)
	}
	return file, nil
}

func ParseRetainedOutputGeneration(name string) (uint64, bool) {
	if !strings.HasPrefix(name, "output.") || !strings.HasSuffix(name, ".log") {
		return 0, false
	}
	generation, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimPrefix(name, "output."), ".log"), 10, 64)
	return generation, err == nil && generation != 0
}
