// Package linode implements the provider.Provider interface for Linode.
//
// Linode bills whole hours rounded up, so it reports BillingHourlyRoundUp and
// the core keeps VMs warm for the paid hour. Provisioning passes cloud-init via
// the Linode Metadata service (user-data) and tags every instance so reconcile
// and the orphan sweep can find them.
package linode

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/linode/linodego"
	"gopkg.in/yaml.v3"

	"github.com/hstern/fj-bellows/internal/provider"
)

// config is the provider_config subtree for Linode.
type config struct {
	Region     string          `yaml:"region"`
	Type       string          `yaml:"type"`
	Image      string          `yaml:"image"`
	Token      string          `yaml:"token"`
	FirewallID int             `yaml:"firewall_id"`
	Firewall   *firewallConfig `yaml:"firewall"`
}

// Linode is the provider implementation.
type Linode struct {
	cfg    config
	client linodego.Client
	tag    string // cfg.Tag from the orchestrator, captured in Configure

	// fw is non-nil when managed-firewall mode is enabled (the `firewall:`
	// block is set in provider_config). It's created at Configure time —
	// no lazy-init, no first-Provision retries hammering upstream sentinels.
	// nil means firewall_id mode (or no firewall at all).
	fw *managedFirewall
}

func init() {
	provider.Register("linode", func() provider.Provider { return &Linode{} })
}

// Configure decodes the opaque node and prepares the API client.
//
// For the managed-firewall mode (`firewall:` block), Configure also:
//   - resolves the allow_inbound sentinels (`auto`, `github-actions`) and
//     fails fast on any error (silent fallback to literal CIDRs would risk
//     locking out the orchestrator from its own workers);
//   - creates the Cloud Firewall via the Linode API and starts the refresh
//     goroutine.
//
// All firewall work happens here, at startup. Failures surface immediately
// (PAT-scope misconfiguration, network blips, missing sentinels) instead
// of deferred to the first Provision call when the first job lands.
// See #26.
func (l *Linode) Configure(ctx context.Context, tag string, node yaml.Node) error {
	if err := node.Decode(&l.cfg); err != nil {
		return fmt.Errorf("linode: decode provider_config: %w", err)
	}
	var missing []string
	if l.cfg.Region == "" {
		missing = append(missing, "region")
	}
	if l.cfg.Type == "" {
		missing = append(missing, "type")
	}
	if l.cfg.Image == "" {
		missing = append(missing, "image")
	}
	if l.cfg.Token == "" {
		missing = append(missing, "token")
	}
	if len(missing) > 0 {
		return fmt.Errorf("linode: provider_config missing: %s", strings.Join(missing, ", "))
	}
	if l.cfg.Firewall != nil && l.cfg.FirewallID != 0 {
		return errors.New("linode: provider_config: `firewall` and `firewall_id` are mutually exclusive")
	}
	client := linodego.NewClient(nil)
	client.SetToken(l.cfg.Token)
	l.client = client
	l.tag = tag

	if l.cfg.Firewall != nil {
		fw := newManagedFirewall(*l.cfg.Firewall, tag, &l.client, slog.Default())
		if err := fw.primeResolved(ctx); err != nil {
			return fmt.Errorf("linode: firewall: %w", err)
		}
		if err := fw.ensureAtConfigure(ctx); err != nil {
			return fmt.Errorf("linode: firewall: %w", err)
		}
		fw.startRefreshLoop()
		l.fw = fw
	}
	return nil
}

// Provision creates a tagged Linode with the rendered cloud-init as user-data.
func (l *Linode) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	rootPass, err := randomPassword(32)
	if err != nil {
		return provider.Instance{}, err
	}
	booted := true
	opts := linodego.InstanceCreateOptions{
		Region:   l.cfg.Region,
		Type:     l.cfg.Type,
		Image:    l.cfg.Image,
		Label:    spec.Name,
		Tags:     []string{spec.Tag},
		RootPass: rootPass,
		Booted:   &booted,
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString([]byte(spec.UserData)),
		},
	}
	if key := strings.TrimSpace(spec.AuthorizedKey); key != "" {
		opts.AuthorizedKeys = []string{key}
	}
	switch {
	case l.fw != nil:
		opts.FirewallID = l.fw.id
	case l.cfg.FirewallID != 0:
		opts.FirewallID = l.cfg.FirewallID
	}
	inst, err := l.client.CreateInstance(ctx, opts)
	if err != nil {
		return provider.Instance{}, fmt.Errorf("linode: create instance: %w", err)
	}
	return toInstance(*inst), nil
}

// Destroy deletes the instance with the given ID.
//
// When managed-firewall mode is on, the last Destroy in a deployment triggers
// firewall cleanup (the firewall is removed once no devices remain attached).
// -destroy-on-exit naturally flows through here per instance, so we get
// cleanup for free without a Provider.Shutdown hook.
func (l *Linode) Destroy(ctx context.Context, id string) error {
	n, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("linode: bad instance id %q: %w", id, err)
	}
	if err := l.client.DeleteInstance(ctx, n); err != nil {
		return fmt.Errorf("linode: delete instance %d: %w", n, err)
	}
	if l.fw != nil {
		l.fw.maybeCleanupFirewall(ctx)
	}
	return nil
}

// List returns all instances carrying tag.
func (l *Linode) List(ctx context.Context, tag string) ([]provider.Instance, error) {
	insts, err := l.client.ListInstances(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("linode: list instances: %w", err)
	}
	var out []provider.Instance
	for _, in := range insts {
		if slices.Contains(in.Tags, tag) {
			out = append(out, toInstance(in))
		}
	}
	return out, nil
}

// BillingModel reports hourly rounding.
func (l *Linode) BillingModel() provider.BillingModel {
	return provider.BillingHourlyRoundUp
}

func toInstance(in linodego.Instance) provider.Instance {
	var ip string
	if len(in.IPv4) > 0 && in.IPv4[0] != nil {
		ip = in.IPv4[0].String()
	}
	var created time.Time
	if in.Created != nil {
		created = *in.Created
	}
	var tag string
	if len(in.Tags) > 0 {
		tag = in.Tags[0]
	}
	return provider.Instance{
		ID:        strconv.Itoa(in.ID),
		Name:      in.Label,
		IPv4:      ip,
		CreatedAt: created,
		Tag:       tag,
	}
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789!@#%^&*"

// randomPassword returns a strong random root password. It is never used to log
// in (the orchestrator authenticates with an SSH key) but Linode requires one.
func randomPassword(n int) (string, error) {
	b := make([]byte, n)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", fmt.Errorf("linode: generate password: %w", err)
		}
		b[i] = passwordAlphabet[idx.Int64()]
	}
	return string(b), nil
}
