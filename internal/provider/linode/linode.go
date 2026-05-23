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
	tag    string           // cfg.Tag from the orchestrator, captured on first Provision
	fw     *managedFirewall // nil when Firewall block is absent (firewall_id mode or no firewall)
}

func init() {
	provider.Register("linode", func() provider.Provider { return &Linode{} })
}

// Configure decodes the opaque node and prepares the API client.
//
// For the managed-firewall mode (`firewall:` block), Configure also resolves
// the allow_inbound sentinels EAGERLY (`auto`, `github-actions`) and fails
// fast if any sentinel cannot be resolved or if the resolved set is empty —
// rather than starting the daemon with a default-deny firewall nobody can
// reach. The actual Linode firewall is created lazily on first Provision
// (once we have the orchestrator's tag); the refresh goroutine starts then
// too. See #26.
func (l *Linode) Configure(node yaml.Node) error {
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
	if l.cfg.Firewall != nil {
		probe := newManagedFirewall(*l.cfg.Firewall, "<configure-probe>", nil, slog.Default())
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cidrs, err := probe.resolveAllowInbound(ctx)
		cancel()
		if err != nil {
			return fmt.Errorf("linode: firewall.allow_inbound: %w", err)
		}
		if len(cidrs) == 0 {
			return errors.New("linode: firewall.allow_inbound resolved to zero CIDRs; refusing to start with a default-deny firewall nobody can reach")
		}
	}
	client := linodego.NewClient(nil)
	client.SetToken(l.cfg.Token)
	l.client = client
	return nil
}

// ensureManagedFirewall lazy-creates the managed firewall on first Provision
// (when we finally know spec.Tag) and starts the refresh goroutine. Safe
// against concurrent Provisions; retries on each call until init succeeds so
// a transient API blip during the very first Provision doesn't wedge the
// deployment permanently.
func (l *Linode) ensureManagedFirewall(ctx context.Context, tag string) error {
	if l.cfg.Firewall == nil {
		return nil
	}
	if l.fw != nil {
		return nil
	}
	m := newManagedFirewall(*l.cfg.Firewall, tag, &l.client, slog.Default())
	if err := m.ensureAtConfigure(ctx); err != nil {
		return fmt.Errorf("linode: managed firewall: %w", err)
	}
	m.startRefreshLoop()
	l.fw = m
	l.tag = tag
	return nil
}

// Provision creates a tagged Linode with the rendered cloud-init as user-data.
func (l *Linode) Provision(ctx context.Context, spec provider.Spec) (provider.Instance, error) {
	// If managed-firewall mode is enabled, lazy-create the firewall on the
	// very first Provision and start the refresh goroutine. Subsequent
	// Provisions short-circuit immediately.
	if err := l.ensureManagedFirewall(ctx, spec.Tag); err != nil {
		return provider.Instance{}, err
	}
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
