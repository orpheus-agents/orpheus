package httpserver

import "testing"

func TestJSONBoundary(t *testing.T) {
	for _, raw := range []string{`{"a":1,"a":2}`, `{"a":{"b":1,"b":2}}`, `[] []`, `NaN`, `{"a":Infinity}`, "\xff"} {
		if validateJSON([]byte(raw)) == nil {
			t.Fatalf("accepted %q", raw)
		}
	}
	for _, raw := range []string{`{"a":[{"b":1}],"b":2}`, `"hello"`, `null`} {
		if err := validateJSON([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
}
