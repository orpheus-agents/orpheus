//go:build integration

package store

import (
	"errors"
	"testing"
)

func TestThreadDispatchState(t *testing.T) {
	for _, status := range []string{"pending", "sending", "uncertain", "confirmed", "failed"} {
		for _, thread := range []any{"thread", "another-thread", nil, 42, ""} {
			t.Run(status+"/"+testThreadName(thread), func(t *testing.T) {
				s := fixture(t)
				a, err := s.Accept(t.Context(), request())
				if err != nil {
					t.Fatal(err)
				}
				o, err := s.Operation(t.Context(), a.SessionID, "start", &a.RunID, &a.MessageID, map[string]any{"thread_id": thread})
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetOperation(t.Context(), a.SessionID, o.ID, status, nil); err != nil {
					t.Fatal(err)
				}
				got, err := s.ThreadHasDispatch(t.Context(), a.SessionID, "thread")
				possible := status != "pending" && status != "failed"
				valid := thread == "thread" || thread == "another-thread"
				if errors.Is(err, ErrThreadDispatchInvalid) != (possible && !valid) || got != (possible && thread == "thread") {
					t.Fatalf("dispatch=%t err=%v", got, err)
				}
			})
		}
	}
}

func testThreadName(value any) string {
	if s, ok := value.(string); ok && s != "" {
		return s
	}
	if value == nil {
		return "null"
	}
	if value == "" {
		return "empty"
	}
	return "invalid-type"
}

func TestOrphanNativeTurnDoesNotAllowDispatch(t *testing.T) {
	s := fixture(t)
	a, err := s.Accept(t.Context(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pool.Exec(t.Context(), "UPDATE runs SET native_turn_id='native' WHERE id=$1", a.RunID); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ThreadHasDispatch(t.Context(), a.SessionID, "thread"); got || !errors.Is(err, ErrThreadDispatchInvalid) {
		t.Fatalf("orphan native turn allowed dispatch: %t %v", got, err)
	}
}
