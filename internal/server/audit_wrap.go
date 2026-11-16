package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/hackerduck/duckway/internal/audit"
	"github.com/hackerduck/duckway/internal/server/handlers"
	"github.com/hackerduck/duckway/internal/server/middleware"
)

// auditRoute describes how one mutating route is audited. Snap names a
// snapshot type in handlers.AuditSnapshots; IDParam is the path parameter
// holding the resource id (empty: take it from the response's "id").
type auditRoute struct {
	Type, Verb, IDParam, Snap string
	Skip                      bool // dry runs and dismissals that change nothing
}

const (
	svcT, keyT, phT, grpT, cliT = "service", "api_key", "placeholder", "group", "client"
	ntfT, setT, apvT            = "notification", "setting", "approval"
	aptT                        = "approver_token"
)

var auditRoutes = map[string]auditRoute{
	"POST /api/services":                      {Type: svcT, Verb: "create", Snap: svcT},
	"PUT /api/services/{id}":                  {Type: svcT, Verb: "update", IDParam: "id", Snap: svcT},
	"DELETE /api/services/{id}":               {Type: svcT, Verb: "delete", IDParam: "id", Snap: svcT},
	"POST /api/services/{id}/acl-templates":   {Type: svcT, Verb: "acl_template", IDParam: "id", Snap: svcT},
	"POST /api/services/{id}/pricing":         {Type: svcT, Verb: "pricing_create", IDParam: "id", Snap: svcT},
	"PUT /api/services/{id}/pricing/{pid}":    {Type: svcT, Verb: "pricing_update", IDParam: "id", Snap: svcT},
	"DELETE /api/services/{id}/pricing/{pid}": {Type: svcT, Verb: "pricing_delete", IDParam: "id", Snap: svcT},

	"POST /api/keys":                         {Type: keyT, Verb: "create", Snap: keyT},
	"PUT /api/keys/{id}":                     {Type: keyT, Verb: "update", IDParam: "id", Snap: keyT},
	"DELETE /api/keys/{id}":                  {Type: keyT, Verb: "delete", IDParam: "id", Snap: keyT},
	"POST /api/keys/{id}/acl-templates":      {Type: keyT, Verb: "acl_template", IDParam: "id", Snap: keyT},
	"POST /api/keys/{id}/acl":                {Type: keyT, Verb: "acl_update", IDParam: "id", Snap: keyT},
	"POST /api/keys/github-app/test":         {Skip: true},
	"POST /api/keys/github-app/repositories": {Skip: true},
	"POST /api/oauth/upload":                 {Type: keyT, Verb: "create", Snap: keyT},
	"PUT /api/oauth/{id}":                    {Type: keyT, Verb: "update", IDParam: "id", Snap: keyT},
	"DELETE /api/oauth/{id}":                 {Type: keyT, Verb: "delete", IDParam: "id", Snap: keyT},
	"POST /api/oauth/{id}/refresh":           {Type: keyT, Verb: "refresh", IDParam: "id", Snap: keyT},
	"POST /api/oauth/validate":               {Skip: true},
	"POST /api/oauth/{id}/validate":          {Skip: true},

	"POST /api/placeholders":                     {Type: phT, Verb: "create", Snap: phT},
	"PUT /api/placeholders/{id}":                 {Type: phT, Verb: "update", IDParam: "id", Snap: phT},
	"DELETE /api/placeholders/{id}":              {Type: phT, Verb: "delete", IDParam: "id", Snap: phT},
	"POST /api/placeholders/{id}/acl/test":       {Skip: true},
	"POST /api/phantom-tokens/{id}/simulate":     {Skip: true},
	"POST /api/placeholders/{id}/preview-impact": {Skip: true},
	"POST /api/keys/{id}/preview-impact":         {Skip: true},
	"POST /api/services/{id}/preview-impact":     {Skip: true},

	"POST /api/approvers":        {Type: aptT, Verb: "create", Snap: aptT},
	"DELETE /api/approvers/{id}": {Type: aptT, Verb: "revoke", IDParam: "id", Snap: aptT},

	"POST /api/clients":                   {Type: cliT, Verb: "create", Snap: cliT},
	"PUT /api/clients/{id}":               {Type: cliT, Verb: "update", IDParam: "id", Snap: cliT},
	"DELETE /api/clients/{id}":            {Type: cliT, Verb: "delete", IDParam: "id", Snap: cliT},
	"POST /api/clients/{id}/rotate-token": {Type: cliT, Verb: "rotate_token", IDParam: "id", Snap: cliT},
	"POST /api/clients/{id}/canary":       {Type: cliT, Verb: "canary", IDParam: "id", Snap: cliT},

	"POST /api/groups":                        {Type: grpT, Verb: "create", Snap: grpT},
	"PUT /api/groups/{id}":                    {Type: grpT, Verb: "update", IDParam: "id", Snap: grpT},
	"DELETE /api/groups/{id}":                 {Type: grpT, Verb: "delete", IDParam: "id", Snap: grpT},
	"POST /api/groups/{id}/members":           {Type: grpT, Verb: "member_add", IDParam: "id", Snap: grpT},
	"DELETE /api/groups/{id}/members/{keyId}": {Type: grpT, Verb: "member_remove", IDParam: "id", Snap: grpT},

	"POST /api/notifications":             {Type: ntfT, Verb: "create", Snap: ntfT},
	"PUT /api/notifications/{id}":         {Type: ntfT, Verb: "update", IDParam: "id", Snap: ntfT},
	"DELETE /api/notifications/{id}":      {Type: ntfT, Verb: "delete", IDParam: "id", Snap: ntfT},
	"POST /api/notifications/{id}/active": {Type: ntfT, Verb: "active", IDParam: "id", Snap: ntfT},
	"POST /api/notifications/{id}/test":   {Skip: true},

	"POST /api/approvals/{id}/approve":   {Type: apvT, Verb: "approve", IDParam: "id", Snap: apvT},
	"POST /api/approvals/{id}/reject":    {Type: apvT, Verb: "reject", IDParam: "id", Snap: apvT},
	"POST /admin/approvals/{id}/approve": {Type: apvT, Verb: "approve", IDParam: "id", Snap: apvT},
	"POST /admin/approvals/{id}/reject":  {Type: apvT, Verb: "reject", IDParam: "id", Snap: apvT},

	"POST /api/settings":             {Type: setT, Verb: "update", Snap: setT},
	"POST /api/canary/settings":      {Type: setT, Verb: "update", Snap: setT},
	"POST /api/supply-chain/min-age": {Type: setT, Verb: "update", Snap: setT},
	"POST /api/supply-chain/{id}":    {Type: setT, Verb: "update", Snap: setT},
	"POST /api/logs/capture":         {Type: setT, Verb: "update", Snap: setT},
	"POST /api/whats-new/dismiss":    {Skip: true},
}

