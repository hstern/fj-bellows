// Package config loads fj-bellows configuration from YAML.
//
// The provider_config subtree is intentionally kept as a raw yaml.Node so the
// core never needs to know provider-specific fields; the selected provider
// decodes it into its own struct (deferred decode).
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration.
type Config struct {
	Forgejo Forgejo `yaml:"forgejo"`
	Scale   Scale   `yaml:"scale"`

	// Provider names the registered provider implementation, e.g. "linode".
	Provider string `yaml:"provider"`

	// ProviderConfig is opaque to the core. The chosen provider decodes it.
	ProviderConfig yaml.Node `yaml:"provider_config"`

	Poll Poll `yaml:"poll"`
	SSH  SSH  `yaml:"ssh"`

	// Tag is stamped on every provisioned instance so reconcile and the orphan
	// sweep can find instances this daemon owns.
	Tag string `yaml:"tag"`
}

// Forgejo describes how to reach the Forgejo Actions API.
type Forgejo struct {
	URL string `yaml:"url"`
	// Token is the admin token used to poll the queue and mint ephemeral
	// registrations.
	Token string `yaml:"token"`

	// Scope is the API path segment that owns the runners, e.g. "orgs/example"
	// or "repos/owner/name". Endpoints are built as
	// <url>/api/v1/<scope>/actions/runners...
	Scope string `yaml:"scope"`

	// Labels this pool services. A waiting job is eligible only if all of its
	// required labels are present here.
	Labels []string `yaml:"labels"`
}

// Scale bounds the warm pool.
type Scale struct {
	Max int `yaml:"max"`
}

// Poll controls the reconcile cadence and teardown timers.
type Poll struct {
	Interval Duration `yaml:"interval"`

	// IdleTimeout applies to per-second billing providers: tear an idle node
	// down once it has been idle this long.
	IdleTimeout Duration `yaml:"idle_timeout"`

	// HourMargin applies to hourly-rounding billing providers: kill an idle
	// node this long before each paid-hour boundary (5m -> the :55 rule).
	HourMargin Duration `yaml:"hour_margin"`
}

// SSH configures how the orchestrator reaches worker VMs to dispatch one-job.
type SSH struct {
	User string `yaml:"user"`
	// PrivateKeyFile points at the PEM private key whose public half is injected
	// into each worker at provision time. A key file is referenced by path
	// rather than inlined to keep config.yaml tidy.
	PrivateKeyFile string `yaml:"private_key_file"`
	Port           int    `yaml:"port"`
}

// Load reads, parses, defaults, and validates a config file.
func Load(path string) (*Config, error) {
	//nolint:gosec // G304: path is the operator-supplied config file, not user input.
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// DefaultTag is the instance tag used when none is configured. Because the tag
// is the *only* thing scoping which cloud instances a deployment owns, two
// deployments sharing this default against the same cloud account would adopt
// and destroy each other's VMs. Set a unique tag per deployment.
const DefaultTag = "fj-bellows"

func (c *Config) applyDefaults() {
	if c.Tag == "" {
		c.Tag = DefaultTag
	}
	if c.Scale.Max == 0 {
		c.Scale.Max = 1
	}
	if c.Poll.Interval == 0 {
		c.Poll.Interval = Duration(10 * time.Second)
	}
	if c.Poll.IdleTimeout == 0 {
		c.Poll.IdleTimeout = Duration(5 * time.Minute)
	}
	if c.Poll.HourMargin == 0 {
		c.Poll.HourMargin = Duration(5 * time.Minute)
	}
	if c.SSH.User == "" {
		c.SSH.User = "root"
	}
	if c.SSH.Port == 0 {
		c.SSH.Port = 22
	}
}

// ProviderDocker is the name of the local docker provider, which does not
// dispatch over SSH and therefore needs no SSH private key.
const ProviderDocker = "docker"

func (c *Config) validate() error {
	var missing []string
	if c.Forgejo.URL == "" {
		missing = append(missing, "forgejo.url")
	}
	if c.Forgejo.Token == "" {
		missing = append(missing, "forgejo.token")
	}
	if c.Forgejo.Scope == "" {
		missing = append(missing, "forgejo.scope")
	}
	if c.Provider == "" {
		missing = append(missing, "provider")
	}
	// SSH key is required only for providers that dispatch over SSH. The
	// docker provider execs into local containers and needs no SSH at all.
	if c.SSH.PrivateKeyFile == "" && c.Provider != ProviderDocker {
		missing = append(missing, "ssh.private_key_file")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required fields: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Duration is a time.Duration that unmarshals from a Go duration string ("10s").
type Duration time.Duration

// UnmarshalYAML parses a duration string such as "10s" or "5m".
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	pd, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(pd)
	return nil
}

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }
