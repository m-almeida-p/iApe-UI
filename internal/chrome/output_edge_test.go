package chrome_test

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func TestParsePages_Lines(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []chrome.Page
	}{
		{"a title with brackets and a colon", "2: [Draft] Re: plan (v2) (http://x/a)", []chrome.Page{{ID: 2, Title: "[Draft] Re: plan (v2)", URL: "http://x/a"}}},
		{"a title that says it is selected, on a page that is not", "2: Foo [selected] (http://x/)", []chrome.Page{{ID: 2, Title: "Foo [selected]", URL: "http://x/"}}},
		{"a title that says it is selected, on a page that is", "2: Foo [selected] (http://x/) [selected]", []chrome.Page{{ID: 2, Title: "Foo [selected]", URL: "http://x/", Selected: true}}},
		{"no title", "2:  (http://x/)", []chrome.Page{{ID: 2, URL: "http://x/"}}},
		{"no title, selected", "2:  (http://x/) [selected]", []chrome.Page{{ID: 2, URL: "http://x/", Selected: true}}},
		{"a blank page that is selected", "1: about:blank [selected]", []chrome.Page{{ID: 1, URL: "about:blank", Selected: true}}},
		{"a title that ends with a smiley", "2: Great :)", []chrome.Page{{ID: 2, Title: "Great :)"}}},
		{"a closing parenthesis with nothing to close", "2: Tiny) (http://x/", []chrome.Page{{ID: 2, Title: "Tiny) (http://x/"}}},
		{"a title with parentheses and no address", "3: Docs (beta)", []chrome.Page{{ID: 3, Title: "Docs (beta)"}}},
		{"an address with parentheses", "4: Go (programming language) (https://en.wikipedia.org/wiki/Go_(programming_language)) [selected]", []chrome.Page{{ID: 4, Title: "Go (programming language)", URL: "https://en.wikipedia.org/wiki/Go_(programming_language)", Selected: true}}},
		{"an address with a query and an anchor", "5: Search (http://x/s?q=a%20b&r=(1)#top)", []chrome.Page{{ID: 5, Title: "Search", URL: "http://x/s?q=a%20b&r=(1)#top"}}},
		{"an address with spaces is not an address", "6: T (http://a b/)", []chrome.Page{{ID: 6, Title: "T (http://a b/)"}}},
		{"windows line ends", "1: about:blank\r\n2: Chat (http://x/) [selected]\r\n", []chrome.Page{{ID: 1, URL: "about:blank"}, {ID: 2, Title: "Chat", URL: "http://x/", Selected: true}}},
		{"a title with emoji and accents", "7: Pagos 💸 Ñandú (http://x/é)", []chrome.Page{{ID: 7, Title: "Pagos 💸 Ñandú", URL: "http://x/é"}}},
		{"ids with several digits", "12: A (http://x/)\n123456: B (http://y/)", []chrome.Page{{ID: 12, Title: "A", URL: "http://x/"}, {ID: 123456, Title: "B", URL: "http://y/"}}},
		{"lines that are not pages", "## Pages\nSuccessfully reloaded.\n  3: indented\nx: y (z)\n: nothing\n", nil},
		{"no pages", "", nil},
		{"an id too big for a number is not a page", "99999999999999999999: A (http://x/)", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chrome.ParsePages(tc.text))
		})
	}
}

func TestSelectedPage(t *testing.T) {
	t.Run("the first one selected", func(t *testing.T) {
		got, ok := chrome.SelectedPage([]chrome.Page{{ID: 1}, {ID: 2, Selected: true}, {ID: 3, Selected: true}})

		require.True(t, ok)
		assert.Equal(t, 2, got.ID)
	})
	t.Run("no pages", func(t *testing.T) {
		_, ok := chrome.SelectedPage(nil)

		assert.False(t, ok)
	})
}

