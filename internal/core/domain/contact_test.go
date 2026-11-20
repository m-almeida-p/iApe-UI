package domain_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

func namesOf(cs []domain.Contact) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

var defaultNames = []string{"Juan Perez", "Maria Torres", "Carlos Diaz", "Ana Rojas", "Luis Vargas"}

// hostileQueries are what a person, or a script, may type after the @.
var hostileQueries = map[string]string{
	"only the at sign":                    "@",
	"two at signs":                        "@@",
	"at signs and spaces":                 " @ @ ",
	"spaces":                              "   ",
	"tabs and newlines":                   "\t\n\r",
	"a tab and a newline":                 "ana\tmaria\nx",
	"a NUL":                               "\x00",
	"a NUL in a name":                     "ana\x00rojas",
	"control characters":                  "\x01\x02\x1b[31mred",
	"bytes that are not UTF-8":            "\xff\xfe ana \xc3",
	"a right-to-left override":            "\u202eana\u202c rojas",
	"a zero width space":                  "an\u200ba",
	"a lone combining mark":               "́",
	"a combining mark alone, with a name": "́ rojas",
	"a Turkish dotted I":                  "İstanbul",
	"a German sharp s":                    "Straße",
	"regular expression signs":            ".*+?^$[]{}|\\",
	"glob signs":                          "*?[a-z]",
	"SQL signs":                           "'; DROP TABLE messages;--",
	"markup":                              `<script>alert(1)</script>`,
	"a percent escape":                    "%00%0a",
	"parentheses, which close a mention":  "ana) @(root",
	"emoji":                               "🙂🙂🙂",
	"61 letters of two bytes":             strings.Repeat("ñ", 61),
	"10000 letters of two bytes":          strings.Repeat("ñ", 10_000),
	"10000 letters of four bytes":         strings.Repeat("𝒜", 10_000),
	"a long run of spaces and a name":     strings.Repeat(" ", 10_000) + "ana",
	"a name, spaces, and a name":          "ana" + strings.Repeat(" ", 10_000) + "luis",
}

func TestCandidates_WhateverIsTypedTheOffersAreUsable(t *testing.T) {
	for name, query := range hostileQueries {
		t.Run(name, func(t *testing.T) {
			var got []domain.Contact
			require.NotPanics(t, func() { got = domain.Candidates(query) })

			require.NotEmpty(t, got, "there is always somebody to choose")
			assert.LessOrEqual(t, len(got), 5)
			for _, c := range got {
				assert.True(t, utf8.ValidString(c.Name), "%q", c.Name)
				assert.Equal(t, strings.TrimSpace(c.Name), c.Name, "no space around a name: %q", c.Name)
				assert.NotEmpty(t, c.Name)
				assert.LessOrEqual(t, utf8.RuneCountInString(c.Name), 70, "a long query is cut")
				assert.Regexp(t, `^9\d{8}$`, c.Phone, "%q", c.Name)
				for _, r := range c.Name {
					assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '(' || r == ')',
						"%q has %U, which has no place in a name that goes in a message as @(Name)", c.Name, r)
				}
			}
			assert.Equal(t, got, domain.Candidates(query), "the same query offers the same people")
		})
	}
}

func TestCandidates_NothingToGoOnOffersTheDefaults(t *testing.T) {
	for name, query := range map[string]string{
		"nothing": "", "only the at sign": "@", "two at signs": "@@", "at signs and spaces": " @ @ ", "spaces": "   ",
		"tabs and newlines": "\t\n\r", "a NUL": "\x00", "control characters": "\x01\x02",
		"a right-to-left override": "\u202e", "a zero width space": "\u200b", "only parentheses": "()",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, defaultNames, namesOf(domain.Candidates(query)))
		})
	}
}