// routeTypeNames maps the first path segment of an un-tabled route to a resource type.
var routeTypeNames = map[string]string{
	"plugins": "plugin", "cc": "cc", "canary": "canary", "key-suites": "key_suite", "key-groups": "key_group",
	"client-drafts": "client_draft", "client-updates": "client_update", "auth": "admin_user", "logs": "request_log",
}

func verbOf(method string) string {
	switch method {
	case http.MethodPost:
		return "post"
	case http.MethodPut:
		return "update"
	case http.MethodPatch:
		return "update"
	case http.MethodDelete:
		return "delete"
	}
	return strings.ToLower(method)
}

// pathParams extracts {name} values by walking pattern and path segments together.
func pathParams(pattern, path string) map[string]string {
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = pattern[i+1:]
	}
	ps, xs := strings.Split(pattern, "/"), strings.Split(path, "/")
	out := map[string]string{}
	for i, p := range ps {
		if i >= len(xs) {
			break
		}
		if strings.HasPrefix(p, "{") && strings.HasSuffix(p, "}") {
			out[strings.TrimSuffix(strings.TrimPrefix(strings.TrimSuffix(p, "}"), "{"), "...")] = xs[i]
		}
	}
	return out
}

// genericRoute derives a route description for a mutating route with no table entry.
func genericRoute(pattern, path string) (auditRoute, string) {
	spec := pattern
	if i := strings.IndexByte(spec, ' '); i >= 0 {
		spec = spec[i+1:]
	}
	segs := strings.Split(strings.Trim(spec, "/"), "/")
	typ := "admin"
	if len(segs) >= 2 {
		typ = segs[1]
		if n, ok := routeTypeNames[typ]; ok {
			typ = n
		}
	}
	rt := auditRoute{Type: typ}
	var id string
	for _, p := range segs {
		if strings.HasPrefix(p, "{") {
			name := strings.Trim(p, "{}.")
			if id = pathParams(pattern, path)[name]; id != "" {
				break
			}
		}
	}
	// action verb: last literal segment beyond the type, else the HTTP verb.
	verb := ""
	for i := len(segs) - 1; i >= 2; i-- {
		if !strings.HasPrefix(segs[i], "{") {
			verb = strings.ReplaceAll(segs[i], "-", "_")
			break
		}
	}
	rt.Verb = verb
	return rt, id
}

type auditWriter struct {
	http.ResponseWriter
	status  int
	capture bool
	buf     bytes.Buffer
}

