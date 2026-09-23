package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skillum-ai/orpheus/internal/session"
	"github.com/skillum-ai/orpheus/internal/store/db"
)

type cursorEnvelope struct {
	Scope    string          `json:"scope"`
	Position json.RawMessage `json:"position"`
}

func encodeCursor(scope string, position any) string {
	data, _ := json.Marshal(position)
	raw, _ := json.Marshal(cursorEnvelope{scope, data})
	return base64.RawURLEncoding.EncodeToString(raw)
}
func decodeCursor(value, scope string, dest any) error {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return invalidCursor()
	}
	var env cursorEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Scope != scope {
		return invalidCursor()
	}
	// No cursor component is nullable. encoding/json otherwise silently turns
	// null integers (including array elements) into zero.
	decoder := json.NewDecoder(bytes.NewReader(env.Position))
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || token == nil {
			return invalidCursor()
		}
	}
	decoder = json.NewDecoder(bytes.NewReader(env.Position))
	decoder.DisallowUnknownFields()
	if decoder.Decode(dest) != nil {
		return invalidCursor()
	}
	return nil
}
func invalidCursor() error { return session.Problem(422, "invalid_cursor", "Invalid cursor.") }
func validLimit(limit int) error {
	if limit < 1 || limit > 200 {
		return session.Problem(422, "validation_error", "Invalid page limit.")
	}
	return nil
}
func (s *Store) Session(ctx context.Context, id uuid.UUID) (session.Session, error) {
	var out session.Session
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		r, err := GetSession(ctx, tx, id, false)
		if err != nil {
			return err
		}
		out, err = SessionView(ctx, tx, r)
		return err
	})
	return out, err
}
func (s *Store) Run(ctx context.Context, sid, rid uuid.UUID) (session.Run, error) {
	var out session.Run
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		if _, err := GetSession(ctx, tx, sid, false); err != nil {
			return err
		}
		r, err := GetRun(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		out, err = RunView(ctx, tx, r)
		return err
	})
	return out, err
}
func (s *Store) ListSessions(ctx context.Context, limit int, cursor string, filter ListFilter) (session.Page[session.Session], error) {
	out := session.Page[session.Session]{Items: []session.Session{}}
	if err := validLimit(limit); err != nil {
		return out, err
	}
	scope, err := filter.scope("sessions")
	if err != nil {
		return out, err
	}
	type position struct {
		Date time.Time
		ID   uuid.UUID
	}
	var pos position
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &pos); err != nil {
			return out, err
		}
		if pos.Date.IsZero() || pos.ID == uuid.Nil {
			return out, invalidCursor()
		}
	}
	err = s.Snapshot(ctx, func(tx pgx.Tx) error {
		args := db.ListSessionsParams{Namespace: filter.Namespace, ExternalKey: filter.ExternalKey, Status: filter.Status, FirstPage: cursor == "", AfterCreatedAt: pos.Date, AfterID: pos.ID, PageLimit: limit + 1}
		var records []SessionRecord
		var err error
		if filter.Order == "desc" {
			records, err = sessionRecords(db.New(tx).ListSessionsDesc(ctx, db.ListSessionsDescParams(args)))
		} else {
			records, err = sessionRecords(db.New(tx).ListSessions(ctx, args))
		}
		if err != nil {
			return err
		}
		for _, r := range records[:min(limit, len(records))] {
			v, err := SessionView(ctx, tx, r)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, v)
		}
		if len(records) > limit {
			r := records[limit-1]
			out.NextCursor = new(encodeCursor(scope, position{r.CreatedAt, r.ID}))
		}
		return nil
	})
	return out, err
}
func (s *Store) ListAllRuns(ctx context.Context, limit int, cursor string, filter ListFilter) (session.Page[session.Run], error) {
	out := session.Page[session.Run]{Items: []session.Run{}}
	if err := validLimit(limit); err != nil {
		return out, err
	}
	scope, err := filter.scope("runs")
	if err != nil {
		return out, err
	}
	type position struct {
		Date time.Time
		ID   uuid.UUID
	}
	var pos position
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &pos); err != nil {
			return out, err
		}
		if pos.Date.IsZero() || pos.ID == uuid.Nil {
			return out, invalidCursor()
		}
	}
	err = s.Snapshot(ctx, func(tx pgx.Tx) error {
		args := db.ListAllRunsParams{InputFingerprint: filter.InputFingerprint, Namespace: filter.Namespace, ExternalKey: filter.ExternalKey, Status: filter.Status, FirstPage: cursor == "", AfterCreatedAt: pos.Date, AfterID: pos.ID, PageLimit: limit + 1}
		var records []RunRecord
		var err error
		if filter.Order == "desc" {
			records, err = runRecords(db.New(tx).ListAllRunsDesc(ctx, db.ListAllRunsDescParams(args)))
		} else {
			records, err = runRecords(db.New(tx).ListAllRuns(ctx, args))
		}
		if err != nil {
			return err
		}
		out.Items, err = RunViews(ctx, tx, records[:min(limit, len(records))])
		if err != nil {
			return err
		}
		if len(records) > limit {
			r := records[limit-1]
			out.NextCursor = new(encodeCursor(scope, position{r.CreatedAt, r.ID}))
		}
		return nil
	})
	return out, err
}
func (s *Store) ListRuns(ctx context.Context, sid uuid.UUID, limit int, cursor string, filter ListFilter) (session.Page[session.Run], error) {
	out := session.Page[session.Run]{Items: []session.Run{}}
	if err := validLimit(limit); err != nil {
		return out, err
	}
	scope, err := filter.scope("runs:" + sid.String())
	if err != nil {
		return out, err
	}
	position := 0
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &position); err != nil {
			return out, err
		}
		if position < 0 {
			return out, invalidCursor()
		}
	}
	err = s.Snapshot(ctx, func(tx pgx.Tx) error {
		if _, err := GetSession(ctx, tx, sid, false); err != nil {
			return err
		}
		args := db.ListRunsParams{InputFingerprint: filter.InputFingerprint, Status: filter.Status, FirstPage: cursor == "", SessionID: sid, AfterNumber: position, PageLimit: limit + 1}
		var records []RunRecord
		var err error
		if filter.Order == "desc" {
			records, err = runRecords(db.New(tx).ListRunsDesc(ctx, db.ListRunsDescParams(args)))
		} else {
			records, err = runRecords(db.New(tx).ListRuns(ctx, args))
		}
		if err != nil {
			return err
		}
		out.Items, err = RunViews(ctx, tx, records[:min(limit, len(records))])
		if err != nil {
			return err
		}
		if len(records) > limit {
			out.NextCursor = new(encodeCursor(scope, records[limit-1].Number))
		}
		return nil
	})
	return out, err
}
func (s *Store) Events(ctx context.Context, sid uuid.UUID, after string, limit int) (session.EventPage, error) {
	out := session.EventPage{Items: []session.Event{}, NextCursor: after}
	if err := validLimit(limit); err != nil {
		return out, err
	}
	position, err := strconv.ParseInt(after, 10, 64)
	if err != nil || len(after) == 0 || len(after) > 19 || position < 0 {
		return out, invalidCursor()
	}
	for _, r := range after {
		if r < '0' || r > '9' {
			return out, invalidCursor()
		}
	}
	err = s.Snapshot(ctx, func(tx pgx.Tx) error {
		record, err := GetSession(ctx, tx, sid, false)
		if err != nil {
			return err
		}
		if position >= record.NextEventSequence {
			return invalidCursor()
		}
		rows, err := db.New(tx).ListEvents(ctx, db.ListEventsParams{SessionID: sid, Sequence: position, PageLimit: limit + 1})
		if err != nil {
			return err
		}
		for _, row := range rows {
			e := session.Event{ID: row.Sequence, SessionID: row.SessionID, Type: row.Type, CreatedAt: row.CreatedAt, Data: row.Data}
			if len(out.Items) == limit {
				out.HasMore = true
				break
			}
			out.Items = append(out.Items, e)
			out.NextCursor = e.ID
		}
		return nil
	})
	return out, err
}
func (s *Store) History(ctx context.Context, sid uuid.UUID, rid *uuid.UUID, limit int, cursor string, messageExternalKey *string) (session.HistoryPage, error) {
	out := session.HistoryPage{Page: session.Page[session.HistoryItem]{Items: []session.HistoryItem{}}}
	if err := validLimit(limit); err != nil {
		return out, err
	}
	if err := session.ValidateExternal(messageExternalKey, session.ExternalKeyMaxBytes, "query", "message_external_key"); err != nil {
		return out, err
	}
	scope := historyScope(sid, rid, messageExternalKey)
	type position struct {
		Watermark *int64
		Last      []int64
	}
	var pos position
	if cursor != "" {
		if err := decodeCursor(cursor, scope, &pos); err != nil {
			return out, err
		}
		if pos.Watermark == nil || *pos.Watermark < 0 || len(pos.Last) != 4 {
			return out, invalidCursor()
		}
		for _, v := range pos.Last {
			if v < 0 {
				return out, invalidCursor()
			}
		}
	}
	err := s.Snapshot(ctx, func(tx pgx.Tx) error {
		record, err := GetSession(ctx, tx, sid, false)
		if err != nil {
			return err
		}
		if rid != nil {
			if _, err := GetRun(ctx, tx, sid, *rid); err != nil {
				return err
			}
		}
		if cursor == "" {
			pos.Watermark = new(record.NextEventSequence - 1)
			pos.Last = []int64{0, 0, 0, 0}
		}
		if *pos.Watermark >= record.NextEventSequence {
			return invalidCursor()
		}
		out.EventCursor = strconv.FormatInt(*pos.Watermark, 10)
		params := db.HistoryParams{
			SessionID:     sid,
			Watermark:     *pos.Watermark,
			RunID:         rid,
			FirstPage:     cursor == "",
			AfterRun:      pos.Last[0],
			AfterUnknown:  pos.Last[1],
			AfterIndex:    pos.Last[2],
			AfterSequence: pos.Last[3],
			PageLimit:     limit + 1,
		}
		var rows []db.HistoryRow
		if messageExternalKey == nil {
			rows, err = db.New(tx).History(ctx, params)
		} else {
			var filtered []db.HistoryByExternalKeyRow
			filtered, err = db.New(tx).HistoryByExternalKey(ctx, db.HistoryByExternalKeyParams{
				SessionID: sid, RunID: rid, MessageExternalKey: *messageExternalKey,
				Watermark: params.Watermark, FirstPage: params.FirstPage, AfterRun: params.AfterRun,
				AfterUnknown: params.AfterUnknown, AfterIndex: params.AfterIndex, AfterSequence: params.AfterSequence, PageLimit: params.PageLimit,
			})
			rows = make([]db.HistoryRow, len(filtered))
			for i, row := range filtered {
				rows[i] = db.HistoryRow(row)
			}
		}
		if err != nil {
			return err
		}
		var last []int64
		for _, row := range rows {
			kind, raw := row.Type, row.Data
			next := []int64{int64(row.Rn), int64(row.Unknown), row.Idx, row.Seq}
			if len(out.Items) == limit {
				out.NextCursor = new(encodeCursor(scope, position{pos.Watermark, last}))
				break
			}
			item := session.HistoryItem{}
			if kind == "message.updated" {
				item.Type = "message"
				item.Message = new(session.Message)
				err = json.Unmarshal(raw, item.Message)
			} else {
				item.Type = "tool_call"
				item.ToolCall = new(session.ToolCall)
				err = json.Unmarshal(raw, item.ToolCall)
			}
			if err != nil {
				return err
			}
			out.Items = append(out.Items, item)
			last = next
		}
		return nil
	})
	return out, err
}

func historyScope(sid uuid.UUID, rid *uuid.UUID, externalKey *string) string {
	raw, _ := json.Marshal(struct {
		Resource    string
		SessionID   uuid.UUID
		RunID       *uuid.UUID
		ExternalKey *string
	}{"history", sid, rid, externalKey})
	return string(raw)
}
