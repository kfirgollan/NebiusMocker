package peer

import (
	"context"
	"strings"
)

// QuotaGateKey is the capability provided by the quotas module: it admits
// mutations against the quota allowances of their project and tenant.
var QuotaGateKey = NewKey[QuotaGate]("quotas.gate")

// QuotaGate checks quota claims. Usage is never stored: the gate computes it
// from the QuotaUsageSources (each area counts its own store), so it is
// correct after restarts and for resources created before enforcement.
type QuotaGate interface {
	// Admit checks that every positive amount of c fits the allowance of
	// c.ContainerID and of its tenant in the region, then runs apply (the
	// store write that makes the consumption visible to the area's usage
	// source). Checks and apply run under one process-wide lock, so two
	// concurrent claims cannot both slip under a limit. apply must only
	// write the area's own store: it must not call Admit or wait on another
	// operation. When a quota is exhausted, Admit returns
	// errs.QuotaExceeded and apply does not run. A nil apply only checks.
	Admit(ctx context.Context, c QuotaClaim, apply func(ctx context.Context) error) error
}

// QuotaClaim is the consumption a mutation adds.
type QuotaClaim struct {
	// ContainerID is the project (or tenant) the resource is charged to.
	ContainerID string
	// Region is where the quota is consumed; empty means the region of the
	// ContainerID project.
	Region string
	// Amounts maps quota names (pkg/quota) to deltas. Only positive deltas
	// are checked; zero and negative ones (a shrink) always pass.
	Amounts map[string]int64
}

// QuotaUsage is the consumption of one quota by one container, as counted by
// an area from its store.
type QuotaUsage struct {
	ContainerID string // the project (or tenant) the resources are charged to
	Region      string // empty means the region of the ContainerID project
	Name        string // quota name (pkg/quota)
	Amount      uint64
}

// QuotaUsageSource reports an area's current quota consumption across every
// container. Implementations read their own store only and are safe for
// concurrent use.
type QuotaUsageSource interface {
	QuotaUsage(ctx context.Context) ([]QuotaUsage, error)
}

const quotaUsagePrefix = "quotas.usage."

// QuotaUsageKey is the key under which an area (e.g. "compute") provides its
// QuotaUsageSource. Every area that charges quotas provides one.
func QuotaUsageKey(area string) Key[QuotaUsageSource] {
	return NewKey[QuotaUsageSource](quotaUsagePrefix + area)
}

// QuotaUsageSources returns the provided usage sources by area.
func QuotaUsageSources(d *Directory) map[string]QuotaUsageSource {
	out := map[string]QuotaUsageSource{}
	if d == nil {
		return out
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	for n, v := range d.caps {
		area, ok := strings.CutPrefix(n, quotaUsagePrefix)
		if src, isSrc := v.(QuotaUsageSource); ok && isSrc {
			out[area] = src
		}
	}
	return out
}

// AdmitQuota runs c through the quota gate when the quotas module is enabled,
// or simply runs apply when it is not (no allowances exist to enforce).
func AdmitQuota(ctx context.Context, d *Directory, c QuotaClaim, apply func(ctx context.Context) error) error {
	if g, ok := Lookup(d, QuotaGateKey); ok {
		return g.Admit(ctx, c, apply)
	}
	if apply == nil {
		return nil
	}
	return apply(ctx)
}

// QuotaGateFor returns a QuotaGate that looks the quotas module up in d at
// call time (see AdmitQuota), for wiring into a domain's quota port.
func QuotaGateFor(d *Directory) QuotaGate { return lazyGate{d: d} }

type lazyGate struct{ d *Directory }

func (g lazyGate) Admit(ctx context.Context, c QuotaClaim, apply func(ctx context.Context) error) error {
	return AdmitQuota(ctx, g.d, c, apply)
}

// QuotaFunc adapts a function to QuotaUsageSource.
type QuotaFunc func(ctx context.Context) ([]QuotaUsage, error)

// QuotaUsage implements QuotaUsageSource.
func (f QuotaFunc) QuotaUsage(ctx context.Context) ([]QuotaUsage, error) { return f(ctx) }
