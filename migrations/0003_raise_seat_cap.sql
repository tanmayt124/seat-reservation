-- Raise the per-show seat cap from 10,000 to 100,000 so a stadium-sized show
-- fits. The handler enforces the same number; this CHECK is the backstop.
-- The constraint name is Postgres's default for the inline CHECK in 0001.
ALTER TABLE shows
    DROP CONSTRAINT shows_total_seats_check,
    ADD CONSTRAINT shows_total_seats_check CHECK (total_seats BETWEEN 1 AND 100000);
