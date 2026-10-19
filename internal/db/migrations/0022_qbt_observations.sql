-- idlerthing — a lightweight, self-hosted inventory for hosting services.
-- Copyright (C) 2026 AlteredParadox
-- SPDX-License-Identifier: AGPL-3.0-or-later
ALTER TABLE seedboxes ADD COLUMN qbt_enabled INTEGER NOT NULL DEFAULT 0;
CREATE TABLE qbt_observations (
    seedbox_id INTEGER PRIMARY KEY REFERENCES seedboxes(id) ON DELETE CASCADE,
    result TEXT NOT NULL DEFAULT '',
    success_at TEXT NOT NULL DEFAULT '',
    attempted_at TEXT NOT NULL,
    last_error TEXT NOT NULL DEFAULT ''
);
