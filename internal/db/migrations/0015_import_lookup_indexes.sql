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

-- Match case-insensitive import lookups without changing duplicate rules.
CREATE INDEX idx_servers_hostname_nocase ON servers(hostname COLLATE NOCASE);
CREATE INDEX idx_providers_name_nocase ON providers(name COLLATE NOCASE);
CREATE INDEX idx_locations_name_nocase ON locations(name COLLATE NOCASE);
CREATE INDEX idx_os_name_nocase ON os(name COLLATE NOCASE);
CREATE INDEX idx_labels_label_nocase ON labels(label COLLATE NOCASE);
