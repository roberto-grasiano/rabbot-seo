-- 0005_alert_members: per-incident member-URL tracking.
--
-- An incident's group fingerprint (site+change_type+severity) deliberately
-- elides the URL, so many affected pages map to one row in `alerts`. Today
-- Resolve closes the whole group on the FIRST member URL's recovery, silencing
-- still-broken siblings. This table tracks which URLs are live members of an
-- open incident so the alerts pipeline closes the incident only when the LAST
-- member recovers.
--
-- Keyed (alert_id, url) with ON DELETE CASCADE to alerts(id): when an incident
-- row is deleted (e.g. site removal cascading through alerts.site_id), its
-- member rows go with it. PRAGMA foreign_keys = ON is pinned on every
-- connection by the store's connection hook, so the cascade is enforced.
--
-- Forward-only. The composite PRIMARY KEY already indexes by the alert_id
-- prefix, so the by-alert probes (RemoveAlertMember COUNT, CountAlertMembers)
-- need no extra index.

CREATE TABLE alert_members (
    alert_id INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
    url      TEXT    NOT NULL,
    PRIMARY KEY (alert_id, url)
);
