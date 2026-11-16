package ducklord

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ApproverApproval mirrors the /approver/ API's view of an approval. It holds
// approval fields only: never credentials, ACL JSON or logs.
type ApproverApproval struct {
	ID             string  `json:"id"`
	Status         string  `json:"status"`
	ClientName     string  `json:"client_name"`
	Service        string  `json:"service"`
	Method         string  `json:"method"`
	Path           string  `json:"path"`
	Query          string  `json:"query,omitempty"`
	BodyHash       string  `json:"body_hash,omitempty"`
	Rule           string  `json:"rule,omitempty"`
	HeldReason     string  `json:"held_reason"`
	CreatedAt      string  `json:"created_at"`
	ApprovedAt     *string `json:"approved_at"`
	ExpiresAt      *string `json:"expires_at"`
	DefaultMinutes int     `json:"default_minutes"`
	DecidedBy      string  `json:"decided_by,omitempty"`
	DecidedVia     string  `json:"decided_via,omitempty"`
}

// ApproverDecision is the result of an approve or reject call.
type ApproverDecision struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	DecidedBy  string `json:"decided_by"`
	DecidedVia string `json:"decided_via"`
}

// ErrApproverUnauthorized means the server rejected the token (revoked or wrong).
var ErrApproverUnauthorized = errors.New("approver token rejected by the server; run `ducklord approver login`")

// ErrApproverNotFound means the approval no longer exists.
var ErrApproverNotFound = errors.New("approval not found")

// AlreadyDecidedError is the 409 response: someone else decided first.
type AlreadyDecidedError struct {
	Status, DecidedBy, DecidedVia string
}

func (e *AlreadyDecidedError) Error() string {
	by, via := e.DecidedBy, e.DecidedVia
	if by == "" {
		by = "someone else"
	}
	if via == "" {
		return "already decided by " + by
	}
	return "already decided by " + by + " via " + via
}

// ApproverClient talks to a Duckway server's /approver/ API.
type ApproverClient struct {
	server string
	token  string
	http   *http.Client // bounded calls
	stream *http.Client // SSE: no overall timeout
}

// NewApproverClient builds a client; httpClient may be nil.
func NewApproverClient(server, token string, httpClient *http.Client) (*ApproverClient, error) {
	server, err := NormalizeApproverServer(server)
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrApproverNotLoggedIn
	}
	c := &ApproverClient{server: server, token: token}
	if httpClient != nil {
		c.http, c.stream = httpClient, httpClient
	} else {
		c.http = &http.Client{Timeout: 15 * time.Second}
		c.stream = &http.Client{}
	}
	return c, nil
}

// Server returns the normalized server URL.
func (c *ApproverClient) Server() string { return c.server }

func (c *ApproverClient) request(ctx context.Context, client *http.Client, method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.server+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		// url.Error repeats the URL only; the token travels in a header.
		return nil, err
	}
	return resp, nil
}

