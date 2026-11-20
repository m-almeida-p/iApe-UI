package postgres

import (
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"iape-ui/internal/platform/tmplcache"
)

// sqlFiles holds the SQL of the stores: a template for each statement, in a
// folder for each store, and the column list that the statements of a store
// share. A statement includes its column list by the path of the file, with a
// trim marker ({{template "..." -}}) when a clause follows on the next line,
// so that including the list leaves no blank line before it; a statement that
// ends with the list, such as an INSERT's RETURNING, needs no trim.
//
//go:embed sql
var sqlFiles embed.FS

// Statements is the SQL that the stores send, filled in and ready for the
// driver. It comes from LoadStatements; the zero value holds no SQL.
type Statements struct {
	user    userSQL
	session sessionSQL
}

type userSQL struct {
	create, byEmail, byID, count string
}

type sessionSQL struct {
	create, get, remove, removeExpired string
}

// templates says which file fills which statement.
func (s *Statements) templates() map[string]*string {
	return map[string]*string{
		"sql/user/create.gotmpl":   &s.user.create,
		"sql/user/by_email.gotmpl": &s.user.byEmail,
		"sql/user/by_id.gotmpl":    &s.user.byID,
		"sql/user/count.gotmpl":    &s.user.count,

		"sql/session/create.gotmpl":         &s.session.create,
		"sql/session/get.gotmpl":            &s.session.get,
		"sql/session/delete.gotmpl":         &s.session.remove,
		"sql/session/delete_expired.gotmpl": &s.session.removeExpired,
	}
}

// LoadStatements reads the embedded templates into a cache and fills in every
// statement from it. A program calls it once, when it starts: a statement that
// is missing or does not parse stops the start, and no request finds out. No
// statement takes data today, so the stores keep the texts and not the cache.
func LoadStatements() (Statements, error) { return loadStatements(sqlFiles) }

func loadStatements(files fs.FS) (Statements, error) {
	cache, err := tmplcache.Preload(files, nil)
	if err != nil {
		return Statements{}, fmt.Errorf("reading the SQL of the stores: %w", err)
	}
	var st Statements
	templates := st.templates()
	for _, path := range slices.Sorted(maps.Keys(templates)) {
		text, err := cache.Execute(path, nil)
		if err != nil {
			return Statements{}, fmt.Errorf("reading the SQL of the stores: %w", err)
		}
		*templates[path] = strings.TrimSpace(text)
	}
	return st, nil
}
