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

package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

const sshPollTimeout = 12 * time.Second

func validSSHTarget(host string) bool {
	if host == "" || strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") || strings.HasSuffix(strings.ToLower(host), ".local") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	if len(host) > 253 || strings.ContainsAny(host, ":/@[]% ") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, c := range label {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-", c) {
				return false
			}
		}
	}
	return strings.Contains(host, ".")
}

// pollSeedboxLoad uses the system SSH client with strict host-key verification.
// The host key must be enrolled in the app user's known_hosts before polling.
func pollSeedboxLoad(ctx context.Context, b *model.Seedbox) (string, error) {
	if b.SSHUser == "" || len(b.SSHUser) > 64 || strings.HasPrefix(b.SSHUser, "-") || strings.IndexFunc(b.SSHUser, func(c rune) bool {
		return !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.-", c)
	}) >= 0 || !validSSHTarget(b.Hostname) {
		return "", errors.New("invalid SSH target")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(knownHosts); err != nil {
		return "", errors.New("SSH known_hosts is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, sshPollTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts,
		"-o", "GlobalKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=5", "-o", "ConnectionAttempts=1",
		"-o", "ProxyCommand=none", "-o", "ProxyJump=none", "-o", "PermitLocalCommand=no", "-o", "ClearAllForwardings=yes", "-o", "LogLevel=ERROR", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "IdentityAgent=none", "-p", "22", "--", b.SSHUser+"@"+b.Hostname, "cat /proc/loadavg")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("SSH poll failed: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 3 {
		return "", errors.New("invalid load average response")
	}
	for _, field := range fields[:3] {
		if _, err := strconv.ParseFloat(field, 64); err != nil {
			return "", errors.New("invalid load average response")
		}
	}
	return strings.Join(fields[:3], " / "), nil
}

func (s *Server) handleSeedboxPoll(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	b, _, err := (&model.SeedboxStore{DB: s.db}).Get(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if !b.SSHPollEnabled || !b.Active || b.SSHUser == "" {
		http.Error(w, "SSH polling disabled", http.StatusForbidden)
		return
	}
	result, err := s.sshPoll(r.Context(), b)
	if err != nil {
		s.setFlash(w, r, "err", "SSH poll failed; verify the app user's SSH access and known_hosts.")
	} else {
		s.setFlash(w, r, "ok", "Host load (1 / 5 / 15 min): "+result)
	}
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
