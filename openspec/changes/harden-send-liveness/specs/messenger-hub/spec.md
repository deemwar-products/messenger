# messenger-hub

## ADDED Requirements

### Requirement: Outbound-incapable channels fail loudly, not as a gateway error

When `POST /send` targets a channel that cannot deliver outbound because it is configured
inbound-only (a webhook channel with no `options.callbackURL`), the hub SHALL respond with
**HTTP 422 Unprocessable Entity** and a structured JSON body naming the channel and the
reason `inbound_only`, distinguishing an unfixable-by-retry configuration precondition from
a transient upstream gateway failure. Genuine outbound delivery failures — the callback is
unreachable or returns a non-2xx status — SHALL continue to respond with **HTTP 502**.

#### Scenario: /send to an inbound-only webhook channel

- **WHEN** a client `POST /send` with `{"channel":"hook","text":"hi"}` and the `hook`
  channel has no `callbackURL`
- **THEN** the response status is `422`
- **AND** the JSON body has `ok:false` and `reason:"inbound_only"` and names the channel

#### Scenario: /send to a channel whose downstream fails

- **WHEN** a client `POST /send` to a webhook channel whose `callbackURL` returns HTTP 500
- **THEN** the response status is `502`

### Requirement: Channel liveness is observable via /health

`GET /health` SHALL report, in addition to the configured `channels` map, a `streams` map
keyed by streaming-channel kind whose value carries at minimum whether the stream is
currently `running`, its cumulative `restarts`, and the last error text (if any). This lets
a monitor detect a listener that has died or is stuck in a restart loop without inspecting
host processes.

#### Scenario: a streaming channel that failed is visible

- **WHEN** a streaming channel's `Run` has exited with an error and the supervisor is
  backing off before a restart
- **THEN** `GET /health` `streams.<kind>` reports `running:false` (or a nonzero `restarts`)
  and the last error text
- **AND** after a healthy restart the same entry reports `running:true`

## MODIFIED Requirements

### Requirement: Durable at-least-once consumer delivery

The hub SHALL deliver every inbound envelope to each enabled subscription IN ORDER via a
per-consumer on-disk cursor advanced only after a 2xx push, retrying with exponential
backoff — persistent store + ack-on-success + retry, the three components of at-least-once
delivery. This behavior is unchanged; the hub additionally DOCUMENTS the known limitation
that a permanently-failing consumer blocks its own queue head-of-line (no dead-letter), and
records dead-letter-after-N-attempts as a follow-up.

#### Scenario: a consumer that was down catches up

- **WHEN** a subscription's endpoint is unreachable for several inbound messages and then
  recovers
- **THEN** on recovery it receives every missed message in order, exactly from its cursor,
  with no gaps

#### Scenario: signed delivery over the symmetric hook

- **WHEN** a signed peer `POST /hook/send` with a valid `X-Hub-Signature-256`
- **THEN** the message is injected into the inbox and fanned out to subscriptions
- **AND** an invalid signature is rejected with `401` and nothing is injected
