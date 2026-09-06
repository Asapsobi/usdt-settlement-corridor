-- C3.3's discovery mechanism: discovery_cursor is the single-row
-- bookmark (same pattern as depositwatcher's own ingestion_cursor) for
-- how far RunDiscoveryLoop has walked C1's GET /v1/orders?state=funded
-- pagination; screening_queue is every order discovered that way, plus
-- its resolved sender address once known.
--
-- sender_address starts NULL and is resolved by a SEPARATE retry pass,
-- not gated on discovery itself: an order is enqueued the moment C1
-- reports it as funded, whether or not the immediate GetSenderAddress
-- call that tick happened to succeed. This is deliberate -- see
-- internal/discovery/loop.go's own doc comment -- so a transient lookup
-- failure can never cause an order to be silently skipped by the
-- cursor moving past it.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE discovery_cursor (
    id          smallint primary key default 1 check (id = 1),
    cursor      text null,
    updated_at  timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Seeded here, not left for application code to handle a missing row --
-- same reasoning as depositwatcher's own ingestion_cursor seed: the Go
-- side should always be able to assume the single row exists and just
-- UPDATE it, never branch on "does it exist yet". cursor starts NULL,
-- meaning "poll from the beginning" -- there is no earlier state to
-- resume from on a fresh deployment.
INSERT INTO discovery_cursor (id, cursor) VALUES (1, NULL);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE screening_queue (
    order_id        bigint primary key,
    external_id     text not null unique,
    sender_address  text null,
    enqueued_at     timestamptz not null default now(),
    status          text not null default 'PENDING'
        check (status in ('PENDING', 'SCREENING', 'DONE')),
    updated_at      timestamptz not null default now()
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Serves the retry pass's own query directly off the index -- "every row
-- still missing a sender_address" -- without a table scan as the queue
-- grows.
CREATE INDEX idx_screening_queue_missing_sender_address
    ON screening_queue (order_id) WHERE sender_address IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE screening_queue;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE discovery_cursor;
-- +goose StatementEnd
