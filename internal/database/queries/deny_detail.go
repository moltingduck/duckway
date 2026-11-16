package queries

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on the stored refusal detail (request_log.deny_detail).
const (
	MaxDenyDetailBytes = 8 << 10
	MaxDenyDetailRefs  = 20
	maxDenyFieldBytes  = 512
	maxDenyRefBytes    = 300
	maxDenySeesBytes   = 2048
)

// DenyRef is the verdict for one ref of a push (one pkt-line command).
type DenyRef struct {
	Ref     string `json:"ref"`
	Verdict string `json:"verdict"` // allowed | refused
	Pattern string `json:"pattern"`
	Role    string `json:"role"`
	Reason  string `json:"reason"`
}

// DenyDetail is the JSON stored in request_log.deny_detail. Kind must stay the
// first field: the "refused by" filter matches on the stored prefix.
type DenyDetail struct {
	Kind   string    `json:"kind"`
	Layer  string    `json:"layer"`
	Rule   string    `json:"rule"`
	Reason string    `json:"reason"`
	Refs   []DenyRef `json:"refs"`
	Sees   string    `json:"sees"`
	More   int       `json:"more,omitempty"`
}

// cleanDenyText bounds s to max bytes on a UTF-8 boundary and replaces control
// characters (ref names and other fragments come from the agent). keepNL keeps
// line feeds, for the multi-line "what the agent saw" text.
func cleanDenyText(s string, max int, keepNL bool) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) > max {
		s = s[:max]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' && keepNL:
			return r
		case unicode.IsControl(r) || r == ' ' || r == ' ':
			return '�'
		}
		return r
	}, s)
}

func marshalNoEscape(v interface{}) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// Marshal returns the bounded JSON stored for the detail: strings truncated on
// UTF-8 boundaries with control characters replaced, at most MaxDenyDetailRefs
// refs (the rest counted in "more"), and never more than MaxDenyDetailBytes.
func (d DenyDetail) Marshal() string {
	out := DenyDetail{
		Kind:   cleanDenyText(d.Kind, 64, false),
		Layer:  cleanDenyText(d.Layer, 64, false),
		Rule:   cleanDenyText(d.Rule, maxDenyFieldBytes, false),
		Reason: cleanDenyText(d.Reason, maxDenyFieldBytes, false),
		Sees:   cleanDenyText(d.Sees, maxDenySeesBytes, true),
		More:   d.More,
		Refs:   []DenyRef{},
	}
	for i, r := range d.Refs {
		if i >= MaxDenyDetailRefs {
			out.More += len(d.Refs) - i
			break
		}
		out.Refs = append(out.Refs, DenyRef{
			Ref:     cleanDenyText(r.Ref, maxDenyRefBytes, false),
			Verdict: cleanDenyText(r.Verdict, 32, false),
			Pattern: cleanDenyText(r.Pattern, maxDenyFieldBytes, false),
			Role:    cleanDenyText(r.Role, 32, false),
			Reason:  cleanDenyText(r.Reason, maxDenyFieldBytes, false),
		})
	}
	s := marshalNoEscape(out)
	for len(s) > MaxDenyDetailBytes && len(out.Refs) > 0 {
		out.Refs = out.Refs[:len(out.Refs)-1]
		out.More++
		s = marshalNoEscape(out)
	}
	for len(s) > MaxDenyDetailBytes && len(out.Sees) > 0 {
		out.Sees = cleanDenyText(out.Sees, len(out.Sees)/2, true)
		s = marshalNoEscape(out)
	}
	return s
}

// DenyKinds are the values of DenyDetail.Kind: what refused the request.
var DenyKinds = []string{
	"service_acl", "api_key_acl", "token_acl", "capability",
	"branch_rules", "invalid_policy", "uncheckable", "approval",
}

// ValidDenyKind reports whether k is one of DenyKinds.
func ValidDenyKind(k string) bool {
	for _, v := range DenyKinds {
		if v == k {
			return true
		}
	}
	return false
}
