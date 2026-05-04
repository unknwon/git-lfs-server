package forge

import (
	"context"
	"strings"
	"time"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

// Type identifies a forge implementation. The value is used in ini
// configuration as the TYPE key under [forge "{host}"].
type Type string

const (
	TypeGitHub   Type = "github"
	TypeGitLab   Type = "gitlab"
	TypeSkipAuth Type = "skip-auth"
)

// Permission is the access level a token has been verified to hold against a
// repository.
type Permission string

const (
	PermissionRead  Permission = "read"
	PermissionWrite Permission = "write"
)

// Config is the per-forge ini section payload, e.g. [forge "github.com"].
type Config struct {
	Type Type `ini:"TYPE"`
	// Storage is the name of a [storage "{name}"] section, not the backend's
	// TYPE. Multiple forges may share the same storage.
	Storage string `ini:"STORAGE"`
	// SkipAuth bypasses forge token verification and grants write access to
	// every request. For local development only. Loaders MUST surface a
	// warning for any forge that has this enabled.
	SkipAuth bool `ini:"SKIP_AUTH"`
	// RepoAllowlist restricts which repositories on this forge the server
	// accepts. Each entry is either a literal repo path or a path ending in
	// "/**" that matches any non-empty suffix. Entries are matched against
	// the repo path (excluding host), case-insensitive. Empty list allows
	// every repo.
	RepoAllowlist []string `ini:"REPO_ALLOWLIST" delim:","`
}

// Provider authorizes a token against a repository on a specific forge host.
type Provider interface {
	// Type identifies the forge implementation, matching the TYPE key in the
	// [forge "{host}"] config section.
	Type() Type
	// Allow reports whether repo (already lowercased, full path after host)
	// is permitted by the forge's REPO_ALLOWLIST. An empty allowlist
	// permits every repo.
	Allow(repo string) bool
	// Authorize verifies that token grants access to repo and reports the
	// effective Permission. The returned duration tells the caller how long
	// the decision is safe to cache, relative to now:
	//   - Negative: caching is not safe. Either the provider explicitly opts
	//     out (e.g., skip-auth) or it received an expiry signal it could not
	//     interpret and is failing closed.
	//   - Zero: no expiry signal; the caller may apply a conservative default
	//     TTL.
	//   - Positive: the raw time until the token expires; the caller should
	//     apply a safety margin and a maximum cap before caching.
	Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, time.Duration, error)
}

// ErrTokenInvalid signals that the supplied token is invalid or lacks the
// required repository access.
var ErrTokenInvalid = errors.New("forge: token is invalid or lacks repository access")

// SkipAuthProvider short-circuits Authorize to always return PermissionWrite
// without contacting the underlying forge. Used only when [forge "{host}"]
// sets SKIP_AUTH = true. The allowlist still applies.
type SkipAuthProvider struct {
	allowlist *RepoAllowlist
}

func NewSkipAuthProvider(allowlist *RepoAllowlist) *SkipAuthProvider {
	return &SkipAuthProvider{allowlist: allowlist}
}

func (*SkipAuthProvider) Type() Type { return TypeSkipAuth }

func (p *SkipAuthProvider) Allow(repo string) bool { return p.allowlist.Allow(repo) }

func (*SkipAuthProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, time.Duration, error) {
	return PermissionWrite, -1, nil
}

// RepoAllowlist is a compiled, case-insensitive matcher for the
// REPO_ALLOWLIST config key. A nil receiver allows every repo so callers
// can use the zero map value as "unrestricted".
type RepoAllowlist struct {
	exact    map[string]struct{}
	prefixes []string
}

// NewRepoAllowlist compiles raw entries from config. Entries are trimmed
// and lowercased; empty entries are dropped so trailing commas and a key
// set to the empty string both yield a nil matcher (allow all).
func NewRepoAllowlist(entries []string) (*RepoAllowlist, error) {
	a := &RepoAllowlist{exact: make(map[string]struct{})}
	for _, raw := range entries {
		entry := strings.ToLower(strings.TrimSpace(raw))
		if entry == "" {
			continue
		}
		prefix, isPrefix, err := parseAllowlistEntry(entry)
		if err != nil {
			return nil, errors.Wrapf(err, "invalid entry %q", entry)
		}
		if isPrefix {
			a.prefixes = append(a.prefixes, prefix+"/")
			continue
		}
		a.exact[entry] = struct{}{}
	}
	if len(a.exact) == 0 && len(a.prefixes) == 0 {
		return nil, nil
	}
	return a, nil
}

// Allow reports whether repo (already lowercased, full path after host) is
// permitted.
func (a *RepoAllowlist) Allow(repo string) bool {
	if a == nil {
		return true
	}
	if _, ok := a.exact[repo]; ok {
		return true
	}
	for _, p := range a.prefixes {
		if len(repo) > len(p) && strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

// parseAllowlistEntry validates a normalized entry and reports whether it
// is a "<prefix>/**" form. For prefix entries the returned prefix has the
// trailing "/**" stripped.
func parseAllowlistEntry(entry string) (prefix string, isPrefix bool, err error) {
	if entry == "**" {
		return "", false, errors.New(`bare "**" matches everything; leave the key empty instead`)
	}
	if strings.HasPrefix(entry, "/") {
		return "", false, errors.New(`must not start with "/"`)
	}
	if strings.Contains(entry, "//") {
		return "", false, errors.New(`must not contain "//"`)
	}
	if p, ok := strings.CutSuffix(entry, "/**"); ok {
		if strings.Contains(p, "*") {
			return "", false, errors.New(`"*" is only allowed as the final "/**" segment`)
		}
		return p, true, nil
	}
	if strings.Contains(entry, "*") {
		return "", false, errors.New(`"*" is only allowed as the final "/**" segment`)
	}
	return "", false, nil
}
