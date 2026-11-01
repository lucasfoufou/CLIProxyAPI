# Quota batch routing (local fork)

Enable `routing.strategy: quota-batch` for Codex subscription accounts. Other
providers retain round-robin routing. This strategy deliberately overrides
session affinity: a session cannot remain pinned beyond its account's batch.

## Policy

1. Prefer accounts with a 18,000-second (five-hour) window. A batch consumes
   **15 percentage points of that five-hour quota**, measured from the account's
   usage when the batch begins. Weekly consumption does not measure this batch.
2. Within a round, order accounts by the five-hour reset time, soonest first.
   Complete one batch per account before starting another round.
3. Keep 10% remaining in every applicable window, including the weekly window
   of a five-hour account. An earlier weekly safety threshold can stop a short
   batch, but a 15-point weekly usage change cannot complete a short batch.
4. When five-hour accounts have reached their safety threshold, use weekly-only
   accounts in **15 percentage-point batches of their weekly quota**, ordered by
   weekly reset time, with the same 10% reserve.
5. Only when all usable accounts reach their reserve, consume the final reserves
   one account at a time: five-hour accounts first, then weekly-only accounts;
   earliest reset first within each group. Return to normal capacity immediately
   when a quota resets.

The policy state spans models and sessions. Percentages are points of total
window capacity, not a percentage of the remaining balance. If a batch starts at
23% used, its normal endpoint is 38% used (or an earlier safety threshold).
A batch that starts at 85% used stops at 90% before consuming the reserve.

## Quota observations and limits

The selector fetches `/backend-api/wham/usage` through the registered Codex
executor, using its existing credentials and proxy transport. Full snapshots
are refreshed on demand at most every 30 seconds per credential. Passive HTTP
and WebSocket quota observations update the active batch between probes. A
passed reset forces a new probe. Small rounding differences in relative reset
headers do not restart a batch. Network failures never use stale quota data or
unlock reserves. Rejected credentials are excluded until a subsequent probe.

Accounting is based on provider observations. A request already running cannot
be split or undone; concurrent requests and delayed/rounded usage reports can
overshoot a threshold. The 15% batches and 10% safety margin are decisions at
request boundaries, not a hard upper bound on the consumption of a single turn.
Main Codex quota windows are supported; additional model-specific credit pools
are still subject to the provider's normal limits and error handling.

A transient retry exclusion or an explicitly pinned request cannot make other
normal capacity disappear from the reserve decision. Unsupported or unknown
quota windows block reserve spending. Do not enable a plugin scheduler alongside
this strategy: plugin scheduling is an upstream override of selector routing.

## WebSockets

Native continuations belong to an upstream account. At a batch boundary the
proxy uses its existing `upstream requires HTTP replay` close signal. Codex must
reconnect and replay full input; account-scoped `previous_response_id` values
are never sent to a new account. WebSocket connections remain available within
batches. Duplex response steering is disabled under this policy so every new
turn passes through the quota check.

## Persistence and operations

Progress is stored atomically with mode `0600` in
`<oauth.auth-dir>/.quota-batch-state`. It contains account file IDs, baseline
percentages, window IDs and round membership; no tokens. Corrupt or unwritable
state fails closed. Quota snapshots themselves are refreshed after restart.
Removing this file while the service is stopped starts a new rotation.

Selection logs include the auth file ID, tier (0=normal five-hour, 1=normal
weekly, 2=five-hour reserve, 3=weekly reserve), starting usage and reset timestamp.
The stock management UI may not list the custom strategy in its dropdown; use
its YAML/config editor and preserve `quota-batch` when saving settings.

The custom macOS service runs a separately installed binary. Homebrew updates
must not replace that binary with upstream. To roll back, stop the custom
LaunchAgent, restore the pre-deployment config and start the Homebrew service.
