# Git LFS Server

Bring Your Own Forge (BYOF) Git LFS server with affordable storage.

## Price comparison

Assuming 1 TiB is uploaded and stored for a full month, and 3 TiB is downloaded in the same month:

| Provider | Storage | Egress | Yearly total | Savings vs. GitLab LFS |
| --- | ---: | ---: | ---: | ---: |
| GitLab LFS | 109 x 10 GB-month x $5 x 12 = $6,540.00 | Included | $6,540.00 | Baseline |
| GitHub LFS | 1,014 GiB x $0.07 x 12 = $851.76 | 36,744 GiB x $0.0875 = $3,215.10 | $4,066.86 | $2,473.14 (37.81%) |
| DigitalOcean Spaces | ($5 + 774 GiB x $0.02) x 12 = $245.76 | 24,576 GiB x $0.01 = $245.76 | $491.52 | $6,048.48 (92.48%) |
| Cloudflare R2 Standard | 1,090 GB-month x $0.015 x 12 = $196.20 | Included | $196.20 | $6,343.80 (97.00%) |
| Backblaze B2 | 1 TB x $6.95 x 12 = $83.40 | Free up to 3x storage | $83.40 | $6,456.60 (98.72%) |

## Example setup

### Server

The server loads its built-in defaults from [`internal/embed/config.ini`](internal/embed/config.ini) and then layers a custom file on top. The custom file path is read from `LFSD_CONFIG_PATH` and defaults to `config.ini` in the working directory:

```sh
LFSD_CONFIG_PATH=/etc/lfsd/config.ini lfsd
```

Values of the form `${NAME}` in the config are expanded from the process environment at load time. A minimal override that serves GitHub.com repositories from Cloudflare R2 at `https://lfs.example.com` looks like:

```ini
[server]
EXTERNAL_URL = https://lfs.example.com

[forge "github.com"]
TYPE = github
STORAGE = r2

[storage "r2"]
TYPE = s3-presign
SCHEME = r2://
ACCOUNT_ID = ${LFSD_R2_ACCOUNT_ID}
BUCKET = lfs-objects
ACCESS_KEY_ID = ${LFSD_R2_ACCESS_KEY_ID}
SECRET_ACCESS_KEY = ${LFSD_R2_SECRET_ACCESS_KEY}
ENDPOINT = https://%(ACCOUNT_ID)s.r2.cloudflarestorage.com
```

`EXTERNAL_URL` is the public origin clients reach the server at. It is used as the base for the object download, upload, and verify URLs the server returns to the client, so it must match what the client sees (typically the public HTTPS URL terminated by your reverse proxy).

At least one `[forge "{host}"]` section must be configured. Each forge references a `[storage "{name}"]` section by name; multiple forges may share a single storage backend. Supported storage types are `filesystem` and `s3-presign` (compatible with S3, R2, DigitalOcean Spaces, etc.).

### Client

Git LFS resolves the LFS endpoint per repository. To point a clone at lfsd, set `lfs.url` in the repository's `.lfsconfig` (committed so collaborators inherit it) to `EXTERNAL_URL` joined with the forge host and the repository path:

```sh
git config -f .lfsconfig lfs.url https://lfs.example.com/github.com/myorg/myrepo
```

For the GitHub.com repository `myorg/myrepo`, this maps to the server route `/{host}/{**}/info/lfs/objects`, where `{host}` is `github.com` and `{**}` is `myorg/myrepo`. The client then sends the same forge token it would send to GitHub directly (configured via your usual Git credential helper) as the HTTP Basic password.

## Authentication

The server does not maintain its own user database. Clients authenticate to lfsd over HTTP Basic auth, where the password slot carries the same forge token the client already uses against the upstream forge. lfsd then delegates the access check to that forge before issuing object URLs.

For `TYPE = github`, the provider forwards the token as a `Bearer` credential to `GET /repos/{owner}/{repo}` on the forge's REST API:

- `200 OK` with `permissions.push` → write access (uploads and downloads).
- `200 OK` with `permissions.pull` → read access (downloads only).
- `401 Unauthorized` or `404 Not Found` → token rejected.

Both `github.com` and GitHub Enterprise Server are supported; the API base is selected from the forge host. For fine-grained PATs and GitHub App user tokens, the server reads the `github-authentication-token-expiration` response header and caches the authorization decision in memory up to the token's own expiry, with a safety margin subtracted and a maximum cap applied. Classic PATs and OAuth tokens do not advertise an expiry, so they fall back to a short default TTL.

`SKIP_AUTH = true` short-circuits the forge call and grants write access to every request. It exists only for local development; the server logs a warning at startup for any forge that has it enabled. Do not set it in production.

## Repo allowlist

A forge section may restrict which repositories the server will serve via `REPO_ALLOWLIST`. The value is a comma-separated list matched case-insensitively against the path portion of the URL after the host. An empty or unset list accepts every repository the forge authorizes.

Each entry is one of:

- A literal repo path, e.g., `myorg/my-repo`, which matches that path exactly.
- A `<prefix>/**` pattern, e.g., `myorg/**`, which matches any non-empty suffix under the prefix.

`*` is only allowed as the final `/**` segment. Bare `**` is rejected; leave the key empty to allow all repos. Example:

```ini
[forge "github.com"]
TYPE = github
STORAGE = r2
REPO_ALLOWLIST = myorg/**, otherorg/specific-repo
```

The allowlist is enforced before the forge token check, so a request for a disallowed repo is rejected without a round-trip to the forge.

## License

This project is under the MIT License. See the [LICENSE](LICENSE) file for the full license text.
