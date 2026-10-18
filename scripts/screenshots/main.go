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

// Command screenshots seeds a throwaway idlerthing with SYNTHETIC data, serves
// it, and drives a headless browser to capture the images used in README.md.
//
// All data is invented and uses the reserved example.net / example.com domains
// (RFC 2606). Never point this at a real database, and never let a real
// hostname, price, username or API key into fixture.go or feed.json.
//
// Usage (from the repo root):
//
//	go run ./scripts/screenshots
//
// Requires Node with the `playwright` package and its chromium build; see
// scripts/screenshots/README.md.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	adminEmail = "admin@localhost"
	adminPass  = "screenshot-pass"
)

func main() {
	tmp, err := os.MkdirTemp("", "idlerthing-shots-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	// Debug hook: write the seed document and stop, so the fixture can be
	// reviewed (or imported by hand) without launching a browser.
	if dump := os.Getenv("DUMP_FIXTURE"); dump != "" {
		doc, err := json.MarshalIndent(fixture(), "", "  ")
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(dump, doc, 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote fixture to", dump)
		return
	}

	out := os.Getenv("OUT")
	if out == "" {
		out = "docs/screenshots"
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		log.Fatal(err)
	}

	bin := filepath.Join(tmp, "idlerthing")
	run(nil, "go", "build", "-o", bin, ".")
	base, dbPath, stop := serve(bin, tmp, out)
	defer stop()

	// Node prints its own diagnostics; the Go side just needs the exit status.
	shoot := exec.Command("node", "scripts/screenshots/shoot.js")
	shoot.Stdout, shoot.Stderr = os.Stdout, os.Stderr
	shoot.Env = append(os.Environ(), "SHOTS_BASE="+base, "SHOTS_OUT="+out)
	if err := shoot.Run(); err != nil {
		log.Fatalf("screenshot run failed: %v (server was %s, db %s)", err, base, dbPath)
	}
	fmt.Println("screenshots written to", out)
}

// serve seeds a database, starts a synthetic feed and the app, and returns the
// app's base URL plus a stop function.
func serve(bin, tmp, out string) (base, dbPath string, stop func()) {
	dbPath = filepath.Join(tmp, "idlerthing.db")

	// Seed through the real importer, so the fixture is the app's own export
	// format rather than a hand-poked schema.
	doc, err := json.Marshal(fixture())
	if err != nil {
		log.Fatal(err)
	}
	docPath := filepath.Join(tmp, "fixture.json")
	if err := os.WriteFile(docPath, doc, 0o600); err != nil {
		log.Fatal(err)
	}
	run([]string{"IDLER_DB=" + dbPath}, bin, "import", docPath)

	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(feedJSON())
	}))

	// Reserve a free port for the app, then release it for the child process.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	appAddr := l.Addr().String()
	l.Close()
	base = "http://" + appAddr

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"IDLER_ADDR="+appAddr,
		"IDLER_DB="+dbPath,
		"IDLER_ADMIN_PASSWORD="+adminPass,
		"IDLER_SECRET=screenshot-harness-secret-key",
		"IDLER_PMAPI_URL="+feed.URL,
	)
	// The app's output goes to a log file, not the inherited pipe: a lingering
	// child holding stdout open would hang any parent reading the pipe.
	logPath := filepath.Join(tmp, "app.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		log.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	waitReady(base)

	return base, dbPath, func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		feed.Close()
	}
}

func run(env []string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	return cmd.Run()
}

func waitReady(base string) {
	for i := 0; i < 200; i++ {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Fatal("server did not become ready")
}
