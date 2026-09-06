-- C3.7's re-screen mechanism: captures a sender address whose fresh
-- verdict now disagrees with the one that let an order through
-- screening, for a human to decide what happens to an order already
-- mid-flight (screened or dispatching, not yet settled). This chunk
-- builds the capture-and-surface mechanism only, not the policy for
-- what automatically happens next -- same posture as C2.8's
-- orphaned-deposit handling. Nothing in this system automatically
-- transitions an order based on a row here; there is deliberately no
-- screened->held or dispatching->held pair in C1.5's transition table
-- for this reason.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE rescreen_flags (
    id                       bigserial primary key,
    order_id                 bigint not null,
    external_id              text not null,
    order_state_at_detection text not null,
    previous_verdict_id      bigint not null references screening_results(id),
    new_verdict_id           bigint not null references screening_results(id),
    detected_at              timestamptz not null default now(),
    resolution               text null,
    resolved_at              timestamptz null
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Serves the operator-facing "unresolved flags" listing (C3.8's own
-- GET /rescreen-flags?resolved=false) directly off the index.
CREATE INDEX idx_rescreen_flags_unresolved
    ON rescreen_flags (detected_at) WHERE resolution IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE rescreen_flags;
-- +goose StatementEnd
