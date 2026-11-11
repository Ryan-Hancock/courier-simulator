-- Invoices are written by the billing service (later). Integer pence only.
-- Note: no outbox or idempotency_keys tables yet. They arrive with brief 4
-- ("Our biggest shop's orders arrive twice").

CREATE TABLE invoices (
    id           bigserial PRIMARY KEY,
    account_id   bigint NOT NULL REFERENCES accounts(id),
    period       daterange NOT NULL,
    total_pence  bigint NOT NULL,
    issued_at    timestamptz,
    UNIQUE (account_id, period)
);

CREATE TABLE invoice_lines (
    id            bigserial PRIMARY KEY,
    invoice_id    bigint NOT NULL REFERENCES invoices(id),
    job_id        bigint NOT NULL REFERENCES jobs(id),
    amount_pence  integer NOT NULL
);
