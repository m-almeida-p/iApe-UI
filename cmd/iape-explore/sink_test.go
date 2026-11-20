package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDirSink_PathsAndFiles(t *testing.T) {
	dir := t.TempDir()
	s := &dirSink{dir: filepath.Join(dir, "pay")}

	assert.Equal(t, filepath.Join(dir, "pay", "03-click-pay.png"), s.Path(3, "click-pay", "png"))
	require.NoError(t, s.Write(3, "click-pay", "snapshot.txt", []byte("hello")))

	got, err := os.ReadFile(filepath.Join(dir, "pay", "03-click-pay.snapshot.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))
	info, err := os.Stat(filepath.Join(dir, "pay", "03-click-pay.snapshot.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "what a run records is for whoever runs it")
}

func TestDirSink_NeverWritesASecret(t *testing.T) {
	dir := t.TempDir()
	s := &dirSink{dir: dir, secrets: []string{"hunter2-secret", "ab"}}
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r"
	body := "password hunter2-secret and Authorization: Bearer abc.def.ghi and " + jwt + " and ab stays"

	require.NoError(t, s.Write(1, "login", "snapshot.txt", []byte(body)))

	got, err := os.ReadFile(filepath.Join(dir, "01-login.snapshot.txt"))
	require.NoError(t, err)
	assert.NotContains(t, string(got), "hunter2-secret")
	assert.NotContains(t, string(got), "abc.def.ghi")
	assert.NotContains(t, string(got), "eyJ")
	assert.Contains(t, string(got), "ab stays", "a secret too short to be one is left alone, or every word with it would go")
	assert.Equal(t, 3, s.redacted)
}

func TestDirSink_CreatesItsDirectoryPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s := &dirSink{dir: dir}

	require.NoError(t, s.Write(1, "x", "txt", []byte("1")))

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}
