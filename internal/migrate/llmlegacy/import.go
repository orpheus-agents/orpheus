package llmlegacy

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"slices"

	"github.com/google/uuid"
	"github.com/orpheus-agents/orpheus/internal/config"
	"github.com/orpheus-agents/orpheus/internal/llm"
	"github.com/orpheus-agents/orpheus/internal/secret"
	"github.com/orpheus-agents/orpheus/internal/session"
)

//go:embed inspect.sql
var inspectSQL string

type oldCredentials struct {
	Mode      string                   `json:"mode"`
	AccountID string                   `json:"account_id"`
	APIKeyEnv string                   `json:"api_key_env"`
	Store     *session.CredentialStore `json:"store"`
	Key       string                   `json:"key"`
}
type oldSession struct {
	id          uuid.UUID
	closed      bool
	harness     string
	credentials oldCredentials
	source      string
}
type importedAccount struct {
	id          uuid.UUID
	name        string
	harness     string
	active      bool
	credentials oldCredentials
	storeName   string
}
type importer struct {
	old          Profiles
	accounts     map[string]*importedAccount
	environments map[string]string
	observations map[string]observationOwner
	profiles     map[string]llm.ProfileInput
}
type observationOwner struct {
	oldID, fingerprint string
	id                 uuid.UUID
}

