package chrome_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/chrome"
)

func TestPinnedPackage(t *testing.T) {
	const pkg = "chrome-devtools-mcp@1.10.1"
	tests := []struct {
		name    string
		command []string
		want    bool
	}{
		{"npx and the pinned package", []string{"npx", pkg}, true},
		{"npx with -y", []string{"npx", "-y", pkg}, true},
		{"npx.cmd, which is how Windows has it", []string{"npx.cmd", "-y", pkg}, true},
		{"a path to npx", []string{"/usr/local/bin/npx", "-y", pkg}, true},
		{"a path to npx.cmd", []string{`C:/Program Files/nodejs/npx.cmd`, pkg}, true},
		{"a relative path to npx", []string{"./node_modules/.bin/npx", pkg}, true},
		{"every flag npx may be given", []string{"npx", "-y", "--yes", "--no-install", "--prefer-offline", "-q", "--quiet", pkg}, true},
		{"the same flag twice", []string{"npx", "-y", "-y", pkg}, true},
		{"-y and --yes", []string{"npx", "-y", "--yes", pkg}, true},
		{"nothing may follow the package: it would be an argument of the server, past the check of the arguments", []string{"npx", "-y", pkg, "extra"}, false},
		{"not even a privacy flag turned off", []string{"npx", "-y", pkg, "--usage-statistics"}, false},
		{"a pre-release", []string{"npx", "chrome-devtools-mcp@1.10.1-beta.2"}, true},
		{"build metadata", []string{"npx", "chrome-devtools-mcp@1.10.1+build.5"}, true},
		{"a pre-release with hyphens", []string{"npx", "chrome-devtools-mcp@1.10.1-rc-1"}, true},

		{"nothing", nil, false},
		{"an empty command", []string{}, false},
		{"npx alone", []string{"npx"}, false},
		{"npx with a flag and no package", []string{"npx", "-y"}, false},
		{"an empty program", []string{"", pkg}, false},
		{"another program", []string{"node", pkg}, false},
		{"another program named like npx", []string{"npx2", pkg}, false},
		{"npx in upper case", []string{"NPX", pkg}, false},
		{"npx.exe", []string{"npx.exe", pkg}, false},
		{"npx.cmd.sh", []string{"npx.cmd.sh", pkg}, false},
		{"the package from another package manager's command", []string{"pnpm", "dlx", pkg}, false},
		{"a package that is first in the command and not pinned", []string{"npx", "-y", "evil", pkg}, false},
		{"a flag that names another package", []string{"npx", "--package=evil", pkg}, false},
		{"-p and its package", []string{"npx", "-p", "evil", pkg}, false},
		{"a flag that is not on the list", []string{"npx", "--yes=false", pkg}, false},
		{"a flag that only starts like one on the list", []string{"npx", "-yes", pkg}, false},
		{"a lone dash", []string{"npx", "-", pkg}, false},
		{"the flag after the package", []string{"npx", "evil", "-y", pkg}, false},
		{"a version with two parts", []string{"npx", "chrome-devtools-mcp@1.10"}, false},
		{"a version with one part", []string{"npx", "chrome-devtools-mcp@1"}, false},
		{"latest", []string{"npx", "chrome-devtools-mcp@latest"}, false},
		{"a tag", []string{"npx", "chrome-devtools-mcp@next"}, false},
		{"no version", []string{"npx", "chrome-devtools-mcp"}, false},
		{"an empty version", []string{"npx", "chrome-devtools-mcp@"}, false},
		{"a range", []string{"npx", "chrome-devtools-mcp@>=1.10.1"}, false},
		{"a caret", []string{"npx", "chrome-devtools-mcp@^1.10.1"}, false},
		{"a pre-release that is only a dash", []string{"npx", "chrome-devtools-mcp@1.10.1-"}, false},
		{"a scoped package of the same name", []string{"npx", "@scope/chrome-devtools-mcp@1.0.0"}, false},
		{"another package that ends the same", []string{"npx", "evil-chrome-devtools-mcp@1.10.1"}, false},
		{"a shell separator after the version", []string{"npx", "chrome-devtools-mcp@1.10.1;rm"}, false},
		{"a shell command substitution in the version", []string{"npx", "chrome-devtools-mcp@1.10.1$(id)"}, false},
		{"a newline after the version", []string{"npx", "chrome-devtools-mcp@1.10.1\n"}, false},
		{"a space after the version", []string{"npx", "chrome-devtools-mcp@1.10.1 evil"}, false},
		{"a url in place of the package", []string{"npx", "https://evil.example/chrome-devtools-mcp@1.10.1"}, false},
		{"a path in place of the package", []string{"npx", "./chrome-devtools-mcp@1.10.1"}, false},
		{"npm: alias", []string{"npx", "chrome-devtools-mcp@npm:evil@1.10.1"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, chrome.PinnedPackage(tc.command))
		})
	}
}

