package llmlegacy

import (
	"strings"
	"testing"
)

const catalogProfile = "[profiles.default]\nharness='codex'\ndescription='Research profile'\nmodel='model'\ninstructions='Profile instructions'\n[profiles.default.auth]\nmode='api_key'\napi_key_env='KEY'\n"

func TestProfilesWithoutTemplateCatalog(t *testing.T) {
	for _, description := range []string{"", "description = ''", "description = 'New description'"} {
		source := strings.Replace(catalogProfile, "description='Research profile'", description, 1)
		if _, err := ReadProfiles(strings.NewReader(source)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ReadProfiles(strings.NewReader(strings.Replace(catalogProfile, "description='Research profile'", "description = 42", 1))); err == nil {
		t.Fatal("accepted invalid profile description")
	}
	// The existing strict decoder rejects the removed configuration field.
	if _, err := ReadProfiles(strings.NewReader("[templates.codex]\n" + catalogProfile)); err == nil {
		t.Fatal("accepted obsolete template catalog")
	}
}
