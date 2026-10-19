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
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

func sshAccountCommand(b *model.Seedbox) string {
	if !b.SSHQuotaMetrics && !b.SSHTrafficMetrics {
		return ""
	}
	var parts []string
	if b.SSHQuotaMetrics {
		parts = append(parts, `if command -v quota >/dev/null 2>&1; then q=$(quota -v -w 2>/dev/null | tail -n 1 | head -c 320); if [ -n "$q" ]; then printf 'quota=%s\n' "$q"; fi; fi; if [ -f "$HOME/.quota" ]; then printf 'quota_file='; head -c 320 "$HOME/.quota"; printf '\n'; fi`)
	}
	if b.SSHTrafficMetrics {
		parts = append(parts, `for k in traffic_limit bonus_traffic; do case "$k" in traffic_limit) f="$HOME/.trafficLimit";; *) f="$HOME/.bonusTraffic";; esac; if [ -f "$f" ]; then printf '%s=' "$k"; head -c 32 "$f"; printf '\n'; fi; done; if [ -e "$HOME/.throttle" ]; then echo throttled=1; else echo throttled=0; fi`)
	}
	return strings.Join(parts, "; ")
}

func parseAccountQuotaUsed(token string) (int64, error) {
	multiplier := float64(1024)
	if len(token) > 0 {
		switch token[len(token)-1] {
		case 'K':
			multiplier = 1024
			token = token[:len(token)-1]
		case 'M':
			multiplier = 1024 * 1024
			token = token[:len(token)-1]
		case 'G':
			multiplier = 1024 * 1024 * 1024
			token = token[:len(token)-1]
		case 'T':
			multiplier = 1024 * 1024 * 1024 * 1024
			token = token[:len(token)-1]
		}
	}
	v, err := strconv.ParseFloat(token, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v*multiplier > 1<<60 {
		return 0, errors.New("invalid quota")
	}
	return int64(v * multiplier), nil
}

func accountQuotaSummary(raw string) (string, error) {
	fields := strings.Fields(raw)
	if len(fields) < 7 {
		return "", errors.New("invalid account quota")
	}
	usedBytes, err := parseAccountQuotaUsed(fields[1])
	if err != nil {
		return "", err
	}
	softBytes, err := parseAccountQuotaUsed(fields[2])
	if err != nil {
		return "", err
	}
	if softBytes == 0 {
		return "Account disk used: " + strconv.FormatInt(usedBytes/(1024*1024), 10) + " MiB (quota not set)", nil
	}
	return "Account disk used / soft quota: " + strconv.FormatInt(usedBytes/(1024*1024), 10) + " / " + strconv.FormatInt(softBytes/(1024*1024), 10) + " MiB", nil
}

func parseSSHAccountSnapshot(raw string, b *model.Seedbox) (string, error) {
	if len(raw) == 0 || len(raw) > 1024 {
		return "", errors.New("invalid account snapshot")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || key == "" || strings.ContainsAny(val, "\r\n=") || len(val) > 160 {
			return "", errors.New("invalid account snapshot")
		}
		values[key] = strings.TrimSpace(val)
	}
	var results []string
	if b.SSHQuotaMetrics {
		quota := "Account disk quota unavailable"
		for _, key := range []string{"quota", "quota_file"} {
			if values[key] == "" {
				continue
			}
			if summary, err := accountQuotaSummary(values[key]); err == nil {
				quota = summary
				break
			}
		}
		results = append(results, quota)
	}
	if b.SSHTrafficMetrics {
		if values["traffic_limit"] == "" {
			results = append(results, "Traffic limit unavailable")
		} else {
			limit, err := strconv.ParseFloat(values["traffic_limit"], 64)
			if err != nil || math.IsNaN(limit) || math.IsInf(limit, 0) || limit < 0 || limit > 1e9 {
				results = append(results, "Traffic limit unavailable")
			} else {
				bonus := 0.0
				if values["bonus_traffic"] != "" {
					bonus, err = strconv.ParseFloat(values["bonus_traffic"], 64)
					if err != nil || math.IsNaN(bonus) || math.IsInf(bonus, 0) || bonus < 0 || bonus > 1e9 {
						bonus = 0
					}
				}
				throttle := values["throttled"]
				if throttle != "0" && throttle != "1" {
					throttle = "unknown"
				}
				results = append(results, "Traffic limit: "+strconv.FormatFloat(limit+min(bonus, limit), 'f', -1, 64)+" GiB; throttled: "+throttle)
			}
		}
	}
	return strings.Join(results, "; "), nil
}

func sshDirectoryCommand(b *model.Seedbox) string {
	if !b.SSHDirectoryMetrics {
		return ""
	}
	return `LC_ALL=C; export LC_ALL; cd "$HOME" || exit 1; set --; for p in ./* ./.[!.]* ./..?*; do if [ -e "$p" ] || [ -L "$p" ]; then set -- "$@" "$p"; fi; done; [ "$#" -eq 0 ] && { printf '\0'; exit 0; }; if command -v ionice >/dev/null 2>&1; then ionice -c3 nice -n19 du -0 -xs -B1 -- "$@"; else nice -n19 du -0 -xs -B1 -- "$@"; fi`
}

func parseSSHDirectoryUsage(raw string) (int, int64, error) {
	if len(raw) == 0 || len(raw) > 8192 || raw[len(raw)-1] != 0 {
		return 0, 0, errors.New("invalid directory snapshot")
	}
	if raw == "\x00" {
		return 0, 0, nil
	}
	var count int
	var total int64
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00") {
		value, name, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(name, "./") || len(name) <= 2 || strings.ContainsAny(name, "\x00") {
			return 0, 0, errors.New("invalid directory entry")
		}
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil || size < 0 || size > 1<<60 || total > 1<<60-size {
			return 0, 0, errors.New("invalid directory size")
		}
		count++
		total += size
	}
	return count, total, nil
}

type directoryEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}

// ponytail: O(entries*500); replace with a heap only if large home listings become slow.
func parseSSHDirectoryBreakdown(raw string) ([]directoryEntry, int, int64, error) {
	if len(raw) == 0 || len(raw) > 1<<20 || raw[len(raw)-1] != 0 {
		return nil, 0, 0, errors.New("invalid directory snapshot")
	}
	if raw == "\x00" {
		return nil, 0, 0, nil
	}
	var top []directoryEntry
	var restCount int
	var restSize int64
	for _, line := range strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00") {
		value, name, ok := strings.Cut(line, "\t")
		if !ok || !strings.HasPrefix(name, "./") || len(name) <= 2 || strings.ContainsAny(name[2:], "/\x00") || !utf8.ValidString(name) {
			return nil, 0, 0, errors.New("invalid directory entry")
		}
		size, err := strconv.ParseInt(value, 10, 64)
		if err != nil || size < 0 || size > 1<<60 || restSize > 1<<60-size {
			return nil, 0, 0, errors.New("invalid directory size")
		}
		entry := directoryEntry{Name: name[2:], Size: size}
		idx := sort.Search(len(top), func(i int) bool { return top[i].Size < size || top[i].Size == size && top[i].Name > entry.Name })
		if idx >= 500 {
			restCount++
			restSize += size
			continue
		}
		top = append(top, directoryEntry{})
		copy(top[idx+1:], top[idx:])
		top[idx] = entry
		if len(top) > 500 {
			restCount++
			restSize += top[500].Size
			top = top[:500]
		}
	}
	return top, restCount, restSize, nil
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

func sshMetricsCommand(b *model.Seedbox) string {
	var commands []string
	if b.SSHHostMetrics {
		commands = append(commands, "cat /proc/loadavg")
	}
	if b.SSHMemoryMetrics {
		commands = append(commands, "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo")
	}
	return strings.Join(commands, "; ")
}