func TestCheckMCP_Flags(t *testing.T) {
	command := []string{"npx", "-y", "chrome-devtools-mcp@1.10.1"}
	privacy := []string{"--isolated", "--no-usage-statistics", "--no-performance-crux"}
	with := func(args ...string) []string { return append(append([]string{}, privacy...), args...) }

	tests := []struct {
		name    string
		args    []string
		headed  bool
		wantErr string // empty means the arguments are fine
	}{
		{"headless with every flag", with("--headless"), false, ""},
		{"headless with every flag, twice", with("--headless", "--headless", "--isolated"), false, ""},
		{"headed with no --headless", with(), true, ""},
		{"headed with --headless as well", with("--headless"), true, ""},
		{"headed may say it is not headless", with("--headless=false"), true, ""},
		{"headed may say --no-headless", with("--no-headless"), true, ""},
		{"empty arguments, headless", nil, false, "missing: --headless --isolated --no-usage-statistics --no-performance-crux"},
		{"empty arguments, headed", nil, true, "missing: --isolated --no-usage-statistics --no-performance-crux"},
		{"an empty list of arguments is not nil", []string{}, true, "missing: --isolated --no-usage-statistics --no-performance-crux"},
		{"a flag with a value does not make the flag", []string{"--headless", "--isolated=false", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --isolated"},
		{"--isolated=true is another spelling, not the flag", []string{"--headless", "--isolated=true", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --isolated"},
		{"--headless=false does not make --headless", []string{"--headless=false", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --headless"},
		{"--headless=true does not either", []string{"--headless=true", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --headless"},
		{"upper case is not the flag", []string{"--HEADLESS", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --headless"},
		{"a flag with one dash", []string{"-headless", "--isolated", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --headless"},
		{"a flag kept in the value of another", []string{"--user-data-dir=--isolated", "--headless", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --isolated"},
		{"a flag with a space inside the argument", []string{"--headless --isolated", "--no-usage-statistics", "--no-performance-crux"}, false, "missing: --headless --isolated"},

		{"--isolated said again as false", with("--headless", "--isolated=false"), false, "--isolated=false"},
		{"--isolated negated", with("--headless", "--no-isolated"), false, "--no-isolated"},
		{"--isolated followed by false", with("--headless", "false"), false, "false"},
		{"usage statistics turned back on", with("--headless", "--usage-statistics"), false, "--usage-statistics"},
		{"usage statistics turned back on, in camel case", with("--headless", "--usageStatistics"), false, "--usageStatistics"},
		{"usage statistics turned back on with a value", with("--headless", "--usage-statistics=true"), false, "--usage-statistics=true"},
		{"CrUX turned back on", with("--headless", "--performance-crux"), false, "--performance-crux"},
		{"CrUX turned back on, in camel case", with("--headless", "--performanceCrux=true"), false, "--performanceCrux=true"},
		{"--headless said again as false", with("--headless", "--headless=false"), false, "--headless=false"},
		{"--headless negated", with("--headless", "--no-headless"), false, "--no-headless"},
		{"the privacy flag in another case, kept beside the real one", with("--headless", "--ISOLATED=false"), false, "--ISOLATED=false"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := chrome.CheckMCP(command, tc.args, tc.headed)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			assert.NotContains(t, err.Error(), "must pin a version", "the command was fine")
		})
	}
}

func TestCheckMCP_SaysBothProblems(t *testing.T) {
	err := chrome.CheckMCP([]string{"npx", "-y", "evil", "chrome-devtools-mcp@1.10.1"}, nil, true)

	require.Error(t, err)
	assert.ErrorContains(t, err, "must pin a version")
	assert.ErrorContains(t, err, "missing: --isolated --no-usage-statistics --no-performance-crux")
	assert.Equal(t, 2, strings.Count(err.Error(), "\n")+1, "one line for each problem")
}
