package historyapi

import (
	"net/http"
	"net/url"
	"strings"

	"iape-ui/internal/core/domain"
)

// cursors reads the Link header of a page: rel=prev leads to the older page and rel=next to the newer one. What is taken from a link
// is its cursor and nothing else, so the link's address, which may be relative or another host, is never followed.
func cursors(h http.Header) (older, newer string) {
	for _, value := range h.Values("Link") {
		for rest := value; ; {
			open := strings.IndexByte(rest, '<')
			if open < 0 {
				break
			}
			end := strings.IndexByte(rest[open:], '>')
			if end < 0 {
				break
			}
			target := rest[open+1 : open+end]
			rest = rest[open+end+1:]
			params := rest
			if next := strings.IndexByte(rest, '<'); next >= 0 {
				params, rest = rest[:next], rest[next:]
			} else {
				rest = ""
			}
			u, err := url.Parse(target)
			if err != nil {
				continue
			}
			for rel := range strings.FieldsSeq(relOf(params)) {
				switch rel {
				case "prev":
					older = cursorOf(u, "page[before]", older)
				case "next":
					newer = cursorOf(u, "page[after]", newer)
				}
			}
		}
	}
	return older, newer
}

// relOf is the value of the rel parameter in the text that follows a link target, in lower case, or "".
func relOf(params string) string {
	for p := range strings.SplitSeq(params, ";") {
		name, value, found := strings.Cut(strings.TrimSpace(p), "=")
		if found && strings.EqualFold(strings.TrimSpace(name), "rel") {
			return strings.ToLower(strings.Trim(strings.TrimSpace(value), `",`))
		}
	}
	return ""
}

// cursorOf is the cursor of the query parameter key, or current when the link has none that a request may carry.
func cursorOf(u *url.URL, key, current string) string {
	c := u.Query().Get(key)
	if c == "" || (domain.PageRequest{Before: c}).Validate() != nil {
		return current
	}
	return c
}
