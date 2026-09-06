-- C3.6's hold queue: one row per Hold classification C3.4 ever reported
-- to C1, resolved exactly once (RELEASED or REJECTED) by a human via
-- internal/holds.Release/Reject -- the only two paths that ever move an
-- order out of `held` (invariant 1, §0 of
-- docs/03-build/c3-screening-build-prompts.md). C3.4's own automatic
-- pipeline never touches a row here.
--
-- The partial unique index is Open's own idempotency mechanism: calling
-- Open twice for the same order_id while its hold is still OPEN must
-- not create a second row (the build spec's own acceptance criterion,
-- guarding against a retried C3.4 pipeline run double-opening a hold).
-- Once a hold resolves (RELEASED/REJECTED), the index no longer applies
-- to it, so a LATER genuine re-hold of the same order (a future
-- re-screen flagging it again, C3.7) can open a fresh row without
-- fighting a historical one.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE holds (
    id                    bigserial primary key,
    order_id              bigint not null,
    external_id           text not null,
    reason_code           text not null,
    screening_result_id   bigint null references screening_results(id),
    opened_at             timestamptz not null default now(),
    status                text not null default 'OPEN'
        check (status in ('OPEN', 'RELEASED', 'REJECTED')),
    resolved_by           text null,
    resolved_at           timestamptz null,
    resolution_note       text null
);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_holds_one_open_per_order
    ON holds (order_id) WHERE status = 'OPEN';
-- +goose StatementEnd
-- +goose StatementBegin
-- Serves ListOpen directly off the index, oldest first -- the natural
-- order for a human review queue.
CREATE INDEX idx_holds_open_by_opened_at
    ON holds (opened_at) WHERE status = 'OPEN';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE holds;
-- +goose StatementEnd
