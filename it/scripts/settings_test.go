package scripts_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every setting has one home: .env, written down with its default in
// .env.example. The Go programs, the scripts, Docker Compose and the tests
// take theirs from there, so nobody has to know which variable to type before a
// command. These tests keep it that way: a setting that is read somewhere and
// not written down in the example fails here.

// documentedSettings is every KEY the example has, commented out or not.
func documentedSettings(t *testing.T) map[string]bool {
	t.Helper()
	example, err := os.ReadFile("../../.env.example")
	require.NoError(t, err)
	keys := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^(?:# )?([A-Z][A-Z0-9_]*)=`).FindAllStringSubmatch(string(example), -1) {
		keys[m[1]] = true
	}
	require.Greater(t, len(keys), 40, "the example was read")
	return keys
}

// scriptFiles are the shell scripts of the repository.
func scriptFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"../../scripts/*.sh", "../../scripts/lib/*.sh"} {
		matches, err := filepath.Glob(pattern)
		require.NoError(t, err)
		files = append(files, matches...)
	}
	require.NotEmpty(t, files)
	return files
}

func TestSettings_ScriptsTakeThemFromTheSettingsFiles(t *testing.T) {
	documented := documentedSettings(t)

	t.Run("each one they ask for is written down", func(t *testing.T) {
		asked := regexp.MustCompile(`\$\((?:required_)?setting ([A-Z][A-Z0-9_]*)\)`)
		count := 0
		for _, file := range scriptFiles(t) {
			src, err := os.ReadFile(file)
			require.NoError(t, err)
			for _, m := range asked.FindAllStringSubmatch(string(src), -1) {
				count++
				assert.True(t, documented[m[1]], "%s asks for %s, which .env.example does not document", filepath.Base(file), m[1])
			}
		}
		assert.Positive(t, count, "the scripts use setting at all")
	})

	t.Run("none is read from the environment with a default of its own", func(t *testing.T) {
		// ${NAME:-default} is how a script reads a variable somebody exported. Only the
		// names that belong to the helpers themselves, and the one a test swaps for a
		// shim, may do it.
		inline := regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*):-`)
		allowed := []string{"ENV_FILE", "ENV_EXAMPLE", "LSOF"}
		for _, file := range scriptFiles(t) {
			src, err := os.ReadFile(file)
			require.NoError(t, err)
			for _, m := range inline.FindAllStringSubmatch(string(src), -1) {
				assert.Contains(t, allowed, m[1],
					"%s reads %s from the environment with a default: put it in .env.example and ask for it with $(setting %s)", filepath.Base(file), m[1], m[1])
			}
		}
	})
}

func TestSettings_ComposeVariablesAreWrittenDown(t *testing.T) {
	documented := documentedSettings(t)
	src, err := os.ReadFile("../../docker-compose.yml")
	require.NoError(t, err)

	used := regexp.MustCompile(`\$\{([A-Z][A-Z0-9_]*)`).FindAllStringSubmatch(string(src), -1)

	require.NotEmpty(t, used)
	for _, m := range used {
		assert.True(t, documented[m[1]], "docker-compose.yml uses %s, which .env.example does not document", m[1])
	}
}

// The Go code reads its settings through the loaders, which have tests of their
// own. This catches a read of a variable that goes around them.
func TestSettings_GoCodeReadsOnlyWrittenDownVariables(t *testing.T) {
	documented := documentedSettings(t)
	// Not settings: the shell's own, and what a test's fake program is told.
	notSettings := []string{"PATH", "FAKE_BIND"}
	read := regexp.MustCompile(`(?:os\.Getenv|os\.LookupEnv|getenv)\("([A-Z][A-Z0-9_]*)"\)`)

	found := 0
	for _, root := range []string{"../../cmd", "../../internal", "../../it", "../../assets"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range read.FindAllStringSubmatch(string(src), -1) {
				found++
				if slices.Contains(notSettings, m[1]) {
					continue
				}
				assert.True(t, documented[m[1]], "%s reads %s, which .env.example does not document", filepath.ToSlash(path), m[1])
			}
			return nil
		})
		require.NoError(t, err)
	}
	assert.Positive(t, found, "the scan found the reads it knows of")
}
