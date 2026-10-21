package store

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/orpheus-agents/orpheus/internal/harness"
)

const replacementCharacter = "\uFFFD"

var jsonNUL = []byte(`\u0000`)

func sanitizeText(value string) string {
	return strings.ReplaceAll(value, "\x00", replacementCharacter)
}

// sanitizeJSON replaces JSON NUL escapes that PostgreSQL jsonb cannot store.
// An escaped backslash followed by u0000 is ordinary text and stays unchanged.
func sanitizeJSON(value json.RawMessage) json.RawMessage {
	if !bytes.Contains(value, jsonNUL) {
		return value
	}
	clean := make([]byte, 0, len(value))
	for index := 0; index < len(value); {
		if value[index] != '\\' {
			clean = append(clean, value[index])
			index++
			continue
		}
		start := index
		for index < len(value) && value[index] == '\\' {
			index++
		}
		clean = append(clean, value[start:index]...)
		if (index-start)%2 == 0 || index+5 > len(value) || !bytes.Equal(value[index:index+5], jsonNUL[1:]) {
			continue
		}
		clean = clean[:len(clean)-1]
		clean = append(clean, `\ufffd`...)
		index += 5
	}
	return clean
}

func sanitizeSnapshot(snapshot *harness.Snapshot) {
	if snapshot.Path != nil {
		snapshot.Path = new(sanitizeText(*snapshot.Path))
	}
	snapshot.Turns = slices.Clone(snapshot.Turns)
	for turnIndex := range snapshot.Turns {
		turn := &snapshot.Turns[turnIndex]
		turn.NativeID = sanitizeText(turn.NativeID)
		turn.ErrorCode = sanitizeText(turn.ErrorCode)
		turn.ErrorMessage = sanitizeText(turn.ErrorMessage)
		turn.Items = slices.Clone(turn.Items)
		for itemIndex := range turn.Items {
			item := &turn.Items[itemIndex]
			item.NativeID = sanitizeText(item.NativeID)
			item.Type = sanitizeText(item.Type)
			item.Text = sanitizeText(item.Text)
			if item.Kind != nil {
				item.Kind = new(sanitizeText(*item.Kind))
			}
			item.Name = sanitizeText(item.Name)
			item.Input = sanitizeJSON(item.Input)
			item.Status = sanitizeText(item.Status)
			item.Result = sanitizeJSON(item.Result)
			item.Completeness = sanitizeText(item.Completeness)
			if item.TruncationReason != nil {
				item.TruncationReason = new(sanitizeText(*item.TruncationReason))
			}
		}
	}
}

func sanitizeUsageReports(reports []harness.UsageReport) []harness.UsageReport {
	reports = slices.Clone(reports)
	for index := range reports {
		reports[index].ContextID = sanitizeText(reports[index].ContextID)
		reports[index].TurnID = sanitizeText(reports[index].TurnID)
	}
	return reports
}
