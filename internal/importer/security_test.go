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

package importer

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// assertWarningBudget verifies warnings is exactly at the maxWarnings cap,
// every entry is within maxWarningBytes and valid UTF-8, the total bytes stay
// within the overall budget, and the final entry is the suppression notice.
func assertWarningBudget(t *testing.T, warnings []string) {
	t.Helper()
	if len(warnings) != maxWarnings {
		t.Fatalf("got %d warnings", len(warnings))
	}
	total := 0
	for _, warning := range warnings {
		total += len(warning)
		if len(warning) > maxWarningBytes || !utf8.ValidString(warning) {
			t.Fatalf("invalid diagnostic: %d bytes", len(warning))
		}
	}
	if total > maxWarnings*maxWarningBytes {
		t.Fatalf("warning bytes: %d", total)
	}
	if !strings.Contains(warnings[len(warnings)-1], "suppressed") {
		t.Fatal("missing suppression notice")
	}
}

// TestMyJSONWarningBudget verifies a my-idlers JSON import with far more
// invalid IPs than maxWarnings stays within the diagnostic budget end to end
// (parse warnings and the subsequent ImportMyIdlers warnings).
func TestMyJSONWarningBudget(t *testing.T) {
	ips := make([]map[string]string, maxWarnings+20)
	for i := range ips {
		ips[i] = map[string]string{"address": "invalid"}
	}
	// A single large child must also be bounded before %q formats it.
	ips[0]["address"] = strings.Repeat("é", 20000)
	data, err := json.Marshal([]any{map[string]any{"hostname": strings.Repeat("h", maxImportHostnameBytes), "ips": ips}})
	if err != nil {
		t.Fatal(err)
	}
	records, warnings, err := ParseMyJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || len(records[0].IPs) != 0 {
		t.Fatal("invalid IPs were retained")
	}
	assertWarningBudget(t, warnings)
	summary, err := ImportMyIdlers(context.Background(), testDB(t), records, warnings)
	if err != nil {
		t.Fatal(err)
	}
	assertWarningBudget(t, summary.Warnings)
}

// TestNativeWarningBudget verifies a native export with far more
// out-of-range disk entries than maxWarnings stays within the diagnostic budget.
func TestNativeWarningBudget(t *testing.T) {
	disks := make([]any, maxWarnings+20)
	for i := range disks {
		disks[i] = map[string]any{"size_as_mb": int64(1) << 40}
	}
	data, err := json.Marshal(map[string]any{"format": 1, "servers": []any{map[string]any{
		"server": map[string]any{"hostname": strings.Repeat("h", maxImportHostnameBytes)}, "disks": disks,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Import(context.Background(), testDB(t), bytes.NewReader(data), false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Servers != 1 || summary.Disks != 0 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	assertWarningBudget(t, summary.Warnings)
}

// TestImportHostnameLimits verifies hostnames beyond maxImportHostnameBytes
// are skipped with a warning while valid rows remain importable in all formats.
func TestImportHostnameLimits(t *testing.T) {
	host := strings.Repeat("h", maxImportHostnameBytes+1)
	data, err := json.Marshal([]any{
		map[string]any{"hostname": "before.example"},
		map[string]any{"hostname": host},
		map[string]any{"hostname": "after.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var csvData bytes.Buffer
	writer := csv.NewWriter(&csvData)
	if err := writer.WriteAll([][]string{
		{"hostname", "server_type_name"}, {"before.example", "KVM"},
		{host, "KVM"}, {"after.example", "KVM"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name       string
		parse      func() ([]MyServer, []string, error)
		warningRow string
	}{
		{"JSON", func() ([]MyServer, []string, error) { return ParseMyJSON(bytes.NewReader(data)) }, "row 2"},
		{"CSV", func() ([]MyServer, []string, error) { return ParseMyCSV(bytes.NewReader(csvData.Bytes())) }, "row 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records, warnings, err := tc.parse()
			if err != nil {
				t.Fatal(err)
			}
			if len(records) != 2 || records[0].Hostname != "before.example" || records[1].Hostname != "after.example" {
				t.Fatalf("valid rows lost: %+v", records)
			}
			if len(warnings) != 1 || !strings.Contains(warnings[0], tc.warningRow+": hostname exceeds") || len(warnings[0]) > maxWarningBytes {
				t.Fatalf("unexpected warnings: %v", warnings)
			}
			summary, err := ImportMyIdlers(context.Background(), testDB(t), records, warnings)
			if err != nil {
				t.Fatal(err)
			}
			if summary.Imported != 2 || len(summary.Warnings) != 1 {
				t.Fatalf("unexpected import: %+v", summary)
			}
		})
	}
	native, err := json.Marshal(map[string]any{"format": 1, "servers": []any{
		map[string]any{"server": map[string]any{"hostname": host}},
		map[string]any{"server": map[string]any{"hostname": "valid.example", "server_type": 1}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	summary, err := Import(context.Background(), testDB(t), bytes.NewReader(native), false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Servers != 1 || len(summary.Warnings) != 1 || !strings.Contains(summary.Warnings[0], "hostname exceeds") {
		t.Fatalf("unexpected native result: %+v", summary)
	}
}

// TestDiagnosticTextValidUTF8 verifies normalization and truncation preserve valid UTF-8.
func TestDiagnosticTextValidUTF8(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		limit             int
	}{
		{"valid short", "hello", "hello", 8},
		{"invalid short", "a\xffb", "a\uFFFDb", 8},
		{"incomplete sequence", "a\xe2\x82", "a\uFFFD", 8},
		{"invalid truncated prefix", "a\xffbcdefgh", "a\uFFFDb...", 8},
		{"normalization exceeds limit", "a\xffb", "a...", 4},
		{"multibyte boundary", "abc\u754cdef", "abc...", 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := diagnosticText(tc.input, tc.limit)
			if got != tc.want || !utf8.ValidString(got) || len(got) > tc.limit {
				t.Fatalf("diagnosticText = %q, want %q within %d bytes", got, tc.want, tc.limit)
			}
		})
	}
}

// TestWarningFormattingStopsAtBudget verifies warnf stops formatting (and
// never calls Error on an argument) once the warning budget is exhausted.
func TestWarningFormattingStopsAtBudget(t *testing.T) {
	var warnings []string
	text := strings.Repeat("界", 100000)
	for range maxWarnings + 20 {
		warnf(&warnings, "%q: invalid %q", text, text)
	}
	assertWarningBudget(t, warnings)
	// A full budget must not call Error (or format attacker-controlled text).
	warnf(&warnings, "%v", forbiddenDiagnostic{t})
}

// forbiddenDiagnostic fails the test if its Error method is ever called,
// catching a warnf call that formats arguments after the budget is exhausted.
type forbiddenDiagnostic struct{ t *testing.T }

// Error fails the test: it must never be called once the warning budget is exhausted.
func (d forbiddenDiagnostic) Error() string { d.t.Fatal("formatted after budget exhausted"); return "" }
