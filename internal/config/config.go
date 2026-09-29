// Package config loads conduit.yaml with ${env:VAR} / ${file:...} expansion,
// validates the minimal invariants the gateway needs, and exposes typed access.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Server struct {
	Listen        string        `yaml:"listen"`
	AdminListen   string        `yaml:"admin_listen"`
	Mode          string        `yaml:"mode"`
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
	MaxBodyBytes  int64         `yaml:"max_body_bytes"`
}

type Storage struct {
	Driver          string `yaml:"driver"`
	DSN             string `yaml:"dsn"`
	LedgerRetention string `yaml:"ledger_retention"`
	ContentLogging  string `yaml:"content_logging"`
}

type VirtualKey struct {
	ID            string `yaml:"id"`
	Key           string `yaml:"key"`
	Tenant        string `yaml:"tenant"`
	DefaultPolicy string `yaml:"default_policy"`
	RPM           int    `yaml:"rpm"`
}

type Auth struct {
	AdminKeys   []string     `yaml:"admin_keys"`
	VirtualKeys []VirtualKey `yaml:"virtual_keys"`
}

type Provider struct {
	Name         string            `yaml:"name"`
	Type         string            `yaml:"type"`
	BaseURL      string            `yaml:"base_url"`
	AllowPrivate bool              `yaml:"allow_private"`
	APIKeys      []string          `yaml:"api_keys"`
	RateLimits   map[string]int64  `yaml:"rate_limits"`
	Timeouts     map[string]string `yaml:"timeouts"`
}

type Price struct {
	Input  float64 `yaml:"input"`
	Output float64 `yaml:"output"`
}

type LatencyPrior struct {
	TTFTMs       float64 `yaml:"ttft_ms"`
	TokensPerSec float64 `yaml:"tokens_per_sec"`
}

type Model struct {
	ID              string             `yaml:"id"`
	Provider        string             `yaml:"provider"`
	Upstream        string             `yaml:"upstream"`
	ContextTokens   int                `yaml:"context_tokens"`
	MaxOutputTokens int                `yaml:"max_output_tokens"`
	Capabilities    []string           `yaml:"capabilities"`
	PricePerMtok    Price              `yaml:"price_per_mtok"`
	LatencyPrior    LatencyPrior       `yaml:"latency_prior"`
	PriorStrength   float64            `yaml:"prior_strength"`
	Priors          map[string]float64 `yaml:"priors"`
	Regions         []string           `yaml:"regions"`
}

func (m *Model) HasCap(c string) bool {
	for _, k := range m.Capabilities {
		if k == c {
			return true
		}
	}
	return false
}

type Explore struct {
	Enabled bool    `yaml:"enabled"`
	Epsilon float64 `yaml:"epsilon"`
	Delta   float64 `yaml:"delta"`
	Tau     float64 `yaml:"tau"`
}

type Hedging struct {
	Enabled    bool    `yaml:"enabled"`
	MinDelayMs int     `yaml:"min_delay_ms"`
	MaxShare   float64 `yaml:"max_share"`
}

type Reliability struct {
	MaxAttempts    int    `yaml:"max_attempts"`
	MidStream      string `yaml:"mid_stream"`
	RetryOnRefusal bool   `yaml:"retry_on_refusal"`
}

type Constraints struct {
	MinQuality   float64 `yaml:"min_quality"`
	MaxLatencyMs int     `yaml:"max_latency_ms"`
	MaxCostUSD   float64 `yaml:"max_cost_usd"`
}

type Reward struct {
	Sources []string `yaml:"sources"`
}

type Policy struct {
	Name         string             `yaml:"name"`
	Match        string             `yaml:"match"`
	Objective    string             `yaml:"objective"`
	Weights      map[string]float64 `yaml:"weights"`
	Constraints  Constraints        `yaml:"constraints"`
	AllowModels  []string           `yaml:"allow_models"`
	DenyModels   []string           `yaml:"deny_models"`
	Regions      []string           `yaml:"regions"`
	Exploration  Explore            `yaml:"exploration"`
	Hedging      Hedging            `yaml:"hedging"`
	Reliability  Reliability        `yaml:"reliability"`
	OnInfeasible string             `yaml:"on_infeasible"`
	OnSoftLimit  string             `yaml:"on_soft_limit"`
	Reward       Reward             `yaml:"reward"`
	// Static strategies for baselines / tests.
	Strategy string   `yaml:"strategy"` // "" | fixed | round_robin
	Model    string   `yaml:"model"`
	Models   []string `yaml:"models"`
}

type Budget struct {
	Scope        string  `yaml:"scope"`
	Window       string  `yaml:"window"`
	HardLimitUSD float64 `yaml:"hard_limit_usd"`
	SoftLimitUSD float64 `yaml:"soft_limit_usd"`
}

type Cache struct {
	Exact map[string]any `yaml:"exact"`
}

type Config struct {
	Server      Server            `yaml:"server"`
	Storage     Storage           `yaml:"storage"`
	Cluster     map[string]string `yaml:"cluster"`
	Auth        Auth              `yaml:"auth"`
	Providers   []Provider        `yaml:"providers"`
	Models      []Model           `yaml:"models"`
	TaskClasses []string          `yaml:"task_classes"`
	Policies    []Policy          `yaml:"policies"`
	Budgets     []Budget          `yaml:"budgets"`
	Cache       Cache             `yaml:"cache"`
	Version     string            `yaml:"-"`
}

var envRef = regexp.MustCompile(`\$\{(env|file):([^}]+)\}`)

func expand(s string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		parts := envRef.FindStringSubmatch(m)
		if len(parts) != 3 {
			return m
		}
		switch parts[1] {
		case "env":
			return os.Getenv(parts[2])
		case "file":
			b, err := os.ReadFile(parts[2])
			if err != nil {
				return ""
			}
			return strings.TrimSpace(string(b))
		}
		return m
	})
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	expanded := envRef.ReplaceAllFunc(b, func(m []byte) []byte {
		return []byte(expand(string(m)))
	})
	var c Config
	if err := yaml.Unmarshal(expanded, &c); err != nil {
		return nil, err
	}
	if c.Server.Listen == "" {
		c.Server.Listen = ":8080"
	}
	if c.Server.AdminListen == "" {
		c.Server.AdminListen = ":8081"
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) Validate() error {
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	if len(c.Policies) == 0 {
		return fmt.Errorf("no policies configured")
	}
	seen := map[string]bool{}
	for _, p := range c.Policies {
		if p.Name == "" {
			return fmt.Errorf("policy with empty name")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate policy %q", p.Name)
		}
		seen[p.Name] = true
	}
	for _, m := range c.Models {
		if m.ID == "" || m.Provider == "" {
			return fmt.Errorf("model with empty id/provider")
		}
	}
	return nil
}

func (c *Config) PolicyByName(name string) *Policy {
	for i := range c.Policies {
		if c.Policies[i].Name == name {
			return &c.Policies[i]
		}
	}
	return nil
}

func (c *Config) ProviderByName(name string) *Provider {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}

func (c *Config) ModelByID(id string) *Model {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}
