package services

import "testing"

func TestExplainMatchesCheck(t *testing.T) {
	deny := `{"version":"1","provider":"x","rules":[{"name":"d","endpoints":[{"method":"DELETE","path":"/v1/*","allow":false}],"deny_all_other":false}]}`
	allowOnly := `{"version":"1","provider":"x","rules":[{"name":"r","endpoints":[{"method":"GET","path":"/v1/*","allow":true}],"deny_all_other":true}]}`
	configs := []string{"", " ", "\n\t", "   {}  ", "{", "null", "{}", deny, allowOnly, ` ` + deny + ` `}
	requests := []struct{ method, path string }{{"GET", "/v1/models"}, {"DELETE", "/v1/x"}, {"POST", "/other"}}
	pc := NewPermissionChecker()
	for _, c := range configs {
		for _, r := range requests {
			got := pc.Check(c, "ph", r.method, r.path, nil)
			ex := pc.Explain(c, r.method, r.path, nil)
			if got.Allowed != ex.Allowed || got.Reason != ex.Reason {
				t.Errorf("config %q %s %s: Check=%+v Explain=%+v", c, r.method, r.path, got, ex)
			}
		}
	}
}
