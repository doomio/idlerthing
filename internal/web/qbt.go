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

// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
// SPDX-License-Identifier: AGPL-3.0-or-later
package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

// Only the SSH user's own loopback listener on port 8080 is queried. No
// authentication, cookies, redirects, arbitrary URLs or action endpoints.
const qbtCommand = `uid=$(id -u); if ! awk -v uid="$uid" '$2=="0100007F:1F90" && $4=="0A" && $8==uid {found=1} END {exit !found}' /proc/net/tcp; then exit 1; fi; curl -q --silent --show-error --include --request GET --max-time 8 --max-filesize 16777216 --noproxy '*' 'http://127.0.0.1:8080/api/v2/sync/maindata?rid=0'`

func parseQBT(raw string) (string, error) {
	response, err := http.ReadResponse(bufio.NewReader(strings.NewReader(raw)), nil)
	if err != nil {
		return "", errors.New("invalid qBittorrent response")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", errors.New("qBittorrent WebUI unavailable or authentication required")
	}
	var data struct {
		Torrents    map[string]json.RawMessage `json:"torrents"`
		ServerState struct {
			Down *int64 `json:"dl_info_speed"`
			Up   *int64 `json:"up_info_speed"`
		} `json:"server_state"`
	}
	// ReadResponse handles Content-Length/chunked framing; also cap decoded body.
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20+1))
	if err != nil || len(body) > 16<<20 {
		return "", errors.New("qBittorrent response too large or invalid")
	}
	if err = json.Unmarshal(body, &data); err != nil || data.Torrents == nil || data.ServerState.Down == nil || data.ServerState.Up == nil || *data.ServerState.Down < 0 || *data.ServerState.Up < 0 {
		return "", errors.New("invalid qBittorrent snapshot")
	}
	return fmt.Sprintf("%d torrents; down %d KiB/s; up %d KiB/s", len(data.Torrents), *data.ServerState.Down/1024, *data.ServerState.Up/1024), nil
}

func collectQBT(ctx context.Context, b *model.Seedbox, run func(context.Context, *model.Seedbox, string, time.Duration, int) (string, error)) (string, error) {
	if !b.Active || !b.SSHPollEnabled || !b.QBTEnabled || b.SSHUser == "" {
		return "", errors.New("qBittorrent observation disabled")
	}
	raw, err := run(ctx, b, qbtCommand, sshPollTimeout, 16<<20+8192)
	if err != nil {
		return "", err
	}
	return parseQBT(raw)
}

func pollQBT(ctx context.Context, b *model.Seedbox) (string, error) {
	return collectQBT(ctx, b, runSSHCommand)
}

func (s *Server) handleQBTPoll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	b, _, err := (&model.SeedboxStore{DB: s.db}).Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !b.Active || !b.SSHPollEnabled || !b.QBTEnabled || b.SSHUser == "" {
		http.Error(w, "qBittorrent observation disabled", http.StatusForbidden)
		return
	}
	result, pollErr := s.qbtPoll(r.Context(), b)
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	if pollErr != nil {
		_, err = s.db.ExecContext(r.Context(), `INSERT INTO qbt_observations(seedbox_id,attempted_at,last_error) VALUES(?,?,?) ON CONFLICT(seedbox_id) DO UPDATE SET attempted_at=excluded.attempted_at,last_error=excluded.last_error`, id, stamp, "qBittorrent observation unavailable")
	} else {
		_, err = s.db.ExecContext(r.Context(), `INSERT INTO qbt_observations(seedbox_id,result,success_at,attempted_at) VALUES(?,?,?,?) ON CONFLICT(seedbox_id) DO UPDATE SET result=excluded.result,success_at=excluded.success_at,attempted_at=excluded.attempted_at,last_error=''`, id, result, stamp, stamp)
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if pollErr != nil {
		s.setFlash(w, r, "err", "qBittorrent observation unavailable (check SSH, loopback listener ownership and WebUI auth bypass).")
	} else {
		s.setFlash(w, r, "ok", result)
	}
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
