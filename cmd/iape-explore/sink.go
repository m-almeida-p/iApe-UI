package main

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// Path is the file of a step: always one file directly in the directory of the sink, whatever the name and the extension say. A path
// separator in them would make the name a way out of the directory ("/../x" is "..", once the "01-" in front of it is taken as a
// directory and cleaned away), so they are replaced; so is a NUL, which no file name can hold.
func (s *dirSink) Path(n int, name, ext string) string {
	file := strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, fmt.Sprintf("%02d-%s.%s", n, name, ext))
	return filepath.Join(s.dir, file)
}

func (s *dirSink) Write(n int, name, ext string, data []byte) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.Path(n, name, ext), []byte(s.clean(string(data))), 0o600)
}

// clean replaces the secrets of the run, and anything that looks like a token, with [redacted]. Secrets that overlap, or contain one
// another, are found in the text as it was, and the stretches they cover are joined before anything is replaced: replacing one at a
// time would leave the tail of a longer secret behind a shorter one that was replaced first.
func (s *dirSink) clean(text string) string {
	type span struct{ from, to int }
	var spans []span
	for _, secret := range s.secrets {
		if len(secret) < minSecret {
			continue
		}
		for from := 0; ; from++ {
			i := strings.Index(text[from:], secret)
			if i < 0 {
				break
			}
			from += i
			spans = append(spans, span{from, from + len(secret)})
		}
	}
	if len(spans) > 0 {
		slices.SortFunc(spans, func(a, b span) int { return cmp.Or(cmp.Compare(a.from, b.from), cmp.Compare(b.to, a.to)) })
		var b strings.Builder
		at, cur := 0, spans[0]
		flush := func() {
			b.WriteString(text[at:cur.from])
			b.WriteString("[redacted]")
			at = cur.to
			s.redacted++
		}
		for _, sp := range spans[1:] {
			if sp.from <= cur.to { // overlaps or touches the one before
				cur.to = max(cur.to, sp.to)
				continue
			}
			flush()
			cur = sp
		}
		flush()
		b.WriteString(text[at:])
		text = b.String()
	}
	for _, p := range []*regexp.Regexp{bearerPattern, jwtPattern} {
		s.redacted += len(p.FindAllStringIndex(text, -1))
		text = p.ReplaceAllString(text, "[redacted]")
	}
	return text
}
