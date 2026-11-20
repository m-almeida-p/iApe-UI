package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirSink_PathHasThreeDigitsFromStepOneHundred(t *testing.T) {
	s := &dirSink{dir: filepath.Join("a", "b")}

	assert.Equal(t, filepath.Join("a", "b", "05-x.png"), s.Path(5, "x", "png"))
	assert.Equal(t, filepath.Join("a", "b", "99-x.png"), s.Path(99, "x", "png"))
	assert.Equal(t, filepath.Join("a", "b", "100-x.png"), s.Path(100, "x", "png"))
	assert.Equal(t, filepath.Join("a", "b", "1234-x.png"), s.Path(1234, "x", "png"))
}

func TestDirSink_APathAlwaysStaysInsideItsDirectory(t *testing.T) {
	hostile := []string{
		"../x", "../../x", "/../../x", "/../../../../../../etc/passwd", "..", "../", "a/../../b", "/abs", "a/b", `..\x`, `\..\..\x`, "a\x00b",
		"/", "//", "./", "...", "../..", "%2e%2e/x", "..//..//x",
	}
	root := t.TempDir()
	dir := filepath.Join(root, "inside", "run")
	s := &dirSink{dir: dir}

	for _, name := range hostile {
		for _, ext := range []string{"png", "snapshot.txt", "../evil", "/../../evil", "..", ""} {
			path := s.Path(1, name, ext)

			rel, err := filepath.Rel(dir, path)
			require.NoError(t, err)
			assert.False(t, strings.HasPrefix(rel, ".."), "name %q ext %q gave %s, outside %s", name, ext, path, dir)
			assert.Equal(t, dir, filepath.Dir(path), "name %q ext %q: the file is directly in the directory", name, ext)
		}
	}
}

func TestDirSink_WriteWithHostileNamesWritesInsideTheDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "inside", "run")
	s := &dirSink{dir: dir}

	for _, name := range []string{"/../../escaped", "../escaped", "../../escaped", "a/b/c"} {
		require.NoError(t, s.Write(1, name, "txt", []byte("data")), name)
	}

	// nothing outside of dir, except the directories that lead to it
	var outside []string
	require.NoError(t, filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || strings.HasPrefix(path, dir+string(filepath.Separator)) {
			return nil
		}
		outside = append(outside, path)
		return nil
	}))
	assert.Empty(t, outside, "files outside the directory of the run")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 4, "each name made a file of its own, here")
	for _, e := range entries {
		assert.False(t, e.IsDir(), e.Name())
	}
}

func TestDirSink_APathAndAWriteAgreeEvenForHostileNames(t *testing.T) {
	dir := t.TempDir()
	s := &dirSink{dir: dir}

	require.NoError(t, s.Write(2, "../up/and/away", "txt", []byte("data")))

	got, err := os.ReadFile(s.Path(2, "../up/and/away", "txt"))
	require.NoError(t, err, "the run writes a screenshot where Path says and reads it back where it wrote it")
	assert.Equal(t, "data", string(got))
}

func TestDirSink_WriteWhenTheDirectoryCannotBeMade(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))

	for name, dir := range map[string]string{"a file is the directory": file, "a file on the way": filepath.Join(file, "x", "y")} {
		t.Run(name, func(t *testing.T) {
			s := &dirSink{dir: dir}

			err := s.Write(1, "x", "txt", []byte("data"))

			require.Error(t, err, "the run is told the record was not written")
			assert.NoFileExists(t, filepath.Join(dir, "01-x.txt"))
		})
	}
}

func TestDirSink_WriteOverAnExistingFileReplacesIt(t *testing.T) {
	dir := t.TempDir()
	s := &dirSink{dir: dir}
	require.NoError(t, s.Write(1, "x", "txt", []byte("first, and long")))

	require.NoError(t, s.Write(1, "x", "txt", []byte("second")))

	got, err := os.ReadFile(filepath.Join(dir, "01-x.txt"))
	require.NoError(t, err)
	assert.Equal(t, "second", string(got), "nothing of the first remains")
}

