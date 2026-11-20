package domain_test

import (
	"strings"
	"testing"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"iape-ui/internal/core/domain"
)

// The agent accepts as a conversation id only "conv_v1_" and a canonical, lowercase UUID v4, 44 characters in all. The UI makes the id
// of a new chat, so it must make exactly that.
func TestNewConversationID_IsTheShapeTheAgentAccepts(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := domain.NewConversationID()

		assert.Len(t, id, 44)
		assert.True(t, strings.HasPrefix(id, "conv_v1_"))
		assert.Equal(t, strings.ToLower(id), id)
		got, err := domain.ParseConversationID(id)
		require.NoError(t, err, id)
		assert.Equal(t, id, got)
		assert.False(t, seen[id], "an id is never made twice")
		seen[id] = true
	}
}

func TestParseConversationID(t *testing.T) {
	valid := "conv_v1_8ab59029-bc3c-4e77-b49f-9fb053d90c52"

	for name, tc := range map[string]struct {
		in   string
		want bool
	}{
		"a valid id":                         {valid, true},
		"the lowest v4 uuid":                 {"conv_v1_00000000-0000-4000-8000-000000000000", true},
		"the highest v4 uuid":                {"conv_v1_ffffffff-ffff-4fff-bfff-ffffffffffff", true},
		"the variants 8, 9, a and b":         {"conv_v1_8ab59029-bc3c-4e77-9a9f-9fb053d90c52", true},
		"empty":                              {"", false},
		"only the prefix":                    {"conv_v1_", false},
		"a plain uuid":                       {"8ab59029-bc3c-4e77-b49f-9fb053d90c52", false},
		"a uuid v7, like the ids of a chat":  {"conv_v1_01a1f710-cfbf-7d12-bc64-1cae0d2c9f40", false},
		"the nil uuid":                       {"conv_v1_00000000-0000-0000-0000-000000000000", false},
		"a variant the agent does not take":  {"conv_v1_8ab59029-bc3c-4e77-c49f-9fb053d90c52", false},
		"upper case":                         {"conv_v1_8AB59029-BC3C-4E77-B49F-9FB053D90C52", false},
		"one character short":                {"conv_v1_8ab59029-bc3c-4e77-b49f-9fb053d90c5", false},
		"one character long":                 {valid + "0", false},
		"another version of the prefix":      {"conv_v2_8ab59029-bc3c-4e77-b49f-9fb053d90c52", false},
		"a space in front":                   {" " + valid, false},
		"a space behind":                     {valid + " ", false},
		"a line break behind":                {valid + "\n", false},
		"a nul inside":                       {"conv_v1_8ab59029-bc3c-4e77-b49f-9fb053d90c\x0052", false},
		"braces":                             {"conv_v1_{8ab59029-bc3c-4e77-b49f-9fb053d90c52}", false},
		"a urn":                              {"conv_v1_urn:uuid:8ab59029-bc3c-4e77-b49f-9fb053d90c52", false},
		"no hyphens":                         {"conv_v1_8ab59029bc3c4e77b49f9fb053d90c52", false},
		"a letter that is not hex":           {"conv_v1_8ab59029-bc3c-4e77-b49f-9fb053d90c5g", false},
		"letters of another script":          {"conv_v1_８ab59029-bc3c-4e77-b49f-9fb053d90c52", false},
		"129 characters":                     {strings.Repeat("a", 129), false},
		"the legacy form, the account's own": {"25906ac5-9f18-4099-b3fd-e1b74d8549a4", false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := domain.ParseConversationID(tc.in)

			if tc.want {
				require.NoError(t, err)
				assert.Equal(t, tc.in, got)
				return
			}
			require.ErrorIs(t, err, domain.ErrInvalidConversationID)
			assert.Empty(t, got, "nothing is returned for an id that was refused")
		})
	}
}

func TestParseConversationID_AUUIDOfThePackageIsAcceptedOnlyAsV4(t *testing.T) {
	assert.NoError(t, parseOnly("conv_v1_"+uuid.NewV4().String()))
	assert.ErrorIs(t, parseOnly("conv_v1_"+uuid.NewV7().String()), domain.ErrInvalidConversationID)
}

func parseOnly(s string) error { _, err := domain.ParseConversationID(s); return err }
