package client

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// FormatText is the plain report used by pipes and remote control channels.
func (r DoctorReport) FormatText() string { return r.FormatTextWithColor(false) }

// FormatTextWithColor adds terminal styling only when the caller opts in.
// All report data is escaped before styling so local paths cannot inject controls.
func (r DoctorReport) FormatTextWithColor(color bool) string {
	var b strings.Builder
	style := func(s, code string) string {
		if color {
			return "\x1b[" + code + "m" + s + "\x1b[0m"
		}
		return s
	}
	heading := func(s string) { b.WriteString(style(s, "1") + "\n") }
	status := func(s string) string {
		code := "36"
		switch s {
		case "OK":
			code = "32"
		case "ERROR", "MISSING":
			code = "31"
		case "WARN":
			code = "33"
		}
		return style("["+doctorDisplay(s)+"]", code)
	}
	heading("Duckway doctor")
	fmt.Fprintf(&b, "Version: %s\nConfig: %s\n", doctorDisplay(r.Version), doctorDisplay(r.ConfigDir))
	if r.Client != "" {
		fmt.Fprintf(&b, "Client: %s\n", doctorDisplay(r.Client))
	}
	if r.ServerURL != "" {
		fmt.Fprintf(&b, "Server: %s\n", doctorDisplay(r.ServerURL))
	}
	counts := map[string]int{}
	for _, line := range r.Lines {
		counts[line.Status]++
	}
	other := 0
	for s, n := range counts {
		switch s {
		case "ERROR", "MISSING", "WARN", "UNKNOWN", "OK", "INFO":
		default:
			other += n
		}
	}
	summary := "No issues reported"
	if counts["ERROR"]+counts["MISSING"]+counts["WARN"] > 0 {
		summary = "Attention needed"
	} else if counts["UNKNOWN"]+other > 0 {
		summary = "Checks inconclusive"
	} else if len(r.Lines) == 0 {
		summary = "No checks reported"
	}
	fmt.Fprintf(&b, "\nSummary: %s — %d error, %d missing, %d warning; %d inconclusive; %d OK, %d info", summary, counts["ERROR"], counts["MISSING"], counts["WARN"], counts["UNKNOWN"], counts["OK"], counts["INFO"])
	if other > 0 {
		fmt.Fprintf(&b, "; %d other status", other)
	}
	b.WriteByte('\n')
	// Stable priority order; group identical advice while retaining affected labels.
	type repair struct {
		advice string
		names  []string
	}
	var repairs []repair
	byAdvice := map[string]int{}
	for _, severity := range []string{"ERROR", "MISSING", "WARN"} {
		for _, line := range r.Lines {
			if line.Status != severity || line.Missing == "" {
				continue
			}
			if i, ok := byAdvice[line.Missing]; ok {
				repairs[i].names = append(repairs[i].names, line.Name)
			} else {
				byAdvice[line.Missing] = len(repairs)
				repairs = append(repairs, repair{line.Missing, []string{line.Name}})
			}
		}
	}
	if len(repairs) > 0 {
		b.WriteByte('\n')
		heading("Next steps (errors, missing items, then warnings)")
		for i, fix := range repairs {
			fmt.Fprintf(&b, "  %d. %s\n     For: %s\n", i+1, doctorDisplay(fix.advice), doctorDisplay(strings.Join(fix.names, ", ")))
		}
	}
	titles := []string{"Setup and server", "Local runtime", "Credentials (configured files; not request proof)", "Routing expectations (doctor environment; child may differ)", "Observed traffic (since proxy startup; not auth success)", "Additional checks"}
	groups := make([][]DoctorLine, len(titles))
	for _, line := range r.Lines {
		i := doctorSection(line.Name)
		groups[i] = append(groups[i], line)
	}
	for i, lines := range groups {
		if len(lines) == 0 {
			continue
		}
		b.WriteByte('\n')
		heading(titles[i])
		for _, line := range lines {
			fmt.Fprintf(&b, "  %s %s: %s\n", status(line.Status), doctorDisplay(line.Name), doctorDisplay(line.Detail))
			if line.Missing != "" && line.Status != "ERROR" && line.Status != "MISSING" && line.Status != "WARN" {
				label := "note"
				if line.Status == "UNKNOWN" {
					label = "to verify"
				}
				fmt.Fprintf(&b, "    %s: %s\n", label, doctorDisplay(line.Missing))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func doctorSection(name string) int {
	switch name {
	case "config", "server", "server auth", "updates", "cc assignment", "cc sync state", "projects":
		return 0
	case "proxy daemon", "local proxy port", "cc watch", "ducklion", "cc runner mode", "agent binaries":
		return 1
	case "proxy config", "CA cert", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy", "CODEX_CA_CERTIFICATE", "SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE", "NODE_EXTRA_CA_CERTS":
		return 3
	}
	if strings.HasPrefix(name, "observed ") {
		return 4
	}
	if strings.HasPrefix(name, "Codex ") || strings.HasPrefix(name, "Claude ") || strings.HasPrefix(name, "environment ") {
		return 2
	}
	return 5
}

func doctorDisplay(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			q := strconv.QuoteRune(r)
			b.WriteString(q[1 : len(q)-1])
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}
