package store

import (
	"encoding/json"
	"testing"
)

func TestJSONDecimalEquivalence(t *testing.T) {
	for _, pair := range [][2]string{{"1e3", "1000"}, {"1.00", "1"}, {"-0.00", "0"}, {"0.0012300", "123e-5"}, {"9007199254740993.0", "9007199254740993"}, {"1e999999999999", "10e999999999998"}} {
		a, b := json.RawMessage(`{"value":[`+pair[0]+`]}`), json.RawMessage(`{"value":[`+pair[1]+`]}`)
		if !jsonEqual(a, b) || resultDigest(a) != resultDigest(b) {
			t.Fatalf("decimal representations differ: %v", pair)
		}
	}
	for _, pair := range [][2]string{{"9007199254740992", "9007199254740993"}, {"0.00000000000000001", "0"}, {"1", "-1"}, {`"1"`, "1"}} {
		if jsonEqual([]byte(pair[0]), []byte(pair[1])) || resultDigest([]byte(pair[0])) == resultDigest([]byte(pair[1])) {
			t.Fatalf("different values collapsed: %v", pair)
		}
	}
}
