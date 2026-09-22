// Package diagnostic reports error identity without native messages or secrets.
package diagnostic

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func Describe(err error) string {
	var parts []string
	if errors.Is(err, context.DeadlineExceeded) {
		parts = append(parts, "deadline_exceeded")
	}
	if errors.Is(err, context.Canceled) {
		parts = append(parts, "cancelled")
	}
	for err != nil {
		part := fmt.Sprintf("%T", err)
		if state, ok := err.(interface{ SQLState() string }); ok {
			part += " SQLSTATE=" + state.SQLState()
		}
		parts = append(parts, part)
		err = errors.Unwrap(err)
	}
	return strings.Join(parts, ": ")
}