func TestCandidates_ASpaceOfAnyKindSeparatesWords(t *testing.T) {
	want := namesOf(domain.Candidates("ana lucia"))
	require.Equal(t, "Ana Lucia", want[0])

	for name, query := range map[string]string{
		"a tab": "ana\tlucia", "a newline": "ana\nlucia", "a NUL": "ana\x00lucia", "many spaces": "  ana     lucia ",
		"a no-break space": "ana lucia", "an at sign first": "@ana lucia", "parentheses": "ana(lucia)",
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, namesOf(domain.Candidates(query)))
		})
	}
}

func TestCandidates_TheQueryIsCutBeforeItIsUsed(t *testing.T) {
	cut := strings.Repeat("ñ", 60)

	assert.Equal(t, domain.Candidates(cut), domain.Candidates(cut+"ñ"), "61 letters are the same as the first 60")
	assert.Equal(t, domain.Candidates(cut), domain.Candidates(strings.Repeat("ñ", 10_000)))
	first := domain.Candidates(strings.Repeat("ñ", 10_000))[0].Name
	assert.True(t, strings.HasPrefix(first, "Ñ"+strings.Repeat("ñ", 59)+" "), "the name is what was looked at, and then a last name: %q", first)
	assert.LessOrEqual(t, utf8.RuneCountInString(first), 60+1+len("Castillo"))
	assert.NotEqual(t, domain.Candidates(strings.Repeat("ñ", 59)), domain.Candidates(cut), "and 59 is not 60")
}

func TestCandidates_Accents(t *testing.T) {
	t.Run("capitals and the marks of Spanish are ignored when looking for a default contact", func(t *testing.T) {
		for query, want := range map[string]string{
			"JUAN": "Juan Perez", "PÉREZ": "Juan Perez", "pérez": "Juan Perez", "MARÍA": "Maria Torres", "Díaz": "Carlos Diaz",
			"ROJAS": "Ana Rojas", "vargás": "Luis Vargas", "Lüis": "Luis Vargas", "carlós": "Carlos Diaz",
		} {
			assert.Equal(t, []string{want}, namesOf(domain.Candidates(query)), query)
		}
	})

	t.Run("a name with marks is offered with its marks, capitalised", func(t *testing.T) {
		for query, want := range map[string]string{
			"ÁNGEL": "Ángel", "ñandú": "Ñandú", "ÜNAL": "Ünal", "ñu": "Ñu",
		} {
			got := domain.Candidates(query)
			assert.True(t, strings.HasPrefix(got[0].Name, want+" "), "%q: %q", query, got[0].Name)
		}
	})

	t.Run("the same person written with and without a mark has the same phone", func(t *testing.T) {
		assert.Equal(t, domain.Candidates("Ángel Núñez")[0].Phone, domain.Candidates("angel nunez")[0].Phone)
		assert.Equal(t, domain.Candidates("ÁNGEL NÚÑEZ")[0].Phone, domain.Candidates("angel nunez")[0].Phone)
	})

	t.Run("a letter Spanish does not fold is not folded", func(t *testing.T) {
		assert.NotEqual(t, domain.Candidates("Strasse")[0].Phone, domain.Candidates("Straße")[0].Phone)
		assert.NotEqual(t, domain.Candidates("Ça")[0].Phone, domain.Candidates("Ca")[0].Phone)
	})

	t.Run("odd lower-casing does not empty a name or break it", func(t *testing.T) {
		for _, query := range []string{"İ", "İstanbul", "ß", "ǅ", "ŉ", "ǰ"} {
			for _, c := range domain.Candidates(query) {
				assert.NotEmpty(t, strings.TrimSpace(c.Name), query)
				assert.True(t, utf8.ValidString(c.Name), "%q", c.Name)
			}
		}
	})
}

