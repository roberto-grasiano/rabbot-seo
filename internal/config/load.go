package config

import (
	"os"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
	"github.com/spf13/pflag"
)

const (
	envPrefix = "RABBOT_"
	keyDelim  = "."
)

// interpolate expands ${VAR} references against the process environment.
// Used for secrets (webhook URLs, tokens, basic-auth) which are never logged.
func interpolate(s string) string {
	return os.Expand(s, func(name string) string {
		return os.Getenv(name)
	})
}

// Load builds the effective Config by merging, in order: built-in defaults,
// config.yaml (if it exists), RABBOT_ environment variables, and CLI flags.
// A missing config file is not an error. Pass flags=nil when there are none.
//
// Env mapping: strip the RABBOT_ prefix, lowercase, then map "__" -> "."
// (key delimiter). A single underscore stays part of the key name, so
// RABBOT_DATA_DIR -> data_dir and RABBOT_CONTROL__PORT -> control.port.
func Load(configPath string, flags *pflag.FlagSet) (Config, error) {
	k := koanf.New(keyDelim)

	// 1. Built-in defaults.
	if err := k.Load(structs.Provider(Defaults(), "koanf"), nil); err != nil {
		return Config{}, err
	}

	// 2. config.yaml (optional).
	if configPath != "" {
		if _, statErr := os.Stat(configPath); statErr == nil {
			if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
				return Config{}, err
			}
		}
	}

	// 3. Environment: RABBOT_CONTROL__PORT -> control.port; RABBOT_DATA_DIR -> data_dir
	envProvider := env.Provider(keyDelim, env.Opt{
		Prefix: envPrefix,
		TransformFunc: func(k, v string) (string, any) {
			key := strings.TrimPrefix(k, envPrefix)
			key = strings.ToLower(key)
			key = strings.ReplaceAll(key, "__", ".")
			return key, v
		},
	})
	if err := k.Load(envProvider, nil); err != nil {
		return Config{}, err
	}

	// 4. CLI flags (highest precedence).
	if flags != nil {
		if err := k.Load(posflag.Provider(flags, keyDelim, k), nil); err != nil {
			return Config{}, err
		}
	}

	var c Config
	if err := k.Unmarshal("", &c); err != nil {
		return Config{}, err
	}

	// Expand ${ENV} references in secret-bearing fields (contracts §5: Slack
	// webhook URLs, access tokens/headers, basic-auth, cookies, proxy URL).
	// These are never logged. Non-secret fields are left verbatim so literal
	// '$' characters in e.g. segment match patterns are not mangled.
	interpolateSecrets(&c)

	return c, nil
}

// interpolateSecrets expands ${ENV} references against the process environment
// in every secret-bearing string field, in place.
func interpolateSecrets(c *Config) {
	for i := range c.Notifiers {
		n := &c.Notifiers[i]
		// URL (slack/webhook) and the email password are secrets; so are the
		// generic-webhook header VALUES (e.g. Authorization: Bearer …). Each may
		// carry a ${ENV} token so the secret can live outside config.yaml. Header
		// KEYS are not interpolated — only their values.
		n.URL = interpolate(n.URL)
		n.Password = interpolate(n.Password)
		for k, v := range n.Headers {
			n.Headers[k] = interpolate(v)
		}
	}
	for i := range c.Sites {
		a := &c.Sites[i].Access
		a.BasicUser = interpolate(a.BasicUser)
		a.BasicPass = interpolate(a.BasicPass)
		a.ProxyURL = interpolate(a.ProxyURL)
		for k, v := range a.Headers {
			a.Headers[k] = interpolate(v)
		}
		for k, v := range a.Cookies {
			a.Cookies[k] = interpolate(v)
		}
		// GSC credential references are PATHS to 0600 files (mirroring control.token,
		// not inline secrets). Interpolate only the path strings so a ${VAR} can hold
		// the path outside config.yaml; the credential CONTENT is read at runtime from
		// the path and is never stored inline or logged.
		g := &c.Sites[i].GSC
		g.ServiceAccountKeyFile = interpolate(g.ServiceAccountKeyFile)
		g.OAuthTokenFile = interpolate(g.OAuthTokenFile)
	}
}
