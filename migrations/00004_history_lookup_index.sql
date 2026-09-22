-- +goose Up
-- Build after event data migration to avoid maintaining this expression index
-- while adding nullable metadata to existing history.
CREATE INDEX events_message_snapshot ON session_events(session_id, (data->>'id'), sequence DESC) WHERE type = 'message.updated';

-- +goose Down
DROP INDEX events_message_snapshot;
