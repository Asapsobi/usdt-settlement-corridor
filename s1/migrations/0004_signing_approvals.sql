-- signing_approvals is the human-approval half of the hybrid threshold
-- (s1-key-custody-architecture.md's own "The approval queue"): one row
-- per (request, approver) decision, UNIQUE-constrained so a repeated
-- approve/reject from the same actor can never count as a second vote.
--
-- Also adds the same one-way-transition trigger every other lifecycle
-- table in this project enforces at the DB level (depositwatcher's
-- watched_addresses, this module's own s1_slot_keys): PENDING may become
-- SIGNED or REJECTED, but neither of those may change again.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE signing_approvals (
    id                  bigserial primary key,
    signing_request_id  bigint not null references signing_requests(id),
    approver            text not null,
    decision            text not null CHECK (decision IN ('APPROVE', 'REJECT')),
    decided_at          timestamptz not null default now(),
    UNIQUE (signing_request_id, approver)
);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION signing_requests_check_transition() RETURNS trigger AS $$
BEGIN
    IF (OLD.status, NEW.status) NOT IN (
        ('PENDING', 'SIGNED'),
        ('PENDING', 'REJECTED')
    ) THEN
        RAISE EXCEPTION 'signing_requests: illegal status transition % -> % for request %',
            OLD.status, NEW.status, OLD.id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER signing_requests_transition_check
    BEFORE UPDATE OF status ON signing_requests
    FOR EACH ROW
    WHEN (OLD.status IS DISTINCT FROM NEW.status)
    EXECUTE FUNCTION signing_requests_check_transition();
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER signing_requests_transition_check ON signing_requests;
-- +goose StatementEnd
-- +goose StatementBegin
DROP FUNCTION signing_requests_check_transition();
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE signing_approvals;
-- +goose StatementEnd
