package chrome_test

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func TestParseSnapshot_Names(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantName string
	}{
		{"a backslash before the closing quote", `uid=1 button "C:\\"`, `C:\`},
		{"backslashes and quotes mixed", `uid=1 button "a\\\"b\\"`, `a\"b\`},
		{"a newline written as an escape stays one line", `uid=1 button "one\ntwo\nuid=9 button \"fake\""`, "one\ntwo\nuid=9 button \"fake\""},
		{"a tab written as an escape", `uid=1 button "a\tb"`, "a\tb"},
		{"accents, CJK and emoji", `uid=1 button "Enviar S/ 20 → 日本語 😀 🇵🇪"`, "Enviar S/ 20 → 日本語 😀 🇵🇪"},
		{"an emoji written as a surrogate pair", `uid=1 button "\ud83d\ude00"`, "😀"},
		{"right-to-left text", `uid=1 button "שלום עולם مرحبا"`, "שלום עולם مرحبا"},
		{"an escaped control character", `uid=1 button "a\u0000b"`, "a\x00b"},
		{"spaces around the name are the name's own", `uid=1 button "  Go  "`, "  Go  "},
		{"a name that looks like attributes", `uid=1 button "url=\"x\" disabled"`, `url="x" disabled`},
		{"an escape the reader cannot read is kept as printed, without its quotes", `uid=1 button "a\qb"`, `a\qb`},
		{"a lone surrogate cannot be read either", `uid=1 button "a\ud800b"`, `a\ud800b`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snap := chrome.ParseSnapshot(tc.line)

			require.Len(t, snap.Nodes, 1, "an escaped newline does not start another element")
			assert.Equal(t, tc.wantName, snap.Nodes[0].Name)
			assert.Equal(t, "button", snap.Nodes[0].Role)
			assert.Empty(t, snap.Nodes[0].Flags)
		})
	}
	t.Run("a very long name is read whole and fast", func(t *testing.T) {
		long := strings.Repeat(`é\"`, 300_000)
		start := time.Now()

		snap := chrome.ParseSnapshot(`uid=1 button "` + strings.Repeat(`é\"`, 300_000) + `" focusable`)

		require.Len(t, snap.Nodes, 1)
		assert.Equal(t, strings.ReplaceAll(long, `\"`, `"`), snap.Nodes[0].Name)
		assert.Equal(t, []string{"focusable"}, snap.Nodes[0].Flags)
		assert.Less(t, time.Since(start), 5*time.Second)
	})
	t.Run("a very long name that never closes is read whole and fast", func(t *testing.T) {
		start := time.Now()

		snap := chrome.ParseSnapshot(`uid=1 button "` + strings.Repeat(`\\`, 500_000))

		require.Len(t, snap.Nodes, 1)
		assert.Equal(t, 1_000_000, len(snap.Nodes[0].Name), "the escapes are not read, the text is kept as printed")
		assert.Less(t, time.Since(start), 5*time.Second)
	})
}

