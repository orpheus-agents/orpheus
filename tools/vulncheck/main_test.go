package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		name, id, module, version, function string
		ok                                  bool
	}{
		{"known false positive", "GO-2026-6909", "github.com/crewjam/saml", "v0.6.3", "ParseResponse", true},
		{"older version", "GO-2026-6909", "github.com/crewjam/saml", "v0.5.1", "ParseResponse", false},
		{"future version needs review", "GO-2026-6909", "github.com/crewjam/saml", "v0.6.4", "ParseResponse", false},
		{"other advisory", "GO-2026-9999", "github.com/crewjam/saml", "v0.6.3", "ParseResponse", false},
		{"other module", "GO-2026-6909", "example.com/other", "v0.6.3", "ParseResponse", false},
		{"stdlib", "GO-2026-6859", "stdlib", "v1.27.3", "MaxBytesReader", false},
		{"unreachable", "GO-2026-9999", "example.com/other", "v1.0.0", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := finding{OSV: tc.id, Trace: []frame{{Module: tc.module, Version: tc.version, Function: tc.function}}}
			data, err := json.Marshal(message{Finding: &f})
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			err = check(strings.NewReader(`{"config":{"scan_level":"symbol"}}{"osv":{"id":"GO-2026-6909","modified":"2026-11-05T22:55:15Z"}}`+string(data)), &output)
			if (err == nil) != tc.ok {
				t.Fatal(err, output.String())
			}
			if tc.ok && tc.function != "" && !strings.Contains(output.String(), "VERIFIED FALSE POSITIVE") {
				t.Fatal("exception was silent")
			}
		})
	}
}

func TestCheckRejectsIncompleteOrMalformedOutput(t *testing.T) {
	for _, input := range []string{"", `{"config":{"scan_level":"package"}}`, `{"config":{"scan_level":"symbol"}}{"finding":`, `{"config":{"scan_level":"symbol"}}{"finding":{"osv":"GO-2026-6909"}}`} {
		if err := check(strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	if err := check(strings.NewReader(`{"config":{"scan_level":"symbol"}}`), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestExceptionCannotHideAnotherFinding(t *testing.T) {
	input := `{"config":{"scan_level":"symbol"}}
 {"osv":{"id":"GO-2026-6909","modified":"2026-11-05T22:55:15Z"}}
 {"finding":{"osv":"GO-2026-6909","trace":[{"module":"github.com/crewjam/saml","version":"v0.6.3","function":"ParseResponse"}]}}
 {"finding":{"osv":"GO-2026-9999","trace":[{"module":"github.com/crewjam/saml","version":"v0.6.3","function":"ParseResponse"}]}}`
	if err := check(strings.NewReader(input), &bytes.Buffer{}); err == nil {
		t.Fatal("exception hid another advisory")
	}
}

func TestRevisedAdvisoryRequiresReassessment(t *testing.T) {
	for _, modified := range []string{"", "2026-11-06T22:55:15Z"} {
		input := `{"config":{"scan_level":"symbol"}}{"osv":{"id":"GO-2026-6909","modified":"` + modified + `"}}{"finding":{"osv":"GO-2026-6909","trace":[{"module":"github.com/crewjam/saml","version":"v0.6.3","function":"ParseResponse"}]}}`
		if err := check(strings.NewReader(input), &bytes.Buffer{}); err == nil {
			t.Fatal("accepted an unreviewed advisory revision")
		}
	}
}
