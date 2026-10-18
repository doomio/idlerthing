// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package main

import "fmt"

// Synthetic fixture data. Every value here is invented and uses the reserved
// example.net / example.com domains (RFC 2606). This file is committed to a
// public repository: never put a real hostname, price, username, email or API
// key in it.

// fixture builds a document in idlerthing's own export format (format 1), so
// the seed runs through the real importer.
//
// Items are nested under their entity key ("server", "seedbox", ...) because
// that is the shape export produces; a flat object is silently skipped.
func fixture() map[string]any {
	return map[string]any{
		"format": 1,
		"providers": []any{
			map[string]any{"id": 1, "name": "Example Hosting"},
			map[string]any{"id": 2, "name": "PulsedMedia"},
		},
		"locations": []any{
			map[string]any{"id": 1, "name": "Amsterdam"},
			map[string]any{"id": 2, "name": "Helsinki"},
		},
		"os": []any{
			map[string]any{"id": 1, "name": "Debian 13"},
		},
		"labels": []any{
			map[string]any{"id": 1, "label": "production"},
			map[string]any{"id": 2, "label": "personal"},
		},
		"servers": []any{
			wrap("server", server(1, "web-01.example.net", 1, 1, "AMD EPYC 7443P", 16, 32768, 10, 1000)),
			wrap("server", server(2, "web-02.example.net", 1, 1, "AMD EPYC 7443P", 16, 32768, 10, 1000)),
			wrap("server", server(3, "db-01.example.net", 2, 1, "Intel Xeon E-2388G", 8, 65536, 20, 2500)),
			wrap("server", server(4, "cache-01.example.net", 2, 2, "Intel N100", 4, 16384, 5, 1000)),
		},
		"seedboxes": []any{
			wrap("seedbox", seedbox(1, "dragon.example.net", 2, "Dragon-R", 2500, 10)),
			wrap("seedbox", seedbox(2, "kvm.example.net", 2, "KVM", 1000, 5)),
		},
		"domains": []any{
			wrap("domain", map[string]any{
				"id": 1, "domain": "example.net", "extension": "net",
				"provider_id": 1, "active": true, "was_promo": false,
			}),
		},
	}
}

func wrap(key string, entity map[string]any) map[string]any {
	return map[string]any{key: entity}
}

func server(id int, hostname string, provider, location int, cpu string, cores, ramMB, bwTB, linkMbps int) map[string]any {
	return map[string]any{
		"id": id, "hostname": hostname, "server_type": 1, "os_id": 1,
		"provider_id": provider, "location_id": location, "cpu": cores,
		"cpu_model": cpu, "ram_as_mb": ramMB, "bandwidth_as_mb": bwTB * 1024 * 1024,
		"link_speed": linkMbps, "ssh_port": 22, "active": true,
		"show_public": false, "was_promo": false, "network_type": "IPv4+IPv6",
		// Pricing rides with the service in this format.
		"pricing": map[string]any{
			"currency": "EUR", "price": 36.00, "term": 1, "next_due_date": "2026-11-01",
		},
		"disks": []any{
			map[string]any{"size_as_mb": 1024 * 1024, "media": "NVMe"},
		},
	}
}

func seedbox(id int, hostname string, provider int, kind string, portSpeed, bwTB int) map[string]any {
	return map[string]any{
		"id": id, "hostname": hostname, "provider_id": provider, "location_id": 1,
		"seed_box_type": kind, "port_speed": portSpeed,
		"disk_as_mb":      2 * 1024 * 1024,
		"bandwidth_as_mb": bwTB * 1024 * 1024, "active": true,
		"show_public": false, "was_promo": false,
		"pricing": map[string]any{
			"currency": "EUR", "price": 44.97, "term": 2, "next_due_date": "2026-11-01",
		},
	}
}

// feedJSON is a synthetic PulsedMedia customer-API response: three active
// services and one cancelled one, so the fleet view's toggle has something to
// show.
func feedJSON() []byte {
	svc := func(id int, product, host, cycle, due string, amount float64, status string, torrents int) string {
		return fmt.Sprintf(`{"id":%d,"product":%q,"group":"Example","domain":%q,
"status":%q,"registered":"2025-03-01","next_due":%q,"billing_cycle":%q,
"recurring_amount":%v,"seedbox":{"username":"demo",
"login_url":"https://%s/user-demo/","permanent_address":"https://%s/",
"usage":{"tracked_since":"2026-10-01","sent_bytes":1099511627776,"received_bytes":549755813888,
"cpu_hours":%d,"cpu_since":"2026-10-01","disk_used_bytes":3298534883328,
"disk_quota_bytes":8796093022208,"torrents":%d}}}`,
			id, product, host, status, due, cycle, amount, host, host, id*3, torrents)
	}
	body := `{"client_id":1000,"generated_at":"2026-10-18T09:00:00Z","currency":"EUR","services":[` +
		svc(101, "Dragon-R", "dragon.example.net", "Quarterly", "2026-11-01", 44.97, "Active", 312) + "," +
		svc(102, "KVM 2T", "kvm.example.net", "Monthly", "2026-10-25", 14.99, "Active", 47) + "," +
		svc(103, "Storage 4T", "store.example.net", "Annually", "2027-01-15", 99.00, "Active", 5) + "," +
		svc(104, "Retired Box", "retired.example.net", "Monthly", "", 9.99, "Cancelled", 0) +
		`]}`
	return []byte(body)
}