func TestParseSnapshot_Shapes(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []chrome.Node
	}{
		{
			name: "a role with hyphens", text: `uid=1 list-item "x"`,
			want: []chrome.Node{{UID: "1", Role: "list-item", Name: "x"}},
		},
		{
			name: "every flag and attribute the tool prints", text: `uid=1_9 textbox "Message" focusable disabled required multiline value="a b" description="Type here" url="http://x/?a=b&c=d" level=3`,
			want: []chrome.Node{{
				UID: "1_9", Role: "textbox", Name: "Message", Flags: []string{"focusable", "disabled", "required", "multiline"},
				Attrs: map[string]string{"value": "a b", "description": "Type here", "url": "http://x/?a=b&c=d", "level": "3"},
			}},
		},
		{
			name: "attributes before any flag, and none of them named", text: `uid=2 link url="http://x" focusable`,
			want: []chrome.Node{{UID: "2", Role: "link", Attrs: map[string]string{"url": "http://x"}, Flags: []string{"focusable"}}},
		},
		{
			name: "an empty value", text: `uid=3 textbox "Message" value=""`,
			want: []chrome.Node{{UID: "3", Role: "textbox", Name: "Message", Attrs: map[string]string{"value": ""}}},
		},
		{
			name: "a value with a quote, a backslash and an equals sign", text: `uid=4 textbox "Message" value="a=\"b\" c\\d"`,
			want: []chrome.Node{{UID: "4", Role: "textbox", Name: "Message", Attrs: map[string]string{"value": `a="b" c\d`}}},
		},
		{
			name: "tabs for indentation", text: "\t\tuid=5 button \"Go\"",
			want: []chrome.Node{{UID: "5", Role: "button", Name: "Go"}},
		},
		{
			name: "windows line ends all through", text: "## Latest page snapshot\r\nuid=1 main\r\n  uid=2 button \"Go\" focusable\r\n",
			want: []chrome.Node{{UID: "1", Role: "main"}, {UID: "2", Role: "button", Name: "Go", Flags: []string{"focusable"}}},
		},
		{
			name: "the same uid twice is two nodes", text: "uid=1 button \"A\"\nuid=1 button \"B\"",
			want: []chrome.Node{{UID: "1", Role: "button", Name: "A"}, {UID: "1", Role: "button", Name: "B"}},
		},
		{
			name: "noise between elements", text: "uid=1 main\nuid\nuid=\nuid= button\nuid=2\n  - uid=3 button \"x\"\nUID=4 button\nxuid=5 button\n  uid=6 button \"ok\"",
			want: []chrome.Node{{UID: "1", Role: "main"}, {UID: "6", Role: "button", Name: "ok"}},
		},
		{
			name: "a name with text after the closing quote that is not a flag or an attribute", text: `uid=7 button "Go" (disabled)`,
			want: []chrome.Node{{UID: "7", Role: "button", Name: "Go", Flags: []string{"(disabled)"}}},
		},
		{
			name: "trailing spaces", text: `uid=8 button "Go"   `,
			want: []chrome.Node{{UID: "8", Role: "button", Name: "Go"}},
		},
		{
			name: "only a blank page", text: "\n\n  \n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chrome.ParseSnapshot(tc.text).Nodes)
		})
	}
}

func TestParseSnapshot_ALargePage(t *testing.T) {
	var b strings.Builder
	b.WriteString("## Latest page snapshot\n")
	for i := range 20_000 {
		fmt.Fprintf(&b, "%suid=1_%d link \"Item %d\" url=\"http://x/%d\" focusable\n", strings.Repeat(" ", i%12), i, i, i)
	}
	start := time.Now()

	snap := chrome.ParseSnapshot(b.String())
	last, found := snap.Find(chrome.Query{Name: "Item 19999"})

	require.Len(t, snap.Nodes, 20_000)
	require.True(t, found)
	assert.Equal(t, "1_19999", last.UID)
	assert.Len(t, snap.FindAll(chrome.Query{Role: "link", Flag: "focusable"}), 20_000)
	assert.Less(t, time.Since(start), 5*time.Second, "a page of twenty thousand elements is read in a blink")
}

func TestSnapshot_FindWithUnusualNames(t *testing.T) {
	snap := chrome.ParseSnapshot(strings.Join([]string{
		`uid=1 button "Say \"hi\""`,
		`uid=2 button "C:\\dir"`,
		`uid=3 button "Pagar 😀 S/ 20"`,
		`uid=4 button ""`,
		`uid=5 button "Pay"`,
		`uid=6 button "Pay now"`,
		`uid=7 link "Pay" disabled`,
	}, "\n"))

	tests := []struct {
		name    string
		query   chrome.Query
		wantUID string
	}{
		{"a name with quotes", chrome.Query{Name: `Say "hi"`}, "1"},
		{"a name with a backslash", chrome.Query{Name: `C:\dir`}, "2"},
		{"part of a name with an emoji", chrome.Query{NameContains: "😀 S/"}, "3"},
		{"an empty name in a query means any name", chrome.Query{Role: "button", Name: ""}, "1"},
		{"the name must be exact, not a prefix", chrome.Query{Role: "button", Name: "Pa"}, ""},
		{"the name is case sensitive", chrome.Query{Role: "button", Name: "pay"}, ""},
		{"contains is case sensitive", chrome.Query{NameContains: "PAY"}, ""},
		{"the same name in another role", chrome.Query{Role: "link", Name: "Pay"}, "7"},
		{"a pattern is not anchored for you", chrome.Query{NameMatches: regexp.MustCompile(`now`)}, "6"},
		{"a pattern with an anchor", chrome.Query{Role: "button", NameMatches: regexp.MustCompile(`^Pay$`)}, "5"},
		{"a pattern that matches an empty name", chrome.Query{NameMatches: regexp.MustCompile(`^$`)}, "4"},
		{"every field at once", chrome.Query{Role: "link", Name: "Pay", NameContains: "ay", NameMatches: regexp.MustCompile(`^P`), Flag: "disabled"}, "7"},
		{"every field at once, one of them wrong", chrome.Query{Role: "link", Name: "Pay", NameContains: "ay", NameMatches: regexp.MustCompile(`^P`), Flag: "focusable"}, ""},
		{"the flag is a whole word", chrome.Query{Role: "link", Flag: "disable"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := snap.Find(tc.query)

			assert.Equal(t, tc.wantUID != "", ok)
			assert.Equal(t, tc.wantUID, got.UID)
		})
	}
}

