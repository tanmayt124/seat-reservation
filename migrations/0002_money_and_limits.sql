-- Money is integer paise, never floating point. Defaults keep the change safe
-- on databases that already hold shows and reservations.
ALTER TABLE shows
    ADD COLUMN price_paise bigint NOT NULL DEFAULT 0 CHECK (price_paise >= 0),
    -- The per-user seat limit is a property of the show ("a limit=4 show").
    ADD COLUMN per_user_limit integer NOT NULL DEFAULT 4 CHECK (per_user_limit BETWEEN 1 AND 100);

ALTER TABLE reservations
    ADD COLUMN amount_paise bigint NOT NULL DEFAULT 0 CHECK (amount_paise >= 0);
