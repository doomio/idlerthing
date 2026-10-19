-- idlerthing — a lightweight, self-hosted inventory for hosting services.
-- Copyright (C) 2026 AlteredParadox
--
-- This program is free software: you can redistribute it and/or modify it
-- under the terms of the GNU Affero General Public License as published by
-- the Free Software Foundation, either version 3 of the License, or (at your
-- option) any later version.
--
-- This program is distributed in the hope that it will be useful, but WITHOUT
-- ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
-- FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
-- for more details.
--
-- You should have received a copy of the GNU Affero General Public License
-- along with this program. If not, see <https://www.gnu.org/licenses/>.

-- idlerthing — a lightweight, self-hosted inventory for hosting services.
-- Copyright (C) 2026 AlteredParadox
-- SPDX-License-Identifier: AGPL-3.0-or-later
-- Private observation only; deliberately excluded from exports.
ALTER TABLE ssh_observations ADD COLUMN directory_entries TEXT NOT NULL DEFAULT '';
ALTER TABLE ssh_observations ADD COLUMN directory_rest_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ssh_observations ADD COLUMN directory_rest_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE ssh_observations ADD COLUMN directory_at TEXT NOT NULL DEFAULT '';