func TestDirSink_WriteAnEmptyFile(t *testing.T) {
	s := &dirSink{dir: t.TempDir()}

	require.NoError(t, s.Write(1, "x", "txt", nil))

	got, err := os.ReadFile(s.Path(1, "x", "txt"))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestDirSink_Clean(t *testing.T) {
	tests := map[string]struct {
		secrets []string
		text    string
		want    string
		count   int
	}{
		"no secrets, no change":             {nil, "plain text", "plain text", 0},
		"an empty secret is none":           {[]string{""}, "plain text", "plain text", 0},
		"a secret that is not there":        {[]string{"hunter2"}, "plain text", "plain text", 0},
		"the secret is all the text":        {[]string{"hunter2"}, "hunter2", "[redacted]", 1},
		"a secret twice":                    {[]string{"hunter2"}, "hunter2 and hunter2", "[redacted] and [redacted]", 2},
		"on many lines":                     {[]string{"hunter2"}, "a hunter2\nb\nhunter2 c\n", "a [redacted]\nb\n[redacted] c\n", 2},
		"the same secret twice in the list": {[]string{"hunter2", "hunter2"}, "x hunter2 y", "x [redacted] y", 1},
		"touching secrets":                  {[]string{"abcd", "efgh"}, "abcdefgh", "[redacted]", 1},
		"3 characters are left alone":       {[]string{"abc"}, "abc abc", "abc abc", 0},
		"4 characters are taken out":        {[]string{"abcd"}, "abcd abc", "[redacted] abc", 1},
		"regex characters are literal":      {[]string{"a.b*c(d[e"}, "x a.b*c(d[e y aXb", "x [redacted] y aXb", 1},
		"a regex that would match all":      {[]string{".*.*"}, "plain text .*.*", "plain text [redacted]", 1},
		"a backslash and a dollar":          {[]string{`p\w$1d`}, `a p\w$1d b`, "a [redacted] b", 1},
		"a replacement mark in the text":    {[]string{"hunter2"}, "$1 ${x} hunter2", "$1 ${x} [redacted]", 1},
		"case matters for a secret":         {[]string{"Hunter2"}, "hunter2 Hunter2", "hunter2 [redacted]", 1},
		"unicode":                           {[]string{"contraseña-ñ"}, "x contraseña-ñ y", "x [redacted] y", 1},
		"a secret with a space":             {[]string{"two words"}, "say two words now", "say [redacted] now", 1},
		"a secret that is a repeated char":  {[]string{"aaaa"}, "aaaaaaa", "[redacted]", 1},
		"a short secret inside a long word": {[]string{"abc"}, "abcdefgh", "abcdefgh", 0},
		"a secret next to a bracket":        {[]string{"hunter2"}, `textbox "hunter2"`, `textbox "[redacted]"`, 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &dirSink{secrets: tc.secrets}

			got := s.clean(tc.text)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.count, s.redacted)
		})
	}
}

func TestDirSink_CleanLeavesNoPartOfASecretThatContainsAnother(t *testing.T) {
	tests := map[string]struct {
		secrets []string
		text    string
	}{
		"the short one first":          {[]string{"hunter2", "hunter2-secret"}, "pw=hunter2-secret;"},
		"the long one first":           {[]string{"hunter2-secret", "hunter2"}, "pw=hunter2-secret;"},
		"a secret inside the middle":   {[]string{"ter2-se", "hunter2-secret"}, "pw=hunter2-secret;"},
		"a secret at the end":          {[]string{"secret", "hunter2-secret"}, "pw=hunter2-secret;"},
		"overlapping, not nested":      {[]string{"abcdef", "defghi"}, "pw=abcdefghi;"},
		"overlapping, the other order": {[]string{"defghi", "abcdef"}, "pw=abcdefghi;"},
		"a chain of three":             {[]string{"abcdef", "defghi", "ghijkl"}, "pw=abcdefghijkl;"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &dirSink{secrets: tc.secrets}

			got := s.clean(tc.text)

			assert.Equal(t, "pw=[redacted];", got, "not one character of a secret is left behind")
		})
	}
}

