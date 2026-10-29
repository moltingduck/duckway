package serverplugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/hackerduck/duckway/internal/clientplugin"
)

const DefaultCheckInterval = 6 * time.Hour

func checkInterval(raw string) (time.Duration, error) {
	if raw == "" {
		return DefaultCheckInterval, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < time.Minute || d > 7*24*time.Hour {
		return 0, errors.New("plugin check interval must be between one minute and seven days")
	}
	return d, nil
}

// The approval digest includes every typed manifest field, including original
// artifact URLs, descriptions and ordered artifact hashes/sizes. Version alone
// is a publisher assertion and cannot identify the administrator's reviewed data.
func manifestDigest(m *clientplugin.Manifest) string {
	if m == nil {
		return ""
	}
	data, err := json.Marshal(m)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func validRelease(m clientplugin.Manifest, name string) error {
	if m.Name != name || m.Validate() != nil {
		return errors.New("invalid plugin manifest or name mismatch")
	}
	var total int64
	for _, a := range m.Artifacts {
		total += a.Size
	}
	if len(m.Artifacts) > maxArtifacts || total > maxTotalSize {
		return errors.New("plugin release exceeds import limits")
	}
	return nil
}
func readSource(ctx context.Context, client *http.Client, source, name string) (clientplugin.Manifest, error) {
	resp, err := fetch(ctx, client, source)
	if err != nil {
		return clientplugin.Manifest{}, err
	}
	defer resp.Body.Close()
	m, err := clientplugin.DecodeManifest(resp.Body)
	if err != nil {
		return m, err
	}
	return m, validRelease(m, name)
}
func (s *Store) snapshot(ctx context.Context, name string) (Entry, error) {
	root, done, err := s.locked(ctx, false)
	if err != nil {
		return Entry{}, err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return Entry{}, err
	}
	e, ok := c.Plugins[name]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Callers acquire a per-plugin lock before this method. Global locks protect
// only catalog snapshots and short atomic writes, never upstream HTTP.
func (s *Store) checkSource(ctx context.Context, e Entry, now time.Time) (clientplugin.Manifest, error) {
	source, err := clientplugin.SourceURL(e.Source)
	var m clientplugin.Manifest
	if err == nil {
		client, clientErr := s.policy.client(source)
		err = clientErr
		if err == nil {
			m, err = readSource(ctx, client, source.String(), e.Name)
			client.CloseIdleConnections()
		}
	}
	if err == nil {
		if old, ok := e.Releases[m.Version]; ok && !reflect.DeepEqual(old, rewritten(m)) {
			err = errors.New("immutable plugin version changed")
		}
	}
	root, done, lockErr := s.locked(ctx, true)
	if lockErr != nil {
		return clientplugin.Manifest{}, lockErr
	}
	defer done()
	c, readErr := readCatalog(root)
	if readErr != nil {
		return clientplugin.Manifest{}, readErr
	}
	current, ok := c.Plugins[e.Name]
	if !ok || current.Source != e.Source {
		return clientplugin.Manifest{}, ErrNotFound
	}
	current.LastCheckedAt = now.UTC().Format(time.RFC3339Nano)
	current.NextCheckAt = now.Add(s.interval).UTC().Format(time.RFC3339Nano)
	if err == nil {
		current.Latest = &m
		current.LastSuccessfulCheckAt = current.LastCheckedAt
		current.CheckError = ""
	} else {
		current.CheckError = "Source check failed; published release is unchanged"
	}
	// Merge metadata into the latest entry, preserving a concurrent enable/disable.
	c.Plugins[e.Name] = current
	if saveErr := saveCatalog(root, c); saveErr != nil {
		return clientplugin.Manifest{}, saveErr
	}
	if err != nil {
		return clientplugin.Manifest{}, errors.New("plugin source check failed")
	}
	return rewritten(m), nil
}
func (s *Store) Import(ctx context.Context, name, raw string) (clientplugin.Manifest, error) {
	var empty clientplugin.Manifest
	if clientplugin.ValidateName(name) != nil {
		return empty, errors.New("invalid plugin name")
	}
	source, err := clientplugin.SourceURL(raw)
	if err != nil {
		return empty, errNetwork
	}
	client, err := s.policy.client(source)
	if err != nil {
		return empty, err
	}
	client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, release, err := s.lockedFile(ctx, ".plugin-"+name+".lock", true)
	if err != nil {
		return empty, err
	}
	defer release()
	root, done, err := s.locked(ctx, true)
	if err != nil {
		return empty, err
	}
	c, err := readCatalog(root)
	if err != nil {
		done()
		return empty, err
	}
	e, exists := c.Plugins[name]
	if !exists {
		if len(c.Plugins) >= maxPlugins {
			done()
			return empty, errors.New("plugin catalog is full")
		}
		e = Entry{Name: name, Source: source.String(), Releases: make(map[string]clientplugin.Manifest)}
		c.Plugins[name] = e
		err = saveCatalog(root, c)
	} else if e.Source != source.String() {
		err = errors.New("plugin source is fixed")
	}
	done()
	if err != nil {
		return empty, err
	}
	return s.checkSource(ctx, e, time.Now())
}
func (s *Store) check(ctx context.Context, name string, now time.Time, dueOnly bool) (clientplugin.Manifest, error) {
	var empty clientplugin.Manifest
	if clientplugin.ValidateName(name) != nil {
		return empty, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	_, release, err := s.lockedFile(ctx, ".plugin-"+name+".lock", true)
	if err != nil {
		return empty, err
	}
	defer release()
	e, err := s.snapshot(ctx, name)
	if err != nil {
		return empty, err
	}
	if dueOnly {
		due, err := time.Parse(time.RFC3339Nano, e.NextCheckAt)
		if err == nil && now.Before(due) {
			return empty, nil
		}
	}
	return s.checkSource(ctx, e, now)
}
func (s *Store) Check(ctx context.Context, name string) (clientplugin.Manifest, error) {
	return s.check(ctx, name, time.Now(), false)
}
func (s *Store) CheckDue(ctx context.Context, now time.Time) error {
	entries, err := s.List(ctx, true)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, _ = s.check(ctx, e.Name, now, true)
	}
	return nil
}
func (s *Store) publishFailure(ctx context.Context, original Entry) error {
	root, done, err := s.locked(ctx, true)
	if err != nil {
		return err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return err
	}
	e, ok := c.Plugins[original.Name]
	if !ok || e.Source != original.Source {
		return ErrNotFound
	}
	e.CheckError = "Source changed or became unavailable; check again before publishing"
	c.Plugins[e.Name] = e
	return saveCatalog(root, c)
}
func (s *Store) Publish(ctx context.Context, name, expectedVersion, expectedDigest string) (clientplugin.Manifest, error) {
	var empty clientplugin.Manifest
	if clientplugin.ValidateName(name) != nil {
		return empty, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	stageRoot, release, err := s.lockedFile(ctx, ".plugin-"+name+".lock", true)
	if err != nil {
		return empty, err
	}
	defer release()
	e, err := s.snapshot(ctx, name)
	if err != nil {
		return empty, err
	}
	if e.Latest == nil || e.Latest.Version != expectedVersion || expectedVersion == e.Current || e.CheckError != "" || expectedDigest == "" || manifestDigest(e.Latest) != expectedDigest {
		return empty, errors.New("checked manifest approval changed; refresh before publishing")
	}
	expected := *e.Latest
	approved := rewritten(expected)
	source, err := clientplugin.SourceURL(e.Source)
	if err != nil {
		return empty, errNetwork
	}
	client, err := s.policy.client(source)
	if err != nil {
		return empty, err
	}
	defer client.CloseIdleConnections()
	confirm := func() error {
		actual, err := readSource(ctx, client, source.String(), name)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			if saveErr := s.publishFailure(ctx, e); saveErr != nil {
				return saveErr
			}
			return errors.New("plugin source changed; check again")
		}
		return nil
	}
	if err := confirm(); err != nil {
		return empty, err
	}
	stage := ""
	if old, ok := e.Releases[expectedVersion]; ok {
		if !reflect.DeepEqual(old, approved) {
			return empty, errors.New("immutable plugin version changed")
		}
	} else {
		stage, err = stageRelease(ctx, stageRoot, expected, source, client)
		if err != nil {
			return empty, err
		}
		defer stageRoot.RemoveAll(stage)
	}
	if err := confirm(); err != nil {
		return empty, err
	}
	root, done, err := s.locked(ctx, true)
	if err != nil {
		return empty, err
	}
	defer done()
	c, err := readCatalog(root)
	if err != nil {
		return empty, err
	}
	current, ok := c.Plugins[name]
	// Enable/disable increments Revision. A concurrent administrator action wins;
	// publishing must be retried explicitly rather than silently re-enabling it.
	if !ok || current.Source != e.Source || current.Revision != e.Revision || current.Current != e.Current || manifestDigest(current.Latest) != expectedDigest || current.CheckError != "" {
		return empty, errors.New("plugin state changed during publish; refresh and retry")
	}
	if stage != "" {
		if err := installStage(root, name, expectedVersion, stage); err != nil {
			return empty, err
		}
	}
	current.Current = expectedVersion
	current.Enabled = true
	current.Revision++
	current.Releases[expectedVersion] = approved
	c.Plugins[name] = current
	if err := saveCatalog(root, c); err != nil {
		return empty, err
	}
	return approved, nil
}