func TestParseScriptResult_Shapes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{"text before and after the block", "Script ran on page and returned:\n```json\n{\"a\":1}\n```\n\n## Console\nnothing", `{"a":1}`},
		{"no newline after the fence", "```json{\"a\":1}```", `{"a":1}`},
		{"windows line ends", "Script ran on page and returned:\r\n```json\r\n[1,2]\r\n```\r\n", "[1,2]"},
		{"an empty body is null", "```json\n```", "null"},
		{"a body of spaces is null", "```json\n   \n\t\n```", "null"},
		{"open and close with nothing between", "```json```", "null"},
		{"undefined is passed on as the tool printed it", "```json\nundefined\n```", "undefined"},
		{"a string holding a fence", "```json\n\"```\"\n```", "\"```\""},
		{"a result that ends with a fence of its own and the tool's", "```json\n\"x\\n```\"\n```", "\"x\\n```\""},
		{"a result that ends with the word json", "x ```json\n\"```json\"\n``` y", "\"```json\""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chrome.ParseScriptResult(tc.text)

			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
	t.Run("two blocks are one span, which the caller then cannot read", func(t *testing.T) {
		got, err := chrome.ParseScriptResult("```json\n1\n```\n```json\n2\n```")

		require.NoError(t, err)
		assert.Equal(t, "1\n```\n```json\n2", string(got), "from the first opening to the last closing: the tool prints one block")
	})
	t.Run("a closing fence before the opening one is no result", func(t *testing.T) {
		_, err := chrome.ParseScriptResult("```\nsome code\n```\nthen ```json")

		require.ErrorIs(t, err, chrome.ErrNoScriptResult)
	})
	t.Run("a fence that is not json", func(t *testing.T) {
		_, err := chrome.ParseScriptResult("```\n{\"a\":1}\n```")

		require.ErrorIs(t, err, chrome.ErrNoScriptResult)
	})
	t.Run("a huge result is returned whole", func(t *testing.T) {
		big := `"` + strings.Repeat("é", 1_000_000) + `"`

		got, err := chrome.ParseScriptResult("```json\n" + big + "\n```")

		require.NoError(t, err)
		assert.Equal(t, len(big), len(got))
	})
	t.Run("the error shows a little of a huge text", func(t *testing.T) {
		_, err := chrome.ParseScriptResult(strings.Repeat("no result here ", 100_000))

		require.ErrorIs(t, err, chrome.ErrNoScriptResult)
		assert.Less(t, utf8.RuneCountInString(err.Error()), 250)
	})
}

func TestShortenAndEnding(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		n          int
		wantShort  string
		wantEnding string
	}{
		{"n is zero", "abc", 0, "…", "…"},
		{"n is zero and the text is empty", "", 0, "", ""},
		{"n is larger than the text", "abc", 100, "abc", "abc"},
		{"n is the length of the text", "abc", 3, "abc", "abc"},
		{"one less than the length", "abc", 2, "ab…", "…bc"},
		{"white space is one space", "a \t\n  b\r\nc", 100, "a b c", "a b c"},
		{"emoji are not cut in half", "😀😀😀😀", 3, "😀😀😀…", "…😀😀😀"},
		{"combining accents count as characters, and the bytes stay valid", "ééé", 3, "ée…", "…́é"},
		{"CJK", "日本語日本語", 4, "日本語日…", "…語日本語"},
		{"right to left", "שלום עולם", 4, "שלום…", "…עולם"},
		{"bytes that are not UTF-8 become the replacement character", "a\xffb\xfe", 3, "a�b…", "…�b�"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			short, end := chrome.Shorten(tc.text, tc.n), chrome.Ending(tc.text, tc.n)

			assert.Equal(t, tc.wantShort, short)
			assert.Equal(t, tc.wantEnding, end)
			assert.True(t, utf8.ValidString(short), "shorten gives valid UTF-8")
			assert.True(t, utf8.ValidString(end), "ending gives valid UTF-8")
		})
	}
	t.Run("a huge text is cut fast", func(t *testing.T) {
		huge := strings.Repeat("😀 x ", 200_000)
		start := time.Now()

		short, end := chrome.Shorten(huge, 10), chrome.Ending(huge, 10)

		assert.Equal(t, 11, utf8.RuneCountInString(short))
		assert.Equal(t, 11, utf8.RuneCountInString(end))
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

func TestScriptName(t *testing.T) {
	tests := []struct {
		name     string
		function string
		want     string
	}{
		{"a one-line arrow function", "() => document.title", "() => document.title"},
		{"only the first line", "() => {\n  return 1\n}", "() => {"},
		{"leading space and blank lines are skipped", "\n\n   () => 1\n", "() => 1"},
		{"windows line ends", "() => {\r\n  return 1\r\n}", "() => {"},
		{"nothing", "", ""},
		{"only white space", " \n\t ", ""},
		{"a long line is cut", "() => " + strings.Repeat("x", 100), "() => " + strings.Repeat("x", 42) + "…"},
		{"a line of exactly the size is whole", strings.Repeat("y", 48), strings.Repeat("y", 48)},
		{"a long line of emoji is cut without breaking one", strings.Repeat("😀", 60), strings.Repeat("😀", 48) + "…"},
		{"inner spaces are one", "()   =>    1", "() => 1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chrome.ScriptName(tc.function)

			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got))
		})
	}
}