// Up is called only when Goose applies version 31. No input is read at registration.
func Up(ctx context.Context, tx *sql.Tx) error {
	var occupied bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM llm_accounts) OR EXISTS(SELECT 1 FROM model_profiles)`).Scan(&occupied); err != nil {
		return err
	}
	if occupied {
		return errors.New("LLM import requires an empty target catalog")
	}
	path := os.Getenv("ORPHEUS_MIGRATION_CONFIG_FILE")
	if path == "" {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sessions) OR EXISTS(SELECT 1 FROM account_limit_observations)`).Scan(&occupied); err != nil {
			return err
		}
		if occupied {
			return errors.New("ORPHEUS_MIGRATION_CONFIG_FILE is required for legacy data")
		}
		return nil
	}
	old, err := LoadProfiles(path)
	if err != nil {
		return errors.New("invalid legacy migration configuration")
	}
	in := importer{old: old, accounts: map[string]*importedAccount{}, environments: map[string]string{}, observations: map[string]observationOwner{}, profiles: map[string]llm.ProfileInput{}}
	for _, name := range slices.Sorted(maps.Keys(old.Profiles)) {
		profile := old.Profiles[name]
		creds := oldCredentials{Mode: profile.Auth.Mode, AccountID: profile.Auth.AccountID, APIKeyEnv: profile.Auth.APIKeyEnv, Key: profile.Auth.Key}
		if creds.Mode == "account" {
			creds.Store = new(old.CredentialStores[profile.Auth.Store])
		}
		account, err := in.addAccount(profile.Harness, creds, true)
		if err != nil {
			return err
		}
		if profile.Model == nil {
			return fmt.Errorf("profile %q requires a model", name)
		}
		options := map[string]string{}
		for key, value := range map[string]string{"effort": profile.Codex.Effort, "summary": profile.Codex.Summary, "personality": profile.Codex.Personality, "service_tier": profile.Codex.ServiceTier} {
			if value != "" {
				options[key] = value
			}
		}
		input, err := llm.NormalizeProfile(llm.ProfileInput{Name: name, Description: profile.Description, AccountID: account.id, Model: *profile.Model, Instructions: profile.Instructions, Codex: options, Active: new(true)})
		if err != nil {
			return fmt.Errorf("profile %q violates the new catalog contract", name)
		}
		in.profiles[name] = input
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT configuration #>> '{credentials,api_key_env}' FROM sessions WHERE configuration #>> '{credentials,mode}'='api_key'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name sql.NullString
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		if name.Valid && envName.MatchString(name.String) {
			if value := os.Getenv(name.String); value != "" {
				in.environments[name.String] = value
			}
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	stores, err := json.Marshal(old.CredentialStores)
	if err != nil {
		return err
	}
	available := slices.Sorted(maps.Keys(in.environments))
	if available == nil {
		available = []string{}
	}
	rows, err = tx.QueryContext(ctx, inspectSQL, string(stores), available)
	if err != nil {
		return err
	}
	var sessions []oldSession
	for rows.Next() {
		var item oldSession
		var raw []byte
		var problems string
		if err := rows.Scan(&item.id, &raw, &item.closed, &problems); err != nil {
			_ = rows.Close()
			return err
		}
		if problems != "" {
			_ = rows.Close()
			return fmt.Errorf("session %s blocks LLM import: %s", item.id, problems)
		}
		var snapshot struct {
			Harness     string         `json:"harness"`
			Credentials oldCredentials `json:"credentials"`
		}
		decodeErr := json.Unmarshal(raw, &snapshot)
		if decodeErr == nil {
			item.harness = snapshot.Harness
			item.credentials = snapshot.Credentials
			item.source, decodeErr = sourceKey(item.harness, item.credentials)
		}
		if decodeErr != nil && !item.closed {
			_ = rows.Close()
			return fmt.Errorf("session %s has invalid legacy credentials", item.id)
		}
		sessions = append(sessions, item)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return err
	}
	for _, item := range sessions {
		if !item.closed {
			if _, err := in.addAccount(item.harness, item.credentials, false); err != nil {
				return fmt.Errorf("session %s: %w", item.id, err)
			}
		}
	}
	for _, item := range sessions {
		if account := in.accounts[item.source]; account != nil {
			in.rememberObservation(account, item.credentials)
		}
	}
	if err := in.writeCatalog(ctx, tx); err != nil {
		return err
	}
	for _, item := range sessions {
		var id *uuid.UUID
		if account := in.accounts[item.source]; account != nil {
			id = &account.id
		}
		if id == nil && !item.closed {
			return fmt.Errorf("session %s has no imported account", item.id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET llm_account_id=$2,configuration=configuration-'credentials' WHERE id=$1`, item.id, id); err != nil {
			return err
		}
	}
	for _, key := range slices.Sorted(maps.Keys(in.observations)) {
		owner := in.observations[key]
		if _, err := tx.ExecContext(ctx, `UPDATE account_limit_observations SET account_id=$3 WHERE account_id=$1 AND source_fingerprint=$2`, owner.oldID, owner.fingerprint, owner.id.String()); err != nil {
			return errors.New("account observation mapping conflicts; import rolled back")
		}
	}
	return nil
}

func sourceKey(harness string, creds oldCredentials) (string, error) {
	if harness != "codex" {
		return "", errors.New("unsupported legacy harness")
	}
	if creds.Mode == "api_key" && envName.MatchString(creds.APIKeyEnv) {
		return harness + "\x00api_key\x00" + creds.APIKeyEnv, nil
	}
	if creds.Mode != "account" || creds.Key == "" || creds.Store == nil {
		return "", errors.New("invalid legacy source")
	}
	return config.SourceFingerprint(harness, session.Credentials{Mode: creds.Mode, Store: creds.Store, Key: creds.Key})
}
func (in *importer) addAccount(harness string, creds oldCredentials, active bool) (*importedAccount, error) {
	source, err := sourceKey(harness, creds)
	if err != nil {
		return nil, err
	}
	if account := in.accounts[source]; account != nil {
		account.active = account.active || active
		in.rememberObservation(account, creds)
		return account, nil
	}
	account := &importedAccount{id: uuid.New(), name: creds.AccountID, harness: harness, active: active, credentials: creds}
	if creds.Mode == "account" {
		matches := 0
		for name, store := range in.old.CredentialStores {
			if reflect.DeepEqual(store, *creds.Store) {
				matches++
				account.storeName = name
			}
		}
		if matches != 1 {
			return nil, errors.New("legacy credential store must match exactly one configured store")
		}
	} else {
		account.name = creds.APIKeyEnv
		value := os.Getenv(creds.APIKeyEnv)
		if !llm.ValidKey(value) {
			return nil, errors.New("legacy API key environment is unavailable")
		}
		in.environments[creds.APIKeyEnv] = value
	}
	if account.name == "" {
		account.name = "Imported account " + account.id.String()
	}
	in.accounts[source] = account
	in.rememberObservation(account, creds)
	return account, nil
}
func (in *importer) rememberObservation(account *importedAccount, creds oldCredentials) {
	if creds.Mode != "account" || creds.AccountID == "" {
		return
	}
	fingerprint, err := sourceKey(account.harness, creds)
	if err == nil {
		in.observations[creds.AccountID+"\x00"+fingerprint] = observationOwner{oldID: creds.AccountID, fingerprint: fingerprint, id: account.id}
	}
}
func (in *importer) writeCatalog(ctx context.Context, tx *sql.Tx) error {
	var cipher *secret.Cipher
	for _, source := range slices.Sorted(maps.Keys(in.accounts)) {
		account := in.accounts[source]
		var secretID *uuid.UUID
		var storeName, key *string
		if account.credentials.Mode == "api_key" {
			if cipher == nil {
				var err error
				cipher, err = secret.New(os.Getenv("ENV_ENCRYPTION_KEY"))
				if err != nil {
					return errors.New("migration encryption key is unavailable")
				}
			}
			value, err := cipher.EncryptAccount(account.id, in.environments[account.credentials.APIKeyEnv])
			if err != nil {
				return err
			}
			secretID = new(uuid.New())
			if _, err := tx.ExecContext(ctx, `INSERT INTO secrets(id,nonce,ciphertext) VALUES($1,$2,$3)`, *secretID, value.Nonce, value.Ciphertext); err != nil {
				return err
			}
		} else {
			storeName = &account.storeName
			key = &account.credentials.Key
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO llm_accounts(id,name,harness,auth_mode,active,api_key_secret_id,credential_store,credential_key) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, account.id, account.name, account.harness, account.credentials.Mode, account.active, secretID, storeName, key); err != nil {
			return err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(in.profiles)) {
		profile := in.profiles[name]
		options, err := json.Marshal(profile.Codex)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO model_profiles(id,name,description,llm_account_id,model,instructions,codex,active) VALUES($1,$2,$3,$4,$5,$6,$7,true)`, uuid.New(), profile.Name, profile.Description, profile.AccountID, profile.Model, profile.Instructions, string(options)); err != nil {
			return err
		}
	}
	return nil
}
