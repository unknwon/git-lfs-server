package forge

import (
	"context"

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
