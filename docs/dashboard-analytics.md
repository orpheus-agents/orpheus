# Dashboard analytics API

`GET /api/v1/analytics/overview` returns a PostgreSQL snapshot for a dashboard. It accepts a Bearer API key or the read-only browser access configured in [browser authentication](browser-auth.md). The response has `Cache-Control: no-store`.

```sh
curl -H 'Authorization: Bearer <key>' \
  'http://localhost:8000/api/v1/analytics/overview?window=7d&bucket=day&timezone=Europe%2FMoscow'
```

The default query is a sliding 24-hour window with hourly UTC buckets. Choose `window=24h|7d|30d|3m|6m|1y`, or supply both `from` and `to` as RFC 3339 timestamps with offsets. The longer presets subtract 3, 6 or 12 calendar months in UTC, clamp to the last day of the target month when needed, and use daily buckets. Explicit ranges are `[from,to)`, span at most 31 days and end no later than `as_of`. Omitting the exact `namespace` filter includes all namespaces. The response contains the database snapshot time `as_of` and the resolved range.

`period` counts **runs created in the range**. `by_status` reports their status at `as_of`; the sum of the eight statuses equals `runs_count`. `usage` sums the stored run counters, including usage recorded after `to`; its values are decimal strings because an aggregate can exceed JavaScript's safe integer range. `cached_input_tokens` is included in `input_tokens`, and `reasoning_output_tokens` is included in `output_tokens`. The breakdown starts when its collection is enabled; earlier usage is not reconstructed. `runtime_seconds` sums each selected run's lifetime from creation to its finish or `as_of`. Parallel runs add their durations. `current.active_sessions` counts sessions with an unfinished run at `as_of`, regardless of the selected range.

`namespaces` groups the same runs by their session namespace, most runs first. Each group carries `runs_count`, `by_status`, `usage` and `runtime_seconds` with the definitions of `period`; `null` is the group of sessions without a namespace, and the groups add up to `period`. With a `namespace` filter the list has at most one group.

`series` covers the whole range with zero-filled local calendar hours or days. The first and last buckets may be partial. Bucket timestamps are UTC, so repeated local hours at a daylight-saving transition have distinct `from` values. A response contains at most 800 buckets.

`GET /api/v1/sessions` filters by activity, latest run status, external key and a paired `last_run_created_from` / `last_run_created_to` range of at most 31 days. Use `sort=created_at|last_run_created_at` to choose the timestamp for sorting.

- `namespace`, `profile` and `model` match exact values. Profile and model come from the stored session configuration.
- `namespace_contains` and `model_contains` match literal substrings without regard to case.

Filters combine with AND. The live list cursor binds all filters and the sort order. Start from page one when changing any of them.

For example, current sessions ordered by their latest run:

```sh
curl -H 'Authorization: Bearer <key>' \
  'http://localhost:8000/api/v1/sessions?activity=active&sort=last_run_created_at&order=desc'
```

The optional `integration perf` test builds 100,000 sessions and 1,000,000 runs, with 20 active sessions, then records timings and `EXPLAIN ANALYZE` plans. On a local Apple M5 Max with a Docker Linux/aarch64 VM (18 CPUs, 16 GiB), the 24h/hour snapshot took 0.13 s, 30d/day took 0.54 s across all namespaces and 0.51 s in the popular namespace; a rare namespace took 0.03 s. The active page of 20 took 0.003 s; an inactive page of 50 took 0.016 s. The 30d SQL plan used `runs_created` (572 ms execution); the active list used `runs_one_unfinished_per_session`, `runs_session_id_number_key` and `sessions_pkey` (0.2 ms execution). These are local fixture measurements, not a production latency guarantee. Reproduce with `go test -tags 'integration perf' ./internal/store -run '^TestAnalyticsLargeArchive$' -count=1 -v` in the tools container.
