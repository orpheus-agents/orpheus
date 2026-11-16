package llmlegacy

import (
	"strings"
	"testing"
)

func TestProfilePresenceValidation(t *testing.T) {
	const api = "[profiles.p]\nharness='codex'\n[profiles.p.auth]\nmode='api_key'\napi_key_env='KEY'\n"
	const account = "[credential_stores.s]\nbucket='b'\n[profiles.p]\nharness='codex'\n[profiles.p.auth]\nmode='account'\naccount_id='team-main'\nstore='s'\nkey='auth'\n"
	for name, source := range map[string]string{
		"api store": api + "store=''", "api key": api + "key=''",
		"account api env": account + "api_key_env=''",
		"empty type":      strings.Replace(account, "bucket='b'", "bucket='b'\ntype=''", 1),
		"empty region":    strings.Replace(account, "bucket='b'", "bucket='b'\nregion=''", 1),
		"missing key":     strings.Replace(account, "key='auth'", "", 1),
		"unknown field":   api + "unknown=true",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadProfiles(strings.NewReader(source)); err == nil {
				t.Fatal("accepted invalid profile")
			}
		})
	}
	for _, source := range []string{api, account} {
		if _, err := ReadProfiles(strings.NewReader(source)); err != nil {
			t.Fatal(err)
		}
	}
}
