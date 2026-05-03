package forge

import (
	"context"
	"strings"

	"github.com/cockroachdb/errors"

	"unknwon.dev/git-lfs-server/internal/logx"
)

// Type identifies a forge implementation. The value is used in ini
// configuration as the TYPE key under [forge "{host}"].
type Type string

const (
	TypeGitHub   Type = "github"
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
	Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, error)
}

// ErrTokenInvalid signals that the supplied token is invalid or lacks the
// required repository access.
var ErrTokenInvalid = errors.New("forge: token is invalid or lacks repository access")

// SkipAuthProvider wraps another Provider and short-circuits Authorize to
// always return PermissionWrite without contacting the underlying forge.
// Used only when [forge "{host}"] sets SKIP_AUTH = true.
type SkipAuthProvider struct{}

func (SkipAuthProvider) Type() Type { return TypeSkipAuth }

func (SkipAuthProvider) Authorize(ctx context.Context, logger *logx.Logger, repo, token string) (Permission, error) {
	return PermissionWrite, nil
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
		if err := validateAllowlistEntry(entry); err != nil {
			return nil, errors.Wrapf(err, "invalid entry %q", entry)
		}
		if prefix, ok := strings.CutSuffix(entry, "/**"); ok {
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
// permitted. A nil receiver allows everything.
func (a *RepoAllowlist) Allow(repo string) bool {
	if a == nil {
		return true
	}
	if _, ok := a.exact[repo]; ok {
		return true
	}
	for _, p := range a.prefixes {
		if strings.HasPrefix(repo, p) {
			return true
		}
	}
	return false
}

func validateAllowlistEntry(entry string) error {
	if entry == "**" {
		return errors.New(`bare "**" matches everything; leave the key empty instead`)
	}
	if strings.HasPrefix(entry, "/") {
		return errors.New(`must not start with "/"`)
	}
	if strings.Contains(entry, "//") {
		return errors.New(`must not contain "//"`)
	}
	if prefix, ok := strings.CutSuffix(entry, "/**"); ok {
		if prefix == "" {
			return errors.New(`prefix before "/**" must be non-empty`)
		}
		if strings.Contains(prefix, "*") {
			return errors.New(`"*" is only allowed as the final "/**" segment`)
		}
		return nil
	}
	if strings.Contains(entry, "*") {
		return errors.New(`"*" is only allowed as the final "/**" segment`)
	}
	return nil
}
