package httpserver

import (
	"encoding/json"
	"reflect"
	"testing"
)

func assertCatalogJSON(t *testing.T, raw []byte, expected string) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog:\n%s\nwant:\n%s", raw, expected)
	}
}
