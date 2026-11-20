package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// dirSink records a run in a directory: a file for each thing of a step, named after the step. What it writes passes a filter first:
// no secret of the run reaches a file. A screenshot is written by the browser and cannot be filtered, but the explorer's pages show
// no secret: the password is typed in the sign-in page, which nothing is recorded from.
type dirSink struct {
	dir string
	// secrets are values (the password) that must not be written.
	secrets []string
	// redacted counts what the filter replaced.
	redacted int
}

// minSecret is the shortest value taken for a secret: a shorter one would take words with it.
const minSecret = 4

var (
	jwtPattern    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}(?:\.[A-Za-z0-9_-]*)?`)
	bearerPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

func (s *dirSink) Path(n int, name, ext string) string {
	return filepath.Join(s.dir, fmt.Sprintf("%02d-%s.%s", n, name, ext))
}

func (s *dirSink) Write(n int, name, ext string, data []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.Path(n, name, ext), []byte(s.clean(string(data))), 0o600)
}

// clean replaces the secrets of the run, and anything that looks like a token, with [redacted].
func (s *dirSink) clean(text string) string {
	for _, secret := range s.secrets {
		if len(secret) >= minSecret {
			s.redacted += strings.Count(text, secret)
			text = strings.ReplaceAll(text, secret, "[redacted]")
		}
	}
	for _, p := range []*regexp.Regexp{bearerPattern, jwtPattern} {
		s.redacted += len(p.FindAllStringIndex(text, -1))
		text = p.ReplaceAllString(text, "[redacted]")
	}
	return text
}
