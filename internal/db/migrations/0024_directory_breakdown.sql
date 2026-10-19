-- idlerthing — a lightweight, self-hosted inventory for hosting services.
-- Copyright (C) 2026 AlteredParadox
-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Private observation only; deliberately excluded from exports.
ALTER TABLE ssh_observations ADD COLUMN directory_entries TEXT NOT NULL DEFAULT '';
ALTER TABLE ssh_observations ADD COLUMN directory_rest_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ssh_observations ADD COLUMN directory_rest_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ssh_observations ADD COLUMN directory_at TEXT NOT NULL DEFAULT '';