func parseSelectedSSHMetrics(raw string, b *model.Seedbox) (sshHostSnapshot, error) {
	if len(raw) > 256 {
		return sshHostSnapshot{}, errors.New("invalid host snapshot")
	}
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	idx := 0
	var snap sshHostSnapshot
	if b.SSHHostMetrics {
		if idx >= len(lines) {
			return snap, errors.New("missing host load")
		}
		fields := strings.Fields(lines[idx])
		idx++
		if len(fields) != 5 {
			return snap, errors.New("invalid host load")
		}
		for _, field := range fields[:3] {
			v, err := strconv.ParseFloat(field, 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1e6 {
				return snap, errors.New("invalid host load")
			}
		}
		snap.Load = strings.Join(fields[:3], " / ")
	}
	if b.SSHMemoryMetrics {
		if len(lines)-idx < 2 {
			return snap, errors.New("invalid host memory")
		}
		parseMemory := func(line, label string) (int64, error) {
			fields := strings.Fields(line)
			if len(fields) != 3 || fields[0] != label || fields[2] != "kB" {
				return 0, errors.New("invalid host memory")
			}
			return strconv.ParseInt(fields[1], 10, 64)
		}
		total, err := parseMemory(lines[idx], "MemTotal:")
		if err != nil {
			return snap, err
		}
		available, err := parseMemory(lines[idx+1], "MemAvailable:")
		if err != nil || total <= 0 || available < 0 || available > total || total > 1<<40 {
			return snap, errors.New("invalid host memory")
		}
		snap.MemoryTotal, snap.MemoryUsed = total*1024, (total-available)*1024
		idx += 2
	}
	if idx != len(lines) {
		return snap, errors.New("unexpected SSH output")
	}
	return snap, nil
}

func parseSSHHostSnapshot(raw string) (sshHostSnapshot, error) {
	return parseSelectedSSHMetrics(raw, &model.Seedbox{SSHHostMetrics: true, SSHMemoryMetrics: true})
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
func runSSHCommand(ctx context.Context, b *model.Seedbox, command string, timeout time.Duration, maxOutput int) (string, error) {
	if command == "" {
		return "", errors.New("no SSH command selected")
	}
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
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cfg := &ssh.ClientConfig{User: b.SSHUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signers...)}, HostKeyCallback: verify, Timeout: 5 * time.Second}
	conn, err := dialPublicSSH(ctx, "tcp", net.JoinHostPort(b.Hostname, "22"))
	if err != nil {
		return "", err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
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
	session.Stdout = &limitedWriter{writer: &out, remaining: maxOutput}
	session.Stderr = io.Discard
	if err := session.Run(command); err != nil {
		return "", err
	}
	return out.String(), nil
}

type sshPollResult struct {
	Summary     string
	Directory   []directoryEntry
	RestCount   int
	RestSize    int64
	DirectoryOK bool
}

func pollSeedboxLoad(ctx context.Context, b *model.Seedbox) (sshPollResult, error) {
	return collectSSHMetricsResult(ctx, b, runSSHCommand)
}

func parseDockerSnapshot(raw string) (string, error) {
	if len(raw) > 16384 {
		return "", errors.New("Docker snapshot too large")
	}
	var containers []struct{ State string }
	dec := json.NewDecoder(strings.NewReader(raw))
	if err := dec.Decode(&containers); err != nil || containers == nil || dec.Decode(new(any)) != io.EOF {
		return "", errors.New("invalid Docker snapshot")
	}
	running := 0
	for _, c := range containers {
		switch c.State {
		case "running":
			running++
		case "created", "restarting", "removing", "paused", "exited", "dead":
		default:
			return "", errors.New("invalid Docker container state")
		}
	}
	return "Docker containers: " + strconv.Itoa(len(containers)) + " (" + strconv.Itoa(running) + " running)", nil
}

func collectSSHMetrics(ctx context.Context, b *model.Seedbox, run func(context.Context, *model.Seedbox, string, time.Duration, int) (string, error)) (string, error) {
	result, err := collectSSHMetricsResult(ctx, b, run)
	return result.Summary, err
}

func collectSSHMetricsResult(ctx context.Context, b *model.Seedbox, run func(context.Context, *model.Seedbox, string, time.Duration, int) (string, error)) (sshPollResult, error) {
	var poll sshPollResult
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var results []string
	var failures int
	if b.SSHHostMetrics || b.SSHMemoryMetrics {
		out, err := run(ctx, b, sshMetricsCommand(b), sshPollTimeout, 256)
		if err == nil {
			var snap sshHostSnapshot
			snap, err = parseSelectedSSHMetrics(out, b)
			if err == nil {
				if b.SSHHostMetrics {
					results = append(results, "Host load (1 / 5 / 15 min): "+snap.Load)
				}
				if b.SSHMemoryMetrics {
					results = append(results, "Host memory: "+fmtMB(snap.MemoryUsed/(1024*1024))+" / "+fmtMB(snap.MemoryTotal/(1024*1024)))
				}
			}
		}
		if err != nil {
			results = append(results, "Host metrics unavailable")
			failures++
		}
	}
	if b.SSHQuotaMetrics || b.SSHTrafficMetrics {
		out, err := run(ctx, b, sshAccountCommand(b), sshPollTimeout, 1024)
		if err == nil {
			var summary string
			summary, err = parseSSHAccountSnapshot(out, b)
			if err == nil {
				results = append(results, summary)
			}
		}
		if err != nil {
			results = append(results, "Account metrics unavailable")
			failures++
		}
	}
	if b.SSHDirectoryMetrics {
		out, err := run(ctx, b, sshDirectoryCommand(b), 2*time.Minute, 1<<20)
		if err == nil {
			var entries []directoryEntry
			var restCount int
			var restSize int64
			entries, restCount, restSize, err = parseSSHDirectoryBreakdown(out)
			if err == nil {
				var total int64 = restSize
				for _, entry := range entries {
					if total > 1<<60-entry.Size {
						err = errors.New("invalid directory total")
						break
					}
					total += entry.Size
				}
				if err == nil {
					poll.Directory, poll.RestCount, poll.RestSize, poll.DirectoryOK = entries, restCount, restSize, true
					results = append(results, "Top-level home entries: "+strconv.Itoa(len(entries)+restCount)+" ("+fmtMB(total/(1024*1024))+" total)")
				}
			}
		}
		if err != nil {
			results = append(results, "Directory usage unavailable")
			failures++
		}
	}
	if b.SSHDockerMetrics {
		// Only the authenticated account's rootless socket; fixed GET, no forwarding.
		out, err := run(ctx, b, `curl -q --silent --show-error --fail --max-time 8 --noproxy '*' --unix-socket "/run/user/$(id -u)/docker.sock" 'http://localhost/v1.41/containers/json?all=1'`, sshPollTimeout, 16384)
		if err == nil {
			var summary string
			summary, err = parseDockerSnapshot(out)
			if err == nil {
				results = append(results, summary)
			}
		}
		if err != nil {
			results = append(results, "Docker observation unavailable")
			failures++
		}
	}
	if len(results) == 0 {
		return poll, errors.New("no SSH metrics selected")
	}
	if failures == len(results) {
		return poll, errors.New("SSH observations unavailable")
	}
	poll.Summary = strings.Join(results, "; ")
	return poll, nil
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
	if !b.SSHHostMetrics && !b.SSHMemoryMetrics && !b.SSHDirectoryMetrics && !b.SSHQuotaMetrics && !b.SSHTrafficMetrics && !b.SSHDockerMetrics {
		http.Error(w, "No SSH metrics selected", http.StatusForbidden)
		return
	}
	result, err := s.sshPoll(r.Context(), b)
	stamp := time.Now().UTC().Format("2006-01-02 15:04:05 UTC")
	if err != nil {
		if _, saveErr := s.db.ExecContext(r.Context(), `INSERT INTO ssh_observations (seedbox_id, attempted_at, last_error) VALUES (?, ?, ?) ON CONFLICT(seedbox_id) DO UPDATE SET attempted_at = excluded.attempted_at, last_error = excluded.last_error`, id, stamp, "SSH observation unavailable"); saveErr != nil {
			http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			return
		}
		s.setFlash(w, r, "err", "SSH poll failed at "+stamp+"; check SSH access, known_hosts and selected metric availability.")
	} else {
		if _, saveErr := s.db.ExecContext(r.Context(), `INSERT INTO ssh_observations (seedbox_id, result, success_at, attempted_at) VALUES (?, ?, ?, ?) ON CONFLICT(seedbox_id) DO UPDATE SET result = excluded.result, success_at = excluded.success_at, attempted_at = excluded.attempted_at, last_error = ''`, id, result.Summary, stamp, stamp); saveErr != nil {
			http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			return
		}
		if result.DirectoryOK {
			data, marshalErr := json.Marshal(result.Directory)
			if marshalErr != nil {
				http.Error(w, errMsgServerErr, http.StatusInternalServerError)
				return
			}
			if _, saveErr := s.db.ExecContext(r.Context(), `UPDATE ssh_observations SET directory_entries = ?, directory_rest_count = ?, directory_rest_size = ?, directory_at = ? WHERE seedbox_id = ?`, string(data), result.RestCount, result.RestSize, stamp, id); saveErr != nil {
				http.Error(w, errMsgServerErr, http.StatusInternalServerError)
				return
			}
		}
		s.setFlash(w, r, "ok", stamp+": "+result.Summary)
	}
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}
