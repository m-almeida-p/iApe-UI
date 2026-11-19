package domain

import (
	"hash/fnv"
	"strings"
	"unicode"
)

// defaultContacts are the people the Web UI offers when it has nothing to go on. They are test data, as in the BFF's own page.
var defaultContacts = []string{"Juan Perez", "Maria Torres", "Carlos Diaz", "Ana Rojas", "Luis Vargas"}

// lastNames complete a first name that nobody in defaultContacts has.
var lastNames = []string{"Perez", "Torres", "Diaz", "Rojas", "Vargas", "Flores", "Salazar", "Castillo", "Ruiz", "Mendoza"}

// maxQuery is how much of a query is looked at.
const maxQuery = 60

// Candidates are the people a query may mean, for the user to choose from. There is no address book behind them: like the page
// of the agent's BFF, the Web UI offers test contacts, always the same for the same query. A query that is part of a default
// contact keeps those; any other is offered as the name typed, then with a few last names. Phones are made up, Peruvian mobile
// numbers (nine digits, the first a 9).
func Candidates(query string) []Contact {
	query = clean(query)
	if query == "" {
		out := make([]Contact, len(defaultContacts))
		for i, name := range defaultContacts {
			out[i] = Contact{Name: name, Phone: phoneFor(name)}
		}
		return out
	}
	needle := fold(query)
	var out []Contact
	for _, name := range defaultContacts {
		if strings.Contains(fold(name), needle) {
			out = append(out, Contact{Name: name, Phone: phoneFor(name)})
		}
	}
	if len(out) > 0 {
		return out
	}
	base := titleCase(query)
	seed := int(hash(fold(query)) % uint32(len(lastNames)))
	if strings.Contains(base, " ") {
		out = append(out, Contact{Name: base, Phone: phoneFor(base)})
	}
	for i := 0; len(out) < 3; i++ {
		name := base + " " + lastNames[(seed+i)%len(lastNames)]
		out = append(out, Contact{Name: name, Phone: phoneFor(name)})
	}
	return out
}

// clean takes the @ and the extra spaces off a query and cuts it.
func clean(q string) string {
	q = strings.Join(strings.Fields(strings.TrimPrefix(strings.TrimSpace(q), "@")), " ")
	if r := []rune(q); len(r) > maxQuery {
		q = string(r[:maxQuery])
	}
	return q
}

// accents are the letters of Spanish that carry a mark, and the plain letter each one is compared as.
var accents = strings.NewReplacer("á", "a", "é", "e", "í", "i", "ó", "o", "ú", "u", "ü", "u", "ñ", "n")

// fold is a text without capitals or the accents of Spanish, to compare.
func fold(s string) string { return accents.Replace(strings.ToLower(s)) }

func titleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	return strings.Join(words, " ")
}

func hash(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// phoneFor is a made-up mobile number that is always the same for a name.
func phoneFor(name string) string {
	n := hash(fold(name)) % 100_000_000
	digits := []byte("900000000")
	for i := 8; i >= 1; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits)
}
