// Command vulncheck runs govulncheck with a version-specific false-positive exception.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type frame struct {
	Module   string `json:"module"`
	Version  string `json:"version"`
	Function string `json:"function"`
}
type finding struct {
	OSV   string  `json:"osv"`
	Trace []frame `json:"trace"`
}
type message struct {
	OSV *struct {
		ID       string `json:"id"`
		Modified string `json:"modified"`
	} `json:"osv"`
	Config *struct {
		ScanLevel string `json:"scan_level"`
	} `json:"config"`
	Finding *finding `json:"finding"`
}

func main() {
	command := exec.Command("govulncheck", "-json", "./...")
	command.Stderr = os.Stderr
	data, err := command.Output()
	if err == nil {
		err = check(bytes.NewReader(data), os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func check(input io.Reader, output io.Writer) error {
	decoder := json.NewDecoder(input)
	configured := false
	blocked := map[string]bool{}
	reported := map[string]bool{}
	advisoryModified := ""
	for {
		var msg message
		if err := decoder.Decode(&msg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return fmt.Errorf("decode govulncheck output: %w", err)
		}
		if msg.Config != nil {
			if msg.Config.ScanLevel != "symbol" {
				return errors.New("govulncheck must run at symbol level")
			}
			configured = true
		}
		if msg.OSV != nil && msg.OSV.ID == "GO-2026-6909" {
			advisoryModified = msg.OSV.Modified
		}
		if msg.Finding == nil {
			continue
		}
		f := msg.Finding
		if f.OSV == "" || len(f.Trace) == 0 || f.Trace[0].Module == "" {
			return errors.New("invalid govulncheck finding")
		}
		// Like govulncheck's text mode, module/package-only findings are informational.
		if f.Trace[0].Function == "" {
			continue
		}
		key := f.OSV + " " + f.Trace[0].Module + "@" + f.Trace[0].Version
		ignored := falsePositive(*f, advisoryModified)
		if !ignored {
			blocked[key] = true
		}
		if !reported[key] {
			status := "REACHABLE"
			if ignored {
				status = "VERIFIED FALSE POSITIVE (docs/vulnerability-checks.md)"
			}
			if _, err := fmt.Fprintf(output, "%s: %s https://pkg.go.dev/vuln/%s\n", status, key, f.OSV); err != nil {
				return err
			}
			reported[key] = true
		}
	}
	if !configured {
		return errors.New("missing govulncheck scan configuration")
	}
	if len(blocked) > 0 {
		return fmt.Errorf("%d reachable vulnerabilities require fixes", len(blocked))
	}
	_, err := fmt.Fprintln(output, "No unaddressed reachable vulnerabilities.")
	return err
}

func falsePositive(f finding, modified string) bool {
	// GO-2026-6909 has no fixed version in its database range. In v0.6.3,
	// Assertion.Issuer is a value and Subject.NameID has the SAML namespace.
	// TestSAMLInvalidAssertions covers both reported attacks through our callback.
	// Do not extend this to other advisories or versions without new evidence.
	return f.OSV == "GO-2026-6909" && f.Trace[0].Module == "github.com/crewjam/saml" && f.Trace[0].Version == "v0.6.3" && modified == "2026-11-05T22:55:15Z"
}