func (w *auditWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *auditWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.capture && w.buf.Len() < 64*1024 {
		w.buf.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *auditWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// auditMux records every mutating request routed by mux: method, route
// pattern, actor, status and resource id, plus before/after snapshots for the
// rule- and access-bearing resources. A recording failure never affects the
// response.
func auditMux(mux *http.ServeMux, rec *audit.Recorder, snaps *handlers.AuditSnapshots) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			mux.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		route, tabled := auditRoutes[pattern]
		if pattern == "" || (tabled && route.Skip) {
			mux.ServeHTTP(w, r)
			return
		}
		id := ""
		if tabled {
			if route.IDParam != "" {
				id = pathParams(pattern, r.URL.Path)[route.IDParam]
			}
		} else {
			route, id = genericRoute(pattern, r.URL.Path)
		}
		if route.Verb == "" {
			route.Verb = verbOf(r.Method)
		}

		user, _ := r.Context().Value(middleware.AdminUserKey).(string)
		actor := "admin:" + user
		if user == "" {
			actor = "anonymous"
		}
		ident := audit.Identity{Actor: actor, Via: "web", RemoteAddr: audit.RemoteHost(r)}
		ctx := audit.WithIdentity(audit.WithRecorder(r.Context(), rec), ident)
		r = r.WithContext(ctx)

		var before any
		var name string
		beforeOK := false
		if route.Snap != "" && (id != "" || route.Snap == setT) {
			func() {
				defer func() { _ = recover() }()
				name, before, beforeOK = snaps.Get(route.Snap, id)
			}()
		}
		aw := &auditWriter{ResponseWriter: w, capture: route.Snap != "" && id == ""}
		mux.ServeHTTP(aw, r)
		status := aw.status
		if status == 0 {
			status = http.StatusOK
		}
		ok := status < 400
		if id == "" && aw.capture {
			var resp struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(aw.buf.Bytes(), &resp) == nil {
				id = resp.ID
			}
		}
		var after any
		afterOK := false
		if ok && route.Snap != "" && route.Verb != "delete" && (id != "" || route.Snap == setT) {
			func() {
				defer func() { _ = recover() }()
				var n string
				n, after, afterOK = snaps.Get(route.Snap, id)
				if n != "" {
					name = n
				}
			}()
		}
		approvalID, resType := "", route.Type
		if route.Type == apvT && beforeOK {
			if m, _ := before.(map[string]any); m != nil {
				if m["status"] != "pending" {
					return // already decided: the redirect changed nothing
				}
				// Decisions are filed under the phantom token they unlock, so the
				// token's History shows them; the approval id stays in the summary.
				approvalID, resType = id, "placeholder"
				id, _ = m["placeholder_id"].(string)
			}
		}
		if !beforeOK {
			before = nil
		}
		if !afterOK {
			after = nil
		}
		if route.Type == setT {
			before, after = changedKeys(before, after)
		}
		entry := audit.Entry{
			Action: route.Type + "." + route.Verb, ResourceType: resType, ResourceID: id, ResourceName: name,
			Before: before, After: after, Summary: auditSummary(route, name, id, before, after, status),
		}
		if approvalID != "" {
			entry.Summary = approvalSummary(route.Verb, approvalID, name)
			if !ok {
				entry.Summary += fmt.Sprintf(" (failed, HTTP %d)", status)
			}
		}
		if !ok {
			entry.Status = "failed"
		}
		rec.Record(ctx, entry)
	})
}

// changedKeys keeps only the keys whose value differs, so a one-field settings
// save does not log the whole settings table.
func changedKeys(before, after any) (any, any) {
	bm, _ := before.(map[string]any)
	am, _ := after.(map[string]any)
	if bm == nil && am == nil {
		return before, after
	}
	nb, na := map[string]any{}, map[string]any{}
	for k, v := range bm {
		if av, ok := am[k]; !ok || !reflect.DeepEqual(v, av) {
			nb[k] = v
		}
	}
	for k, v := range am {
		if bv, ok := bm[k]; !ok || !reflect.DeepEqual(v, bv) {
			na[k] = v
		}
	}
	return nb, na
}

func auditSummary(route auditRoute, name, id string, before, after any, status int) string {
	subject := strings.ReplaceAll(route.Type, "_", " ")
	label := name
	if label == "" {
		label = id
	}
	var s string
	if route.Type == setT {
		keys := map[string]bool{}
		for _, v := range []any{before, after} {
			if m, ok := v.(map[string]any); ok {
				for k := range m {
					keys[k] = true
				}
			}
		}
		var list []string
		for k := range keys {
			list = append(list, k)
		}
		sort.Strings(list)
		if len(list) == 0 {
			s = "saved settings (no change)"
		} else {
			s = "changed " + strings.Join(list, ", ")
		}
	} else {
		s = strings.ReplaceAll(route.Verb, "_", " ") + " " + subject
		if label != "" {
			s += " " + label
		}
	}
	if status >= 400 {
		s += fmt.Sprintf(" (failed, HTTP %d)", status)
	}
	return s
}