func (c *ApproverClient) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.request(ctx, c.http, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrApproverUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return ErrApproverNotFound
	case resp.StatusCode == http.StatusConflict:
		var raw struct {
			Status     string `json:"status"`
			DecidedBy  string `json:"decided_by"`
			DecidedVia string `json:"decided_via"`
		}
		_ = json.Unmarshal(data, &raw)
		return &AlreadyDecidedError{Status: raw.Status, DecidedBy: raw.DecidedBy, DecidedVia: raw.DecidedVia}
	case resp.StatusCode == http.StatusTooManyRequests:
		return errors.New("approver API is rate limiting this client; try again shortly")
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return fmt.Errorf("approver API %d: %s", resp.StatusCode, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// List returns approvals in the given statuses (default pending), newest first.
func (c *ApproverClient) List(ctx context.Context, statuses []string, limit int) ([]ApproverApproval, error) {
	q := url.Values{}
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	path := "/approver/approvals"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out []ApproverApproval
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns one approval.
func (c *ApproverClient) Get(ctx context.Context, id string) (*ApproverApproval, error) {
	var out ApproverApproval
	if err := c.do(ctx, http.MethodGet, "/approver/approvals/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Approve approves for durationMinutes (0 means the approval's default).
func (c *ApproverClient) Approve(ctx context.Context, id string, durationMinutes int) (*ApproverDecision, error) {
	body := map[string]int{}
	if durationMinutes > 0 {
		body["duration_minutes"] = durationMinutes
	}
	var out ApproverDecision
	if err := c.do(ctx, http.MethodPost, "/approver/approvals/"+url.PathEscape(id)+"/approve", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reject rejects the approval.
func (c *ApproverClient) Reject(ctx context.Context, id string) (*ApproverDecision, error) {
	var out ApproverDecision
	if err := c.do(ctx, http.MethodPost, "/approver/approvals/"+url.PathEscape(id)+"/reject", map[string]string{}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ApproverEventKind labels what WatchEvents reports.
type ApproverEventKind string

const (
	ApproverEventConnected    ApproverEventKind = "connected"    // stream open (server hello)
	ApproverEventApprovals    ApproverEventKind = "approvals"    // approvals topic changed
	ApproverEventDisconnected ApproverEventKind = "disconnected" // stream lost; retrying after Retry
	ApproverEventUnauthorized ApproverEventKind = "unauthorized" // token rejected; watching stopped
)

// ApproverEvent is one stream update. Pending and Total come from the server's
// approvals topic payload; consumers should refetch the list on any event.
type ApproverEvent struct {
	Kind    ApproverEventKind
	Pending int64
	Total   int64
	Err     error
	Retry   time.Duration
}

// ApproverBackoff tunes reconnects.
type ApproverBackoff struct{ Min, Max time.Duration }

// DefaultApproverBackoff doubles from 1 s up to 30 s.
var DefaultApproverBackoff = ApproverBackoff{Min: time.Second, Max: 30 * time.Second}

// WatchEvents streams /approver/events until ctx ends, reconnecting with
// exponential backoff (reset after a successful hello; a planned "bye" at the
// server's lifetime cap reconnects immediately). A 401 stops the watch after an
// unauthorized event. The returned channel closes when the watch ends.
func (c *ApproverClient) WatchEvents(ctx context.Context, backoff ApproverBackoff) <-chan ApproverEvent {
	if backoff.Min <= 0 {
		backoff.Min = DefaultApproverBackoff.Min
	}
	if backoff.Max < backoff.Min {
		backoff.Max = max(backoff.Min, DefaultApproverBackoff.Max)
	}
	out := make(chan ApproverEvent, 16)
	emit := func(ev ApproverEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	go func() {
		defer close(out)
		delay := backoff.Min
		for ctx.Err() == nil {
			planned, helloSeen, err := c.streamOnce(ctx, emit)
			if ctx.Err() != nil {
				return
			}
			if errors.Is(err, ErrApproverUnauthorized) {
				emit(ApproverEvent{Kind: ApproverEventUnauthorized, Err: err})
				return
			}
			if helloSeen {
				delay = backoff.Min
			}
			if planned {
				continue
			}
			if !emit(ApproverEvent{Kind: ApproverEventDisconnected, Err: err, Retry: delay}) {
				return
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			delay = min(delay*2, backoff.Max)
		}
	}()
	return out
}

func (c *ApproverClient) streamOnce(ctx context.Context, emit func(ApproverEvent) bool) (planned, hello bool, err error) {
	resp, err := c.request(ctx, c.stream, http.MethodGet, "/approver/events", nil)
	if err != nil {
		return false, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		return false, false, ErrApproverUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return false, false, fmt.Errorf("approver events: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var event, data string
	dispatch := func() bool {
		defer func() { event, data = "", "" }()
		switch event {
		case "hello":
			hello = true
			return emit(ApproverEvent{Kind: ApproverEventConnected})
		case "approvals":
			var p struct{ Pending, Total int64 }
			_ = json.Unmarshal([]byte(data), &p)
			return emit(ApproverEvent{Kind: ApproverEventApprovals, Pending: p.Pending, Total: p.Total})
		case "bye":
			planned = true
		}
		return true
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if !dispatch() {
				return planned, hello, ctx.Err()
			}
			if planned {
				return true, hello, nil
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			data = strings.TrimSpace(line[len("data:"):])
		}
	}
	if err := sc.Err(); err != nil {
		return false, hello, err
	}
	return false, hello, io.EOF
}
