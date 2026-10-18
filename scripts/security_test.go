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

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func writeFixture(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("git", "init", "--quiet", dir)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	return dir
}

func trackFixture(t *testing.T, dir, path string) {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "add", "--", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, output)
	}
}

func runScript(t *testing.T, dir, script string, env []string, args ...string) ([]byte, error) {
	t.Helper()
	path, err := filepath.Abs(script)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{path}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}

func TestHeaderRewriteRejectsSymlinks(t *testing.T) {
	for _, parent := range []bool{false, true} {
		for _, check := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{false: "file", true: "parent"}[parent], map[bool]string{false: "rewrite", true: "check"}[check]}, "/"), func(t *testing.T) {
				dir := fixtureRepo(t)
				private := filepath.Join(t.TempDir(), "private.css")
				secret := "synthetic private content"
				writeFixture(t, private, secret)
				path := "internal/web/assets/static/private.css"
				target := filepath.Join(dir, path)
				if parent {
					writeFixture(t, target, "original")
					trackFixture(t, dir, path)
					if err := os.RemoveAll(filepath.Dir(target)); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(filepath.Dir(private), filepath.Dir(target)); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(private, target); err != nil {
						t.Fatal(err)
					}
					trackFixture(t, dir, path)
				}
				var args []string
				if check {
					args = append(args, "--check")
				}
				output, err := runScript(t, dir, "license-headers.sh", nil, args...)
				if err == nil || !strings.Contains(string(output), "symlink") {
					t.Fatalf("unsafe source accepted: %v: %s", err, output)
				}
				content, err := os.ReadFile(private)
				if err != nil || string(content) != secret {
					t.Fatal("private target was changed")
				}
				linkedPath := target
				if parent {
					linkedPath = filepath.Dir(target)
				}
				info, err := os.Lstat(linkedPath)
				if err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("symlink was replaced")
				}
			})
		}
	}
}

func TestHeaderRewritePreservesFilenameBoundaries(t *testing.T) {
	dir := fixtureRepo(t)
	for _, path := range []string{"-asset.js", "internal/web/assets/static/space and\nnewline.css"} {
		writeFixture(t, filepath.Join(dir, path), "body {}\n")
		trackFixture(t, dir, path)
	}
	if output, err := runScript(t, dir, "license-headers.sh", nil); err != nil {
		t.Fatalf("rewrite: %v: %s", err, output)
	}
	if output, err := runScript(t, dir, "license-headers.sh", nil, "--check"); err != nil {
		t.Fatalf("check: %v: %s", err, output)
	}
}

func TestNoticeGeneratorRejectsVendorSymlinks(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "parent"}[parent], func(t *testing.T) {
			dir, env := noticeFixture(t)
			private := filepath.Join(t.TempDir(), "private.LICENSE")
			writeFixture(t, private, "synthetic private license content")
			vendor := filepath.Join(dir, "internal/web/assets/vendor")
			if parent {
				if err := os.RemoveAll(vendor); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Dir(private), vendor); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink(private, filepath.Join(vendor, "private.LICENSE")); err != nil {
					t.Fatal(err)
				}
			}
			writeFixture(t, filepath.Join(dir, "THIRD_PARTY_LICENSES.md"), "previous notices")
			output, err := runScript(t, dir, "gen-third-party-licenses.sh", env)
			if err == nil || !strings.Contains(string(output), "symlink") {
				t.Fatalf("unsafe vendor accepted: %v: %s", err, output)
			}
			content, err := os.ReadFile(filepath.Join(dir, "THIRD_PARTY_LICENSES.md"))
			if err != nil || string(content) != "previous notices" {
				t.Fatal("failed generation replaced notices")
			}
		})
	}
}

func noticeFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := fixtureRepo(t)
	tools := t.TempDir()
	goroot, cache := filepath.Join(tools, "goroot"), filepath.Join(tools, "cache")
	writeFixture(t, filepath.Join(goroot, "LICENSE"), "Copyright 2026 Go Authors\n")
	writeFixture(t, filepath.Join(cache, "example.com/dep@v1.0.0/LICENSE"), "Permission granted.\n")
	writeFixture(t, filepath.Join(dir, "internal/web/assets/vendor/asset.LICENSE"), "Copyright 2026 Asset Authors\n")
	// Isolate the shell generator from toolchain builds, while exercising its
	// real file selection, content copying and copyright extraction.
	fakeGo := filepath.Join(tools, "go")
	writeFixture(t, fakeGo, `#!/bin/bash
set -eu
case "$1 $2" in
 'env GOROOT') printf '%s\n' "$FIXTURE_GOROOT" ;;
 'env GOMODCACHE') printf '%s\n' "$FIXTURE_MODCACHE" ;;
 'build -o') : > "$3" ;;
 'version -m') printf '\tdep\texample.com/dep\tv1.0.0\n' ;;
 *) exit 1 ;;
esac
`)
	if err := os.Chmod(fakeGo, 0700); err != nil {
		t.Fatal(err)
	}
	return dir, []string{"PATH=" + tools + string(os.PathListSeparator) + os.Getenv("PATH"), "FIXTURE_GOROOT=" + goroot, "FIXTURE_MODCACHE=" + cache}
}

func TestNoticeGeneratorPreservesFilenameBoundaries(t *testing.T) {
	dir, env := noticeFixture(t)
	var cache string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "FIXTURE_MODCACHE="); ok {
			cache = value
		}
	}
	private := filepath.Join(t.TempDir(), "private.go")
	writeFixture(t, private, "Copyright 2026 PRIVATE-SENTINEL\n")
	// With whitespace splitting this path becomes grep options and an outside
	// file operand. The NUL pipeline must treat it as one ordinary source path.
	source := filepath.Join(cache, "example.com/dep@v1.0.0/testdata", " -e .* "+private+" end.go")
	writeFixture(t, source, "// Copyright 2026 Dependency Authors\n")
	writeFixture(t, filepath.Join(cache, "example.com/dep@v1.0.0/NOTICE extra\nlines"), "Additional notice without copyright.\n")
	output, err := runScript(t, dir, "gen-third-party-licenses.sh", env)
	if err != nil {
		t.Fatalf("generator: %v: %s", err, output)
	}
	content, err := os.ReadFile(filepath.Join(dir, "THIRD_PARTY_LICENSES.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Copyright 2026 Dependency Authors") || !strings.Contains(string(content), "Additional notice without copyright.") || strings.Contains(string(content), "PRIVATE-SENTINEL") {
		t.Fatalf("incorrect notices: %s", content)
	}
}
