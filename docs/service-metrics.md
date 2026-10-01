# Service metrics

`orpheus serve` exposes `GET /metrics/service` on its internal system listener
(`ORPHEUS_SYSTEM_HOST` / `ORPHEUS_SYSTEM_PORT`, default port 9100). It requires no
authentication. Keep this port private; the public API listener does not serve
metrics. `/health` and `/ready` remain process probes independent of the database.

Each scrape reads one database snapshot through `Store.AccountLimits`, just like
`GET /api/v1/accounts/limits`. It never contacts Codex, reads credentials or wakes
a sandbox. Two API replicas read the same database; collect these metrics once
through the service, rather than once per pod. Normal updates and freshness
boundaries can produce different values in requests made at different times.

Prometheus text and OpenMetrics are supported through content negotiation.
Responses use `Cache-Control: no-store`. Database errors or the three-second read
deadline produce HTTP 503, without partial metrics, zero substitutes or a cached
success. The system HTTP write timeout leaves two seconds after that deadline.
The registry contains only service metrics, without process, Go or scrape counters.

| Gauge | Labels | Meaning |
| --- | --- | --- |
| `orpheus_account_limit_remaining_percent` | `account_id, limit_id, window` | Last observed remaining quota percentage |
| `orpheus_account_limit_state` | `account_id, state` | Exactly one of four account states is 1; the other three are 0 |
| `orpheus_account_limit_observed_timestamp_seconds` | `account_id` | Unix seconds of the last successful provider observation |
| `orpheus_account_limit_reset_timestamp_seconds` | `account_id, limit_id, window` | Last observed Unix seconds of the next automatic window reset |
| `orpheus_account_reset_credits_available` | `account_id` | Last observed count of available earned resets |

`window` is `primary` or `secondary`, not an assumed five-hour/weekly duration.
States are `fresh`, `stale`, `unknown`, `unavailable`; see
[account limits](account-limits.md) for collection and freshness rules.

Unknown counts and timestamps have no series. A confirmed zero resets produces
a series with value 0. Missing windows produce no percentage series. Stale
accounts retain their last known numbers together with their old observation
time. A successful scrape does not refresh provider data. Removed accounts,
windows and values disappear on the next successful scrape. API-key-only
installations successfully return no account metric series.

## Kubernetes collection

Expose the system listener through an internal ClusterIP Service and configure
your scraper to read `/metrics/service` once per installation. If the container
port is named `system`, use `targetPort: system` on the Service's metrics port.
Keep the port private; do not publish it through an HTTPRoute/Ingress. Configure
service discovery labels, scrape interval and timeout in your monitoring setup.

Check the target with `up`. Replace the example `job` and
`app_kubernetes_io_name` selectors below with labels from your scraper:

```promql
up{job="service-metrics", app_kubernetes_io_name="orpheus"}
```

## Low-quota alert

Example for one installation, to be added to its monitoring configuration.
Replace `job` and `app_kubernetes_io_name` with your target selectors in both
parts of the expression:

```yaml
alert: OrpheusAccountQuotaLow
expr: |
  (
    orpheus_account_limit_remaining_percent{
      job="service-metrics", app_kubernetes_io_name="orpheus"
    } <= 7
  )
  and on (account_id)
  (
    orpheus_account_limit_state{
      job="service-metrics", app_kubernetes_io_name="orpheus", state="fresh"
    } == 1
  )
for: 2m
labels:
  severity: warning
```

For multiple installations, restrict both selectors to the target cluster and
namespace, or include the installation labels in vector matching. No pod
deduplication is needed with service collection. Available resets do not suppress
this alert; display their count on the dashboard alongside quota windows.

The rule matches 7% and below, only while the account snapshot is fresh. Without
an active Codex donor, observations normally become stale after five minutes.
An alert disappearing because data became stale does not establish quota
recovery. Missing active sessions are not an incident. Monitor scrape failures
through the installation's usual `up` rules, separately from quota freshness.

Publishing this endpoint does not install an alert rule, a Grafana dashboard or
notification routing. Those are managed by the installation's monitoring team.
