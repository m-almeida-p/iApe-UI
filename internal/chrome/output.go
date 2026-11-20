package chrome

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Page is one entry of list_pages.
type Page struct {
	ID       int
	Title    string
	URL      string
	Selected bool
}

// pageLine matches "2: Fake chat (http://127.0.0.1:51008/) [selected]". The
// title may hold parentheses itself, so the URL is the last group.
var pageLine = regexp.MustCompile(`^(\d+): (.*?)\s*(?:\(([^()\s]*)\))?(\s+\[selected\])?$`)

// ParsePages reads the "## Pages" list that list_pages, new_page and several
// other tools print.
func ParsePages(text string) []Page {
	var pages []Page
	for line := range strings.SplitSeq(text, "\n") {
		m := pageLine.FindStringSubmatch(strings.TrimRight(line, "\r "))
		if m == nil {
			continue
		}
		id, _ := strconv.Atoi(m[1]) // the pattern only lets digits through
		p := Page{ID: id, Title: m[2], URL: m[3], Selected: m[4] != ""}
		if p.URL == "" && strings.Contains(p.Title, ":") && !strings.Contains(p.Title, " ") {
			p.URL, p.Title = p.Title, "" // "1: about:blank" has a URL and no title
		}
		pages = append(pages, p)
	}
	return pages
}

// SelectedPage is the page the tools act on by default.
func SelectedPage(pages []Page) (Page, bool) {
	for _, p := range pages {
		if p.Selected {
			return p, true
		}
	}
	return Page{}, false
}

// ErrNoScriptResult is returned when evaluate_script's output holds no result.
var ErrNoScriptResult = errors.New("evaluate_script printed no result")

const (
	scriptOpen  = "```json"
	scriptClose = "```"
)

// ParseScriptResult returns the JSON an evaluated function returned, which the
// tool prints between a "```json" line and a closing "```". The closing fence
// is the last one in the text, because the result may hold fences of its own: an
// answer with a code block in it, for one. A function that returns nothing
// prints an empty block, and the result is "null".
func ParseScriptResult(text string) ([]byte, error) {
	start := strings.Index(text, scriptOpen)
	end := strings.LastIndex(text, scriptClose)
	if start < 0 || end < start+len(scriptOpen) {
		return nil, fmt.Errorf("%w: %q", ErrNoScriptResult, shorten(text, 120))
	}
	if body := strings.TrimSpace(text[start+len(scriptOpen) : end]); body != "" {
		return []byte(body), nil
	}
	return []byte("null"), nil
}

var consoleLine = regexp.MustCompile(`^msgid=(\d+) \[([^\]]+)\] (.*)$`)

func shorten(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return string(r[:n]) + "…"
	}
	return text
}

// ending is shorten for a text whose end is what matters, such as the output of
// a program that failed: it keeps the last n characters, behind a mark that says
// the start was left out.
func ending(text string, n int) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return text
}
