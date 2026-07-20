# Tasks

## 1. Loud outbound errors (bug #2: /send 502s silently)
- [x] 1.1 Add `channel.ErrNoOutbound` sentinel error.
- [x] 1.2 `webhookChannel.Send` returns `ErrNoOutbound` (wrapped) when `callbackURL` is empty.
- [x] 1.3 `server.send` maps `ErrNoOutbound` → 422 with structured JSON (`reason:"inbound_only"`); genuine delivery failures stay 502.
- [x] 1.4 Test: `/send` to an inbound-only channel → 422 + reason; `/send` to a callback channel whose peer 5xx's → 502.

## 2. Observable channel liveness (bug #1: whatsapp dies silently)
- [x] 2.1 Runtime records per-stream state (running, restarts, startedAt, lastErr, lastExitAt) in `supervise`.
- [x] 2.2 Expose `StreamHealth()` on Runtime; `/health` includes a `streams` map.
- [x] 2.3 Test: a fake streaming channel that fails then recovers is reflected in `/health` (running/restarts/lastErr).

## 3. CEO use-case end-to-end receipt
- [x] 3.1 New test boots a REAL hub on a free port (not httptest): inbound signed `/webhook/<name>` → durable inbox → live subscription Dispatcher delivers to a consumer (HMAC-signed); outbound `/send` → delivered + threaded reply; inbound-only `/send` → loud 422. (The symmetric `/hook/send` + `/hook/recv` round-trip is covered by the pre-existing `TestUniversalHook_SendRecvAndAuth`.)

## 4. Complementarity (organ ↔ organ)
- [x] 4.1 Prove `company log` output can be picked up by a messenger consumer (subscription/inbox), and document the honest channel map (crypto desk `#cryptoloop`, CEO).

## 5. Ops + docs
- [x] 5.1 Document + script the macOS codesign rebuild fix (bug #3).
- [x] 5.2 Update `docs/API.md` (422 send error, `/health` streams), `docs/ARCHITECTURE.md`, `README.md`.
- [x] 5.3 Document DLQ / head-of-line-blocking limitation as a follow-up.

## 6. Gate
- [x] 6.1 `CGO_ENABLED=0 go build ./... && go vet ./... && CGO_ENABLED=0 go test ./...` green.
- [x] 6.2 Huddle verify + cbrain retrospect.
