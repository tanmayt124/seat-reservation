-- Core schema. The database enforces the rules that matter most, so a bug in
-- application code is rejected here instead of silently corrupting inventory.

CREATE TABLE shows (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text        NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    starts_at   timestamptz,
    total_seats integer     NOT NULL CHECK (total_seats BETWEEN 1 AND 10000),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE reservations (
    id           uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    show_id      uuid        NOT NULL REFERENCES shows (id) ON DELETE CASCADE,
    user_id      text        NOT NULL,
    seat_labels  text[]      NOT NULL CHECK (cardinality(seat_labels) >= 1),
    status       text        NOT NULL DEFAULT 'confirmed'
                             CHECK (status IN ('confirmed', 'cancelled')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    cancelled_at timestamptz,
    CONSTRAINT reservations_cancelled_at_matches_status
        CHECK ((status = 'cancelled') = (cancelled_at IS NOT NULL))
);

CREATE INDEX reservations_user_show_idx ON reservations (user_id, show_id);

-- One row per seat. The primary key makes a label unique within a show.
-- status 'held' is not used by v1 (reservations confirm immediately) but is
-- part of the invariant available + held + confirmed = total_seats, so the
-- schema allows it and the counts report it.
CREATE TABLE seats (
    show_id        uuid        NOT NULL REFERENCES shows (id) ON DELETE CASCADE,
    label          text        NOT NULL CHECK (length(label) BETWEEN 1 AND 16),
    status         text        NOT NULL DEFAULT 'available'
                               CHECK (status IN ('available', 'held', 'confirmed')),
    reservation_id uuid        REFERENCES reservations (id),
    user_id        text,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (show_id, label),
    -- A seat has a holder exactly when it is not available. This makes a
    -- "sold seat with no owner" or "free seat with an owner" impossible.
    CONSTRAINT seats_holder_matches_status CHECK (
        (status = 'available' AND reservation_id IS NULL AND user_id IS NULL)
        OR
        (status <> 'available' AND reservation_id IS NOT NULL AND user_id IS NOT NULL)
    )
);

CREATE INDEX seats_show_status_idx  ON seats (show_id, status);
CREATE INDEX seats_show_user_idx    ON seats (show_id, user_id)  WHERE user_id IS NOT NULL;
CREATE INDEX seats_reservation_idx  ON seats (reservation_id)    WHERE reservation_id IS NOT NULL;

-- Idempotency is scoped per user: two users sending the same key never collide.
-- response_status is NULL while the first request is still in flight.
CREATE TABLE idempotency_keys (
    user_id         text        NOT NULL,
    key             text        NOT NULL CHECK (length(key) BETWEEN 1 AND 128),
    request_hash    bytea       NOT NULL,
    response_status integer,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, key)
);
