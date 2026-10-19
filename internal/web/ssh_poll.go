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
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"idlerthing/internal/model"
)

const sshPollTimeout = 12 * time.Second

type limitedWriter struct {
	writer    io.Writer
	remaining int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, errors.New("SSH response too large")
	}
	n, err := w.writer.Write(p)
	w.remaining -= n
	return n, err
}

func dialPublicSSH(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || !validSSHTarget(host) {
		return nil, errors.New("invalid SSH address")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Is4In6() || ip.IsUnspecified() {
			return nil, errors.New("SSH target resolves to a non-public address")
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("SSH target has no addresses")
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
	return conn, err
}

func parseSSHHostSnapshot(raw string) (sshHostSnapshot, error) {
	if len(raw) > 256 {
		return sshHostSnapshot{}, errors.New("invalid host snapshot")
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	if len(lines) < 3 {
		return sshHostSnapshot{}, errors.New("invalid host snapshot")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 3 {
		return sshHostSnapshot{}, errors.New("invalid host snapshot")
	}
	for _, field := range fields[:3] {
		v, err := strconv.ParseFloat(field, 64)
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
			return sshHostSnapshot{}, errors.New("invalid host load")
		}
	}
	var total, available int64
	if _, err := fmt.Sscanf(lines[1], "MemTotal: %d kB", &total); err != nil {
		return sshHostSnapshot{}, errors.New("invalid host memory")
	}
	if _, err := fmt.Sscanf(lines[2], "MemAvailable: %d kB", &available); err != nil || total <= 0 || available < 0 || available > total || total > 1<<40 {
		return sshHostSnapshot{}, errors.New("invalid host memory")
	}
	return sshHostSnapshot{Load: strings.Join(fields[:3], " / "), MemoryUsed: (total - available) * 1024, MemoryTotal: total * 1024}, nil
}

type sshHostSnapshot struct {
	Load                    string
	MemoryUsed, MemoryTotal int64
}

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

// pollSeedboxLoad uses the existing Go SSH dependency with strict host-key
// verification. The app user must have a private key and trusted known_hosts.
// Dialing the vetted IP avoids DNS rebinding between validation and connect.
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
	verify, err := knownhosts.New(knownHosts)
	if err != nil {
		return "", errors.New("SSH known_hosts is not configured")
	}
	keys, err := filepath.Glob(filepath.Join(home, ".ssh", "id_*"))
	if err != nil {
		return "", err
	}
	var signers []ssh.Signer
	for _, file := range keys {
		if strings.HasSuffix(file, ".pub") {
			continue
		}
		info, err := os.Stat(file)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			continue
		}
		key, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err == nil {
			signers = append(signers, signer)
		}
	}
	if len(signers) == 0 {
		return "", errors.New("SSH identity is not configured")
	}
	ctx, cancel := context.WithTimeout(ctx, sshPollTimeout)
	defer cancel()
	cfg := &ssh.ClientConfig{User: b.SSHUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)}, HostKeyCallback: verify, Timeout: 5 * time.Second}
	conn, err := dialPublicSSH(ctx, "tcp", net.JoinHostPort(b.Hostname, "22"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(sshPollTimeout)); err != nil {
		return "", err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	// Preserve the hostname for host-key verification even though the dial used a vetted IP.
	clientConn, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(b.Hostname, "22"), cfg)
	if err != nil {
		return "", err
	}
	client := ssh.NewClient(clientConn, chans, reqs)
	defer client.Close()
	session, err := client.NewSession()
	if err != nil {
		return "", err
	}
	defer session.Close()
	var out strings.Builder
	session.Stdout = &limitedWriter{writer: &out, remaining: 256}
	session.Stderr = io.Discard
	if err := session.Run("cat /proc/loadavg; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo"); err != nil {
		return "", err
	}
	snap, err := parseSSHHostSnapshot(out.String())
	if err != nil {
		return "", err
	}
	result := ""
	if b.SSHHostMetrics {
		result = "Host load (1 / 5 / 15 min): " + snap.Load
	}
	if b.SSHMemoryMetrics {
		if result != "" {
			result += "; "
		}
		result += "Host memory: " + fmtMB(snap.MemoryUsed/(1024*1024)) + " / " + fmtMB(snap.MemoryTotal/(1024*1024))
	}
	return result, nil
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
	if !b.SSHHostMetrics && !b.SSHMemoryMetrics {
		http.Error(w, "No SSH metrics selected", http.StatusForbidden)
		return
	}
	result, err := s.sshPoll(r.Context(), b)
	if err != nil {
		s.setFlash(w, r, "err", "SSH poll failed; verify the app user's SSH access and known_hosts.")
	} else {
		s.setFlash(w, r, "ok", result)
	}
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
