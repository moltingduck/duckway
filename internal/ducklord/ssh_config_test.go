package ducklord

import (
	"strings"
	"testing"
)

func TestParseSSHConfigHosts(t *testing.T) {
	input := `
Host vulns lab-1 *.internal !blocked
  HostName 100.64.0.9

Host *
  ForwardAgent no

Host github.com # comment
  User git

Host vulns
`
	hosts, err := ParseSSHConfigHosts(strings.NewReader(input), "config")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, h := range hosts {
		names = append(names, h.Name)
	}
	got := strings.Join(names, ",")
	if got != "github.com,lab-1,vulns" {
		t.Fatalf("hosts = %s", got)
	}
}
