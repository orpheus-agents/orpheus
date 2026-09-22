package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/skillum-ai/orpheus/internal/api"
	"github.com/skillum-ai/orpheus/internal/session"
)

func TestGeneratedToolResultRoundTrip(t *testing.T) {
	for _, result := range []string{`null`, `{"type":"text","text":"hello","exit_code":null,"original_bytes":5}`, `{"type":"json","value":{"n":9007199254740993},"original_bytes":22}`, `{"type":"truncated_text","head":"a","tail":"z","source_type":"text","exit_code":0,"original_bytes":100}`} {
		t.Run(result, func(t *testing.T) {
			dto, err := mapResponse[api.ToolCall](session.ToolCall{Input: json.RawMessage(`{"n":9007199254740993}`), Result: json.RawMessage(result)})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(dto)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Input  json.RawMessage
				Result json.RawMessage
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if string(got.Result) != result || !strings.Contains(string(got.Input), "9007199254740993") {
				t.Fatalf("generated DTO lost data: %s", raw)
			}
			if strings.Contains(result, `"type":"json"`) {
				decoded, err := dto.Result.AsJSONResult()
				if err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(decoded)
				if err != nil || !strings.Contains(string(raw), "9007199254740993") {
					t.Fatal(string(raw), err)
				}
			}
		})
	}
}
