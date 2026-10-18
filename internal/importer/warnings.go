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
	"fmt"
	"unicode/utf8"
)

const (
	maxImportHostnameBytes = 253
	maxWarnings            = 1000
	maxWarningBytes        = 1024
	maxWarningArgBytes     = 256
)

// warnf bounds both formatting work and retained diagnostics. All call sites
// pass raw arguments so large strings are shortened BEFORE formatting. With
// fixed count and per-warning limits, retained warnings use at most 1 MiB.
func warnf(warnings *[]string, format string, args ...any) {
	if len(*warnings) >= maxWarnings {
		return
	}
	if len(*warnings) == maxWarnings-1 {
		*warnings = append(*warnings, "Further import warnings suppressed (limit reached).")
		return
	}
	for i, arg := range args {
		switch v := arg.(type) {
		case string:
			args[i] = diagnosticText(v, maxWarningArgBytes)
		case error:
			args[i] = diagnosticText(v.Error(), maxWarningArgBytes)
		}
	}
	*warnings = append(*warnings, diagnosticText(fmt.Sprintf(format, args...), maxWarningBytes))
}

// diagnosticText truncates s to at most limit bytes, appending "..." and
// backing up to a UTF-8 rune boundary so the result is never split mid-rune.
func diagnosticText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit - len("...")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}