func TestDirSink_CleanIsTheSameWhateverTheOrderOfTheSecrets(t *testing.T) {
	secrets := []string{"hunter2", "hunter2-secret", "secret-x", "cret"}
	text := "a hunter2-secret-x b hunter2 c secret-x d"
	want := (&dirSink{secrets: secrets}).clean(text)

	for _, perm := range [][]int{{3, 2, 1, 0}, {1, 0, 3, 2}, {2, 3, 0, 1}} {
		var shuffled []string
		for _, i := range perm {
			shuffled = append(shuffled, secrets[i])
		}

		assert.Equal(t, want, (&dirSink{secrets: shuffled}).clean(text))
	}
	assert.NotContains(t, want, "cret")
}

func TestDirSink_CleanDoesNotChangeTheListOfSecrets(t *testing.T) {
	secrets := []string{"abcd", "abcdefgh", "wxyz"}
	s := &dirSink{secrets: secrets}

	s.clean("abcdefgh")

	assert.Equal(t, []string{"abcd", "abcdefgh", "wxyz"}, secrets)
	assert.Equal(t, []string{"abcd", "abcdefgh", "wxyz"}, s.secrets)
}

func TestDirSink_CleanTokens(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r"
	tests := map[string]struct {
		text  string
		want  string
		count int
	}{
		"Bearer":                  {"Authorization: Bearer abc.def", "Authorization: [redacted]", 1},
		"bearer in lower case":    {"authorization: bearer abc.def", "authorization: [redacted]", 1},
		"BEARER in upper case":    {"AUTHORIZATION: BEARER abc.def", "AUTHORIZATION: [redacted]", 1},
		"bEaReR mixed":            {"bEaReR abc.def", "[redacted]", 1},
		"Bearer with a tab":       {"Bearer\tabc.def", "[redacted]", 1},
		"Bearer with many spaces": {"Bearer     abc.def", "[redacted]", 1},
		"Bearer with a newline":   {"Bearer\nabc.def", "[redacted]", 1},
		"two of them":             {"Bearer a1b2 and Bearer c3d4", "[redacted] and [redacted]", 2},
		"Bearer with no token":    {"Bearer ", "Bearer ", 0},
		"a word ending in bearer": {"pallbearer abc", "pallbearer abc", 0},
		"a JWT alone":             {jwt, "[redacted]", 1},
		"a JWT in a sentence":     {"token=" + jwt + "; done", "token=[redacted]; done", 1},
		"two JWTs":                {jwt + " " + jwt, "[redacted] [redacted]", 2},
		"a JWT with no signature": {"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.", "[redacted]", 1},
		"eyJ that is too short":   {"eyJabc.def", "eyJabc.def", 0},
		"a Bearer JWT is one":     {"Bearer " + jwt, "[redacted]", 1},
		"a JWT in a URL":          {"https://x/cb?token=" + jwt + "&a=1", "https://x/cb?token=[redacted]&a=1", 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			s := &dirSink{}

			got := s.clean(tc.text)

			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.count, s.redacted)
		})
	}
	t.Run("the count adds up across calls", func(t *testing.T) {
		s := &dirSink{secrets: []string{"hunter2"}}

		s.clean("hunter2")
		s.clean("Bearer abc.def hunter2")

		assert.Equal(t, 3, s.redacted)
	})
	t.Run("a secret that is a token is redacted once, not twice", func(t *testing.T) {
		s := &dirSink{secrets: []string{"abc.def"}}

		got := s.clean("Bearer abc.def")

		assert.NotContains(t, got, "abc")
		assert.Equal(t, 1, s.redacted)
	})
}

func TestDirSink_CleanHugeText(t *testing.T) {
	s := &dirSink{secrets: []string{"hunter2"}}
	text := strings.Repeat("some page text hunter2 ", 100_000)

	got := s.clean(text)

	assert.NotContains(t, got, "hunter2")
	assert.Equal(t, 100_000, s.redacted)
}
