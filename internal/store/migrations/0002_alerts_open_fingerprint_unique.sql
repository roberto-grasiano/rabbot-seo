-- Enforce the single-open-incident-per-fingerprint invariant at the database
-- level. Until now this invariant rested solely on the alerts pipeline's
-- in-process mutex (lockFor) plus GetOpenIncident/OpenIncident; a partial unique
-- index makes SQLite reject a second open row for the same fingerprint, so a
-- bug or a second process cannot silently create duplicate open incidents.
CREATE UNIQUE INDEX idx_alerts_open_fingerprint
    ON alerts(fingerprint)
    WHERE status = 'open';