func TestCandidates_TheNameAsTyped(t *testing.T) {
	tests := []struct {
		query string
		want  string
	}{
		{"rosa diaz", "Rosa Diaz"},
		{"ROSA   DIAZ", "Rosa Diaz"},
		{"o'brien", "O'Brien"},
		{"jean-luc picard", "Jean-Luc Picard"},
		{"d'angelo-smith", "D'Angelo-Smith"},
		{"rosa-", "Rosa-"},
		{"-rosa", "-Rosa"},
		{"'", "'"},
		{"123", "123"},
		{"rosa 2nd", "Rosa 2nd"},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			first := domain.Candidates(tc.query)[0].Name
			if strings.Contains(tc.query, " ") {
				assert.Equal(t, tc.want, first)
				return
			}
			assert.True(t, strings.HasPrefix(first, tc.want+" "), "a word is followed by a last name: %q", first)
		})
	}

	t.Run("a lone combining mark is a word and does not panic", func(t *testing.T) {
		require.NotPanics(t, func() { domain.Candidates("́ ́") })
		assert.NotEmpty(t, domain.Candidates("́ ́")[0].Name)
	})
}

func TestCandidates_AQueryThatIsADefaultNameOffersThatPerson(t *testing.T) {
	defaults := domain.Candidates("")
	require.Len(t, defaults, 5)

	for i, full := range defaultNames {
		t.Run(full, func(t *testing.T) {
			for _, query := range []string{full, strings.ToLower(full), strings.ToUpper(full), " @" + full + " "} {
				got := domain.Candidates(query)

				require.Len(t, got, 1, query)
				assert.Equal(t, defaults[i], got[0], "the same person, with the same phone, as without a query")
			}
		})
	}

	t.Run("a part of two names offers both, in the order of the list", func(t *testing.T) {
		assert.Equal(t, []string{"Maria Torres", "Carlos Diaz", "Luis Vargas"}, namesOf(domain.Candidates("ar")), "every name with an ar")
	})
}

func TestCandidates_OfferThreeNamesOfThePersonTyped(t *testing.T) {
	t.Run("one word is offered with three last names, none twice", func(t *testing.T) {
		got := domain.Candidates("Rosa")

		require.Len(t, got, 3)
		assert.Len(t, uniqueOf(namesOf(got)), 3)
		for _, c := range got {
			assert.True(t, strings.HasPrefix(c.Name, "Rosa "), c.Name)
		}
	})

	t.Run("two words are offered as typed first, and then with a last name", func(t *testing.T) {
		got := domain.Candidates("rosa quispe")

		require.Len(t, got, 3)
		assert.Equal(t, "Rosa Quispe", got[0].Name)
		assert.Len(t, uniqueOf(namesOf(got)), 3)
	})
}

func uniqueOf(in []string) map[string]bool {
	out := map[string]bool{}
	for _, s := range in {
		out[s] = true
	}
	return out
}

// The phones are made up from a hash of the name: nine digits with the first fixed leave 10^8 numbers, so two names that
// have the same phone are rare, not impossible. Of 1,500 offers (500 queries, 3 each) about one in a hundred runs is
// expected to hold one pair; more than two would say the hash is poor.
func TestCandidates_DifferentPeopleRarelyShareAPhone(t *testing.T) {
	const queries, tolerated = 500, 2
	owner := map[string]string{}
	collisions := 0

	for i := range queries {
		for _, c := range domain.Candidates(fmt.Sprintf("Persona%d", i)) {
			if other, taken := owner[c.Phone]; taken && other != c.Name {
				collisions++
			}
			owner[c.Phone] = c.Name
		}
	}

	assert.LessOrEqual(t, collisions, tolerated, "phones shared by two different names")
}

func TestCandidates_ThePhoneOfAPersonDoesNotDependOnTheQuery(t *testing.T) {
	// Maria Torres is the same person whether the user typed "mar", "torres" or nothing, so the BFF is told one phone for her.
	want := domain.Candidates("")[1]
	require.Equal(t, "Maria Torres", want.Name)

	for _, query := range []string{"mar", "torres", "MARÍA TORRES", "@maria"} {
		assert.Equal(t, []domain.Contact{want}, domain.Candidates(query), query)
	}
}
