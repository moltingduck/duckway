package queries

import (
	"context"
	"encoding/json"
	"strings"
)

// ReplayGroup is one distinct (placeholder, method, path) of the replay
// window with the number of requests it stands for.
type ReplayGroup struct {
	PlaceholderID string
	Method        string
	Path          string
	Hits          int64 // sum of repeat_count
	MaxID         int64
	// Refs are the per-ref verdicts of refused pushes, merged over the group's
	// rows (at most MaxDenyDetailRefs, first seen wins).
	Refs []DenyRef
}

// ReplayKey identifies a group.
func (g ReplayGroup) ReplayKey() string { return g.PlaceholderID + "\x00" + g.Method + "\x00" + g.Path }

const replayChunk = 200

func placeholdersIn(ids []string) (string, []interface{}) {
	marks := make([]string, len(ids))
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		marks[i], args[i] = "?", id
	}
	return strings.Join(marks, ","), args
}

// ReplayGroups reads, read-only, the request-log rows of the given phantom
// tokens from the last windowDays days, grouped by (placeholder, method, path),
// newest first, at most limit groups. total is the sum of repeat_count of every
// row in the window (not just of the returned groups); truncated reports that
// more than limit groups exist. Marker rows of suppressed denials are skipped.
// When ctx is cancelled (a client that went away, a timeout) it returns what it
// has so far with truncated set and no error.
func (q *RequestLogQueries) ReplayGroups(ctx context.Context, placeholderIDs []string, windowDays, limit int) (groups []ReplayGroup, total int64, truncated bool, err error) {
	since := utcFormat(q.clock().AddDate(0, 0, -windowDays))
	for start := 0; start < len(placeholderIDs); start += replayChunk {
		end := start + replayChunk
		if end > len(placeholderIDs) {
			end = len(placeholderIDs)
		}
		marks, args := placeholdersIn(placeholderIDs[start:end])
		base := ` FROM request_log WHERE placeholder_id IN (` + marks + `) AND created_at >= ? AND path <> ?`
		args = append(args, since, DeniedSuppressedPath)
		var t int64
		if err = q.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(repeat_count), 0)`+base, args...).Scan(&t); err != nil {
			if ctx.Err() != nil {
				return finishReplay(groups, total, limit, true)
			}
			return nil, 0, false, err
		}
		total += t
		rows, qerr := q.db.QueryContext(ctx, `SELECT placeholder_id, method, path, SUM(repeat_count), MAX(id)`+base+
			` GROUP BY placeholder_id, method, path ORDER BY MAX(id) DESC LIMIT ?`, append(args, limit+1)...)
		if qerr != nil {
			if ctx.Err() != nil {
				return finishReplay(groups, total, limit, true)
			}
			return nil, 0, false, qerr
		}
		for rows.Next() {
			var g ReplayGroup
			if err = rows.Scan(&g.PlaceholderID, &g.Method, &g.Path, &g.Hits, &g.MaxID); err != nil {
				rows.Close()
				if ctx.Err() != nil {
					return finishReplay(groups, total, limit, true)
				}
				return nil, 0, false, err
			}
			groups = append(groups, g)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			if ctx.Err() != nil {
				return finishReplay(groups, total, limit, true)
			}
			return nil, 0, false, err
		}
	}
	return finishReplay(groups, total, limit, false)
}

func finishReplay(groups []ReplayGroup, total int64, limit int, truncated bool) ([]ReplayGroup, int64, bool, error) {
	sortReplayGroups(groups)
	if len(groups) > limit {
		groups, truncated = groups[:limit], true
	}
	return groups, total, truncated, nil
}

func sortReplayGroups(g []ReplayGroup) {
	for i := 1; i < len(g); i++ {
		for j := i; j > 0 && g[j].MaxID > g[j-1].MaxID; j-- {
			g[j], g[j-1] = g[j-1], g[j]
		}
	}
}

// AttachReplayRefs fills Refs of the groups that are refused receive-pack
// pushes with stored per-ref detail. It reads at most maxRows denial rows in
// total (across all chunks). A cancelled ctx stops it quietly: refs are detail.
func (q *RequestLogQueries) AttachReplayRefs(ctx context.Context, groups []ReplayGroup, windowDays, maxRows int) error {
	idx := map[string]int{}
	var ids []string
	seen := map[string]bool{}
	for i, g := range groups {
		if g.Method == "POST" && strings.HasSuffix(g.Path, ".git/git-receive-pack") {
			idx[g.ReplayKey()] = i
			if !seen[g.PlaceholderID] {
				seen[g.PlaceholderID] = true
				ids = append(ids, g.PlaceholderID)
			}
		}
	}
	if len(idx) == 0 {
		return nil
	}
	since := utcFormat(q.clock().AddDate(0, 0, -windowDays))
	for start := 0; start < len(ids) && maxRows > 0; start += replayChunk {
		end := start + replayChunk
		if end > len(ids) {
			end = len(ids)
		}
		marks, args := placeholdersIn(ids[start:end])
		args = append(args, DecisionACLDenied, since, `%"refs":[{%`, maxRows)
		rows, err := q.db.QueryContext(ctx, `SELECT placeholder_id, method, path, deny_detail FROM request_log WHERE placeholder_id IN (`+marks+
			`) AND decision = ? AND created_at >= ? AND deny_detail LIKE ? AND method = 'POST' ORDER BY id DESC LIMIT ?`, args...)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for rows.Next() {
			maxRows--
			var ph, m, p, dd string
			if err = rows.Scan(&ph, &m, &p, &dd); err != nil {
				rows.Close()
				return err
			}
			i, ok := idx[ph+"\x00"+m+"\x00"+p]
			if !ok {
				continue
			}
			var d DenyDetail
			if json.Unmarshal([]byte(dd), &d) != nil {
				continue
			}
			g := &groups[i]
			for _, r := range d.Refs {
				if len(g.Refs) >= MaxDenyDetailRefs {
					break
				}
				dup := false
				for _, e := range g.Refs {
					if e.Ref == r.Ref {
						dup = true
						break
					}
				}
				if !dup {
					g.Refs = append(g.Refs, r)
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// ReplayWindowDays is the default replay window.
const ReplayWindowDays = 7
