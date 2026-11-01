# Simple Usage dashboard

Open **`http://localhost:8317/usage.html`** on your CLIProxyAPI instance and enter its existing management key. No additional service, frontend build, account, or external database is required. Remote access follows the existing management API access policy. The page shell contains no usage data; every data request requires management authentication.

The dashboard offers:

- USD API cost estimates and token counts, with a cost/tokens switch.
- Rolling 24-hour, 7-day, 30-day, and 90-day windows; provider and upstream-account filters.
- An hourly chart and breakdowns by model, provider, account, or hour.
- Separate uncached input, cache read, cache write, and output totals and costs.
- Editable model prices, including existing long-context price thresholds.
- Automatic refresh every 30 seconds while the tab is visible.

Accounts use their configured label, or their OAuth email when available. Otherwise the page displays a stable opaque account identifier. Removed accounts remain in the history under that identifier. Account filters refer to upstream credentials, not client API keys. Sessions count distinct nonempty session identifiers; clients that do not send one still contribute usage.

## Persistence and operation

The runtime registers a usage sink independent of the transient usage queue, Home forwarding, and `usage-statistics-enabled`. Visiting the page is not necessary for collection. Events are written by the existing asynchronous usage dispatcher, rather than in the upstream response path. Execution IDs prevent duplicate insertion. Retries and fallbacks are separate attempts, and are labeled as such in the UI.

The default database is **`<auth-dir>/usage/usage.sqlite`**. With the bundled Docker Compose auth-directory volume, it survives container recreation. Keep the entire `usage` directory on a persistent local disk; SQLite uses WAL files alongside the database. Back it up with SQLite's online backup facilities, or stop the proxy before copying the directory.

Optional environment variables:

```sh
# Override the database file, for example on a dedicated volume:
USAGE_DASHBOARD_PATH=/data/usage.sqlite
# Disable both the page and its usage sink:
USAGE_DASHBOARD_DISABLED=true
```

Both settings take effect on restart. Events older than 90 days are pruned when traffic arrives, at most once an hour. Existing activity cannot be recovered retroactively from the transient queue. Home mode keeps the existing restriction on local management APIs and does not expose this page.

The database stores normalized token counts, model/provider, hashed credential identity, session ID, time, outcome, and latency. It does **not** store client keys, OAuth tokens, token fingerprints, headers, prompts, responses, or IP addresses. The management key stays in the page's JavaScript memory and is cleared on logout or reload.

## What the dollar amount means

This is a **standard API-equivalent estimate in USD**, including when requests use subscription/OAuth accounts. It is not the subscription invoice or a quota meter. Tool fees, image pricing, taxes, negotiated rates, and service-tier multipliers are excluded.

Initial reference rates come from the public [OpenRouter models API](https://openrouter.ai/api/v1/models), retrieved on 2026-10-31. The bundled snapshot covers OpenAI, Anthropic, Google, xAI, DeepSeek, Qwen, Moonshot and Mistral model IDs, plus explicit short aliases. Anthropic dotted model versions also have hyphenated aliases. No model-family prefix matching is used, so a new or differently named model is not silently assigned another model's rate. When the source omits a cache rate, the standard input rate is used as the reference estimate for that bucket.

**Tarifs** shows models used in the selected window. Search for any model, or use **Tous les tarifs**. You can add an unrecognized model by its exact reported ID, then enter its four rates per million tokens. Existing long-context tiers appear as additional editable rows. The page makes no external requests; the embedded catalog is not automatically refreshed.

Current rates apply to the entire selected history. Editing a price intentionally recalculates past estimates. This also lets you price a previously unknown model after collecting its usage. Costs include cache writes independently and do not count reasoning tokens a second time: reasoning is already part of output. Cache savings compare cache reads with their uncached input equivalent.

Missing model rates and incomplete/inconsistent upstream token accounting are explicitly marked as **unpriced attempts**; the total is labeled partial. They are not presented as free usage. Token and attempt counts remain visible.

## API

All endpoints use the existing authenticated v8 management route group:

- `GET /v8/management/observability/usage/summary?days=1&provider=&account=`
- `GET /v8/management/observability/usage/prices`
- `PUT /v8/management/observability/usage/prices`

`days` is an integer from 1 through 90. Summary responses contain `summary` (totals, grouped metrics, hourly buckets, filter options, unpriced model IDs) and `account_labels`. The `account` parameter is the opaque ID returned by the summary. Hour buckets use UTC; the page formats them in the viewer's local timezone. Empty hours render as zero.

A price update is a partial upsert of an array of model rates; it does not delete other models:

```json
[
  {
    "model": "my-model",
    "input": 2,
    "cache_read": 0.2,
    "cache_write": 2.5,
    "output": 10,
    "overrides": [
      { "min_input": 200000, "input": 4, "cache_read": 0.4, "cache_write": 5, "output": 15 }
    ]
  }
]
```

Thresholds must be strictly increasing. Prices must be finite, nonnegative USD-per-million values. An explicit zero is supported for a genuinely free model. Requests are limited to 2 MiB and 2,000 price entries.

Summary generation streams events in the selected time range into aggregates, keeping raw events out of the response. This deliberately simple implementation targets a personal proxy; very large installations may need SQL preaggregation.

SDK applications that create `api.Server` directly should call `StartUsageDashboard()` when starting their usage runtime and `CloseUsageDashboard()` after stopping and draining it. The standard CLI service handles both automatically.
