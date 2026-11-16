package llmlegacy

import (
	"fmt"
	"strings"
	"testing"
)

func TestAccountCatalogAndImmutableCredentials(t *testing.T) {
	const source = `[credential_stores.one]
bucket="b"
endpoint_url="HTTPS://S3.EXAMPLE:443/path"
[credential_stores.two]
bucket="b"
endpoint_url="https://s3.example/path"
[profiles.deep]
harness="codex"
model="m"
[profiles.deep.auth]
mode="account"
account_id="team-main"
store="one"
key="auth.json"
[profiles.fast]
harness="codex"
model="m"
[profiles.fast.auth]
mode="account"
account_id="team-main"
store="two"
key="auth.json"
`
	p, err := ReadProfiles(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if p.Profiles["fast"].Auth.AccountID != "team-main" || p.Profiles["deep"].Auth.AccountID != "team-main" {
		t.Fatal("legacy identity changed")
	}
	for name, changed := range map[string]string{
		"source changed": strings.Replace(source, "key=\"auth.json\"\n[profiles.fast]", "key=\"other.json\"\n[profiles.fast]", 1),
		"ID changed":     strings.Replace(source, "account_id=\"team-main\"\nstore=\"two\"", "account_id=\"other\"\nstore=\"two\"", 1),
		"missing ID":     strings.Replace(source, "account_id=\"team-main\"\nstore=\"one\"", "store=\"one\"", 1),
		"invalid ID":     strings.Replace(source, "account_id=\"team-main\"", "account_id=\"bad id\"", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadProfiles(strings.NewReader(changed)); err == nil {
				t.Fatal("invalid account accepted")
			}
		})
	}
	if _, err := ReadProfiles(strings.NewReader(strings.Replace(source, "account_id=\"team-main\"", "account_id=\""+strings.Repeat("a", 129)+"\"", 1))); err == nil {
		t.Fatal("long ID accepted")
	}
}

func TestAccountIDsCannotBeUsedByAPIKeys(t *testing.T) {
	source := `[profiles.p]
harness="codex"
[profiles.p.auth]
mode="api_key"
api_key_env="OPENAI_API_KEY"
account_id="team-main"
`
	if _, err := ReadProfiles(strings.NewReader(source)); err == nil {
		t.Fatal("API key account_id accepted")
	}
}

func TestAccountCatalogCap(t *testing.T) {
	var source strings.Builder
	source.WriteString("[credential_stores.s]\nbucket='b'\n")
	for i := range 1001 {
		fmt.Fprintf(&source, "[profiles.p%d]\nharness='codex'\n[profiles.p%d.auth]\nmode='account'\naccount_id='a%d'\nstore='s'\nkey='k%d'\n", i, i, i, i)
	}
	if _, err := ReadProfiles(strings.NewReader(source.String())); err == nil {
		t.Fatal("unbounded account catalogue accepted")
	}
}
