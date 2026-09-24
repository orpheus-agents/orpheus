# Browser authentication

The API supports service Bearer keys and read-only browser access. It does not
serve a frontend. All authenticated browsers see the same sessions.

| `ORPHEUS_BROWSER_AUTH` | Reads | Writes |
| --- | --- | --- |
| `api_only` (default) | Bearer | Bearer |
| `saml` | SAML session cookie or Bearer | Bearer |
| `anonymous` | No credential required | Bearer |

An explicitly supplied invalid or empty Authorization header always returns 401;
it never falls back to cookies or anonymous access. A valid SAML cookie on a write
returns 403 `read_only_access`. Cookies are ignored outside `saml`.
`PUBLIC_API_KEYS=[]` disables service commands in `saml` and `anonymous`; it is
invalid in `api_only`. Unknown browser modes fail startup of `serve`.
Workers do not load SAML certificates or metadata.

## Local read-only API

Set `ORPHEUS_BROWSER_AUTH=anonymous`, restart `serve`, then:

```sh
curl http://localhost:8000/api/v1/auth/session
curl http://localhost:8000/api/v1/sessions
```

Both work without credentials. Creating sessions still requires a service key.

## Keycloak SAML

Supply these variables to `serve` (paths are inside its container):

```sh
ORPHEUS_BROWSER_AUTH=saml
ORPHEUS_PUBLIC_URL=https://orpheus.example.com
SAML_SP_ENTITY_ID=orpheus-web
SAML_IDP_METADATA_FILE=/etc/orpheus/saml/idp.xml
SAML_SP_CERT_FILE=/etc/orpheus/saml/sp.crt
SAML_SP_KEY_FILE=/run/secrets/orpheus-saml-sp.key
BROWSER_SESSION_TTL_SECONDS=43200
```

Mount the IdP metadata and SP certificate read-only, and the SP private key as a
secret. The SP key must match its RSA/ECDSA certificate. Public URL must be an
HTTPS origin without trailing slash, credentials, path, query or fragment. TLS
termination is supplied by the installation; `serve` itself remains HTTP.
Hostnames are lowercased and default ports (80/443) removed before comparing
Origin or building SAML URLs. Forwarded/Host headers cannot change the public
origin or callback.

1. Create a SAML client in the existing Keycloak realm. Its client ID must equal
   `SAML_SP_ENTITY_ID`.
2. Configure ACS/valid redirect as exactly
   `https://orpheus.example.com/auth/callback`, HTTP-POST binding. SP metadata is
   available at `/saml/metadata` once configuration is complete.
3. Enable signature validation for AuthnRequests with the SP signing certificate.
   Sign responses and assertions using SHA-256. The IdP metadata must offer an
   HTTPS HTTP-Redirect SSO endpoint and contain its signing certificate(s).
4. Set the client's Name ID format to `username` (or another stable identity).
   Orpheus requests `unspecified` so the IdP selects the format instead of issuing
   a fresh transient ID on every login. Without attribute mappers, NameID is also
   the displayed name. To send `preferred_username` or `email`, add SAML protocol
   mappers to the Keycloak client/client scope; attributes are not supplied by
   default. Orpheus accepts both Name and FriendlyName (including X500 email),
   preferring username over email. Control admission at the IdP.
5. Export IdP metadata into the configured file and start `serve`.
6. Open `/auth/login?next=/api/v1/sessions`. After SSO the browser receives JSON
   from the session API. Existing Keycloak SSO can complete without another
   password prompt. `ForceAuthn` and IdP-initiated login are not enabled.

Metadata is local, read at startup. Malformed metadata prevents startup. Expired
metadata blocks SAML login/callback with 503 but does not prevent startup, Bearer
API calls or reads with existing unexpired cookies. Rotate IdP
metadata with overlapping signing certificates and restart API replicas. Keep
replica configuration and their PostgreSQL database consistent.

## HTTP behavior

- `GET /api/v1/auth/session` is public and ignores Authorization. It returns mode,
  authenticated/read_access, nullable user and expires_at. It clears invalid
  cookies; storage failures return 503 rather than a false logged-out state.
- `GET /auth/login?next=…` returns 302 to the IdP. `next` is a local path, defaults
  to `/api/v1/auth/session`, and rejects external URLs and auth loops. Query
  parameters, including encoded spaces and percent signs, are preserved.
- `POST /auth/callback` accepts the SAML form (maximum 1 MiB). A request-specific
  Secure/HttpOnly/SameSite=None nonce cookie is required. Consumption and creation
  of the browser session happen in one transaction; replay fails. The login GET
  stores the previous session hash, so callback revokes it even when the IdP's
  cross-site POST omits the SameSite=Lax session cookie.
- `POST /auth/logout` requires `Origin` equal to `ORPHEUS_PUBLIC_URL` and
  `X-Orpheus-CSRF: 1`. It revokes the current session, clears its cookie, and returns
  204 even if already logged out. Without a configured public origin it returns
  403. This also applies outside SAML mode. The Keycloak session is retained.
- Login/callback/metadata return 404 `auth_not_enabled` outside `saml`.
- Auth responses and API responses use `Cache-Control: no-store`. API errors
  return JSON and do not redirect to the IdP.

Session cookies are `__Host-orpheus_session`, Secure, HttpOnly, SameSite=Lax,
Path=/, with no Domain. Only SHA-256 token hashes are stored in PostgreSQL. The
session lasts the lesser of its configured TTL (300–86400 seconds, default 12h)
and the IdP's session expiry, without sliding renewal. SSE closes at expiry;
revocation and storage loss are checked within 30 seconds. Bearer streams are
unaffected. Each API replica cleans expired records every ten minutes in batches.

Callback signature/claim failures log only fixed reason categories (for example
`audience`, `issuer`, `signature`, `idp_status` for IdP rejection), never the SAML XML, identifiers or raw library
errors. At the installation's ingress, rate-limit `/auth/login`: every accepted
request signs an AuthnRequest and creates a temporary database row. Choose limits
and bursts for the expected number of users; expiry cleanup is not a rate limit.

## Verification

`make check` covers configuration, access precedence, signed SAML fixtures,
concurrent/replayed callbacks, shared storage, SSE authorization lifetime and
migration up/down. `make test-saml` additionally starts isolated Keycloak 26.7.4
and verifies signed login, stable username/subject, return query parameters, JSON
reads, existing SSO, logout, revoked-cookie
rejection and re-login through the production Handler over trusted test HTTPS.
The test realm is deleted afterward. Stop the test IdP with
`docker compose --profile saml-test stop test-keycloak` when finished.

The Keycloak test uses fixture-only credentials on the internal Compose network;
no host ports, real users, frontend, nginx or production identity settings are
required. See [Keycloak SAML clients](https://www.keycloak.org/docs/latest/server_admin/index.html#saml-clients)
and [running Keycloak in containers](https://www.keycloak.org/server/containers).
