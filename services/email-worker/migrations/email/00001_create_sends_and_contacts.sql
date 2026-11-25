-- +goose Up

-- One row per event whose email the worker handles (D69): a claim while the
-- email is being sent, then the outcome. No personal data.
CREATE TABLE sends (
    event_id uuid PRIMARY KEY,
    event_type text NOT NULL,
    order_id uuid NOT NULL,
    event_time timestamptz NOT NULL,
    state text NOT NULL CHECK (state IN ('sending', 'sent', 'failed')),
    claim_id uuid NOT NULL,
    claim_expires_at timestamptz NOT NULL,
    -- Claims taken: above 1, the email may have been sent again.
    attempts integer NOT NULL DEFAULT 1,
    -- The mail server's reply to a refused email.
    reply_code smallint CHECK ((state = 'failed') = (reply_code IS NOT NULL)),
    updated_at timestamptz NOT NULL DEFAULT now()
);

-- The sweep deletes sends 30 days after the event's time.
CREATE INDEX sends_event_time ON sends (event_time);

-- Each order's contact, the only personal data the worker keeps (D64).
CREATE TABLE contacts (
    order_id uuid PRIMARY KEY,
    email text NOT NULL,
    first_name text NOT NULL,
    delete_after timestamptz NOT NULL,
    -- Set when the mail server refused an email to it for good.
    undeliverable_at timestamptz
);

CREATE INDEX contacts_delete_after ON contacts (delete_after);
