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

type Config struct {
	Server   ServerConfig
	Database database.Config
	Forges   map[string]forge.Provider  // key: host (e.g., "github.com")
	Storages map[string]storage.Backend // key: host (e.g., "github.com")
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

	backendsByType := make(map[storage.Type]storage.Backend)
	for _, s := range f.Sections() {
		rest, ok := strings.CutPrefix(s.Name(), storageSectionPrefix)
		if !ok {
			continue
		}
		typeName, ok := strings.CutSuffix(rest, `"`)
		if !ok {
			return nil, errors.Newf("malformed storage section name %q", s.Name())
		}
		t := storage.Type(typeName)
		switch t {
		case storage.TypeLocal:
			b, err := storage.NewLocalBackend(s.Key("ROOT").String(), s.Key("TEMP_DIR").String())
			if err != nil {
				return nil, errors.Wrapf(err, "init storage %q", typeName)
			}
			backendsByType[t] = b
		default:
			return nil, errors.Newf("unknown storage type %q", typeName)
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
		backend, ok := backendsByType[fc.Storage]
		if !ok {
			return nil, errors.Newf("forge %q references unconfigured storage %q", host, fc.Storage)
		}
		if fc.SkipAuth {
			c.Forges[host] = forge.SkipAuthProvider{}
		} else {
			switch fc.Type {
			case forge.TypeGitHub:
				c.Forges[host] = forge.NewGitHubProvider(host)
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
