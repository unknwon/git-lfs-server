package main

import (
	"os"
	"strings"

	"github.com/cockroachdb/errors"
	"gopkg.in/ini.v1"

	"unknwon.dev/git-lfs-server/internal/database"
	"unknwon.dev/git-lfs-server/internal/embed"
	"unknwon.dev/git-lfs-server/internal/forge"
	"unknwon.dev/git-lfs-server/internal/storage"
)

// Host is the host of a forge, e.g., "github.com".
type Host = string

type Config struct {
	Server   ServerConfig
	Database database.Config
	Forges   map[Host]forge.Provider
	Storages map[Host]storage.Backend
	Log      LogConfig
}

type ServerConfig struct {
	ListenAddress string `ini:"LISTEN_ADDRESS"`
	ExternalURL   string `ini:"EXTERNAL_URL"`
	MaxObjectSize int64  `ini:"MAX_OBJECT_SIZE"`
}

type LogConfig struct {
	Level string `ini:"LEVEL"`
}

const (
	storageSectionPrefix = `storage "`
	forgeSectionPrefix   = `forge "`
)

func loadConfig(customPath string) (*Config, error) {
	f, err := ini.Load(embed.Config)
	if err != nil {
		return nil, errors.Wrap(err, "load embedded config")
	}
	if customPath != "" {
		if err := f.Append(customPath); err != nil {
			return nil, errors.Wrapf(err, "load custom config %q", customPath)
		}
	}
	f.ValueMapper = os.ExpandEnv

	var c Config

	if err := f.Section("server").MapTo(&c.Server); err != nil {
		return nil, errors.Wrap(err, `map [server] config`)
	}
	if err := f.Section("database").MapTo(&c.Database); err != nil {
		return nil, errors.Wrap(err, `map [database] config`)
	}
	if err := f.Section("log").MapTo(&c.Log); err != nil {
		return nil, errors.Wrap(err, `map [log] config`)
	}

	backendsByName := make(map[string]storage.Backend)
	for _, s := range f.Sections() {
		rest, ok := strings.CutPrefix(s.Name(), storageSectionPrefix)
		if !ok {
			continue
		}
		name, ok := strings.CutSuffix(rest, `"`)
		if !ok {
			return nil, errors.Newf("malformed storage section name %q", s.Name())
		}
		if _, dup := backendsByName[name]; dup {
			return nil, errors.Newf("duplicate storage configuration for name %q", name)
		}
		scheme := s.Key("SCHEME").String()
		if err := validateStorageScheme(scheme); err != nil {
			return nil, errors.Wrapf(err, "storage %q", name)
		}
		t := storage.Type(s.Key("TYPE").String())
		switch t {
		case storage.TypeFilesystem:
			b, err := storage.NewFilesystemBackend(name, scheme, s.Key("ROOT").String(), s.Key("TEMP_DIR").String())
			if err != nil {
				return nil, errors.Wrapf(err, "init storage %q", name)
			}
			backendsByName[name] = b
		case storage.TypeS3Presign:
			b, err := storage.NewS3PresignBackend(
				name,
				scheme,
				s.Key("BUCKET").String(),
				s.Key("ACCESS_KEY_ID").String(),
				s.Key("SECRET_ACCESS_KEY").String(),
				s.Key("ENDPOINT").String(),
			)
			if err != nil {
				return nil, errors.Wrapf(err, "init storage %q", name)
			}
			backendsByName[name] = b
		case "":
			return nil, errors.Newf("storage %q is missing required key %q", name, "TYPE")
		default:
			return nil, errors.Newf(`storage %q has unknown TYPE %q`, name, t)
		}
	}

	c.Forges = make(map[string]forge.Provider)
	c.Storages = make(map[string]storage.Backend)
	for _, s := range f.Sections() {
		rest, ok := strings.CutPrefix(s.Name(), forgeSectionPrefix)
		if !ok {
			continue
		}
		host, ok := strings.CutSuffix(rest, `"`)
		if !ok {
			return nil, errors.Newf("malformed forge section name %q", s.Name())
		}
		host = strings.ToLower(host)
		if _, dup := c.Forges[host]; dup {
			return nil, errors.Newf("duplicate forge configuration for host %q", host)
		}
		var fc forge.Config
		if err := s.MapTo(&fc); err != nil {
			return nil, errors.Wrapf(err, "map [forge %q] config", host)
		}
		if fc.Storage == "" {
			return nil, errors.Newf("forge %q is missing required key %q", host, "STORAGE")
		}
		backend, ok := backendsByName[fc.Storage]
		if !ok {
			return nil, errors.Newf("forge %q references unconfigured storage %q", host, fc.Storage)
		}
		allowlist, err := forge.NewRepoAllowlist(fc.RepoAllowlist)
		if err != nil {
			return nil, errors.Wrapf(err, "forge %q REPO_ALLOWLIST", host)
		}
		if fc.SkipAuth {
			c.Forges[host] = forge.NewSkipAuthProvider(allowlist)
		} else {
			switch fc.Type {
			case forge.TypeGitHub:
				c.Forges[host] = forge.NewGitHubProvider(host, allowlist)
			case forge.TypeGitLab:
				c.Forges[host] = forge.NewGitLabProvider(host, allowlist)
			default:
				return nil, errors.Newf("forge %q has unknown type %q", host, fc.Type)
			}
		}
		c.Storages[host] = backend
	}
	if len(c.Forges) == 0 {
		return nil, errors.New("no forge configured")
	}

	return &c, nil
}

func validateStorageScheme(scheme string) error {
	if scheme == "" {
		return errors.New("SCHEME is required")
	}
	if !strings.HasSuffix(scheme, "://") {
		return errors.Newf("SCHEME %q must end with %q", scheme, "://")
	}
	return nil
}
