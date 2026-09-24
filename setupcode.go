package main

import (
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// An instance without a passkey belongs to whoever registers the first
// one. Without a secret that would be whoever reaches /setup before the
// operator does — including, after a recovery, all tasks and the webhook
// token. The setup code therefore only appears where exclusively the
// operator can reach it: in the server log and in a file next to the DB
// (in the container /data, on the host ./data).

const setupCodeFile = "setup-code.txt"

// Without 0/O and 1/I/L: the code gets typed in by hand from the log.
const setupCodeAlphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

// prepareSetupCode creates a code for an instance without a passkey (once
// per process) and removes it as soon as a passkey exists.
func (s *server) prepareSetupCode() error {
	setup, err := s.setupMode()
	if err != nil {
		return err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	path := s.setupCodePath()
	if !setup {
		s.setupCode = ""
		if path != "" {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if s.setupCode == "" {
		s.setupCode = newSetupCode()
	}
	if path == "" {
		log.Printf("Setup code for the first passkey: %s", s.setupCode)
		return nil
	}
	if err := os.WriteFile(path, []byte(s.setupCode+"\n"), 0o600); err != nil {
		return err
	}
	log.Printf("Setup code for the first passkey: %s (also in %s)", s.setupCode, path)
	return nil
}

func (s *server) setupCodePath() string {
	if s.cfg.dbPath == "" || strings.HasPrefix(s.cfg.dbPath, ":memory:") {
		return ""
	}
	return filepath.Join(filepath.Dir(s.cfg.dbPath), setupCodeFile)
}

// checkSetupCode compares the X-Setup-Code header with the current code.
// Case, whitespace, and hyphens don't matter.
func (s *server) checkSetupCode(r *http.Request) bool {
	got := normalizeSetupCode(r.Header.Get("X-Setup-Code"))
	s.setupMu.Lock()
	want := normalizeSetupCode(s.setupCode)
	s.setupMu.Unlock()
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func normalizeSetupCode(c string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return -1
	}, c)
}

// newSetupCode: 16 characters out of 31 (~79 bits), in groups of four.
func newSetupCode() string {
	var b strings.Builder
	buf := make([]byte, 1)
	limit := 256 - 256%len(setupCodeAlphabet) // uniform distribution: discard the remainder
	for n := 0; n < 16; {
		rand.Read(buf)
		if int(buf[0]) >= limit {
			continue
		}
		if n > 0 && n%4 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(setupCodeAlphabet[int(buf[0])%len(setupCodeAlphabet)])
		n++
	}
	return b.String()
}
