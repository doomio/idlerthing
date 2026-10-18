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

-- 0014_pulsedmedia.sql — cache the PulsedMedia customer-API feed.
--
-- The feed is one account-wide request authenticated by a single API key, so
-- the key lives on the provider row rather than on each seedbox. pm_services
-- holds the response verbatim: it is replaced wholesale on every sync, so its
-- rows are derived data and never referenced by the inventory.
--
-- pm_service_id on seedboxes links an inventory row to a feed service. It is
-- deliberately NOT a foreign key: cache rows are deleted and reinserted each
-- sync, which a FK would either block or cascade into the inventory. A stale
-- id simply means "no feed data for this seedbox".
--
-- Live usage figures (bytes, cpu_hours, torrents) are stored here only. They
-- are volatile and rendered from the cache; they must never be written into
-- the inventory columns.

ALTER TABLE providers ADD COLUMN pm_api_key TEXT NOT NULL DEFAULT '';
ALTER TABLE seedboxes ADD COLUMN pm_service_id INTEGER;

CREATE TABLE pm_services (
  provider_id INTEGER NOT NULL,
  service_id  INTEGER NOT NULL,
  product TEXT NOT NULL DEFAULT '',
  grp TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  registered TEXT NOT NULL DEFAULT '',
  next_due TEXT NOT NULL DEFAULT '',
  billing_cycle TEXT NOT NULL DEFAULT '',
  recurring_amount REAL NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT '',
  username TEXT NOT NULL DEFAULT '',
  login_url TEXT NOT NULL DEFAULT '',
  permanent_address TEXT NOT NULL DEFAULT '',
  tracked_since TEXT NOT NULL DEFAULT '',
  sent_bytes INTEGER NOT NULL DEFAULT 0,
  received_bytes INTEGER NOT NULL DEFAULT 0,
  cpu_hours INTEGER NOT NULL DEFAULT 0,
  cpu_since TEXT NOT NULL DEFAULT '',
  disk_used_bytes INTEGER NOT NULL DEFAULT 0,
  disk_quota_bytes INTEGER NOT NULL DEFAULT 0,
  torrents INTEGER NOT NULL DEFAULT 0,
  generated_at TEXT NOT NULL DEFAULT '',
  synced_at TEXT NOT NULL,
  PRIMARY KEY (provider_id, service_id)
);

CREATE INDEX idx_pm_services_provider ON pm_services(provider_id, status);