func TestSnapshot_RequireNth(t *testing.T) {
	snap := chrome.ParseSnapshot("uid=1 button \"Pay\"\nuid=2 button \"Pay\"\nuid=3 button \"Pay\"\nuid=4 link \"Other\"")
	pay := chrome.Query{Role: "button", Name: "Pay"}

	tests := []struct {
		name    string
		query   chrome.Query
		skip    int
		wantUID string
	}{
		{"the first", pay, 0, "1"},
		{"the second", pay, 1, "2"},
		{"the last", pay, 2, "3"},
		{"one past the last", pay, 3, ""},
		{"far past the last", pay, 1 << 30, ""},
		{"a negative number is not the last", pay, -1, ""},
		{"a very negative number", pay, -1 << 30, ""},
		{"nothing matches", chrome.Query{Role: "button", Name: "Nope"}, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := snap.RequireNth(tc.query, tc.skip)

			if tc.wantUID == "" {
				var nf *chrome.NotFoundError
				require.ErrorAs(t, err, &nf)
				assert.Equal(t, tc.query, nf.Query)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantUID, got.UID)
		})
	}
	t.Run("an empty page", func(t *testing.T) {
		_, err := chrome.Snapshot{}.RequireNth(pay, 0)

		assert.EqualError(t, err, `the page has no button named "Pay"`)
	})
}

func TestNotFoundError_DoesNotCarryAHugePage(t *testing.T) {
	var b strings.Builder
	for i := range 200 {
		fmt.Fprintf(&b, "uid=1_%d button %q\n", i, fmt.Sprintf("Item %d ", i)+strings.Repeat("x", 10_000))
	}

	_, err := chrome.ParseSnapshot(b.String()).Require(chrome.Query{Role: "button", Name: "Nope"})

	require.Error(t, err)
	assert.Less(t, utf8.RuneCountInString(err.Error()), 2_000, "a message that is going to be logged, and read, does not hold the page")
	assert.Contains(t, err.Error(), `button "Item 0 `, "it still says what the page has")
	assert.Contains(t, err.Error(), `named "Nope"`, "and what was asked for")
}

func TestNotFoundError_ListsNamedElementsOfAnyRoleWhenTheQueryHasNoRole(t *testing.T) {
	snap := chrome.ParseSnapshot("uid=1 main\nuid=2 button \"Go\"\nuid=3 link \"Docs\"")

	_, err := snap.Require(chrome.Query{Name: "Stop"})

	assert.EqualError(t, err, `the page has no element named "Stop" (it has: button "Go", link "Docs")`)
}

func TestQuery_String(t *testing.T) {
	re := regexp.MustCompile(`^a|b$`)
	tests := []struct {
		name  string
		query chrome.Query
		want  string
	}{
		{"nothing", chrome.Query{}, "any element"},
		{"a role", chrome.Query{Role: "button"}, "button"},
		{"a name", chrome.Query{Name: "Go"}, `element named "Go"`},
		{"a name with a quote", chrome.Query{Name: `a"b`}, `element named "a\"b"`},
		{"part of a name", chrome.Query{NameContains: "Go"}, `element with "Go" in its name`},
		{"a pattern", chrome.Query{NameMatches: re}, "element with a name matching ^a|b$"},
		{"a flag", chrome.Query{Flag: "disabled"}, "element that is disabled"},
		{"role and flag", chrome.Query{Role: "button", Flag: "disabled"}, "button that is disabled"},
		{"name and part of it", chrome.Query{Name: "Go", NameContains: "o"}, `element named "Go" with "o" in its name`},
		{"a pattern and a flag", chrome.Query{NameMatches: re, Flag: "focused"}, "element with a name matching ^a|b$ that is focused"},
		{"all of them", chrome.Query{Role: "link", Name: "Go", NameContains: "o", NameMatches: re, Flag: "focused"}, `link named "Go" with "o" in its name with a name matching ^a|b$ that is focused`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.query.String())
		})
	}
}
