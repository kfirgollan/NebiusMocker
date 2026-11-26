package config

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// ApplicationsConfig configures Nebius Applications
// (NEBIUS_MOCKER_APPLICATIONS_*).
type ApplicationsConfig struct {
	// Products is the Marketplace product catalog: the product slugs a K8s
	// release may install (NEBIUS_MOCKER_APPLICATIONS_PRODUCTS,
	// comma-separated).
	Products []string
}

func defaultApplications() ApplicationsConfig {
	return ApplicationsConfig{Products: []string{
		"nebius/soperator", "nebius/nvidia-gpu-operator", "nebius/nvidia-network-operator",
		"nebius/kuberay", "nebius/jupyterhub", "nebius/mlflow",
	}}
}

func applicationsParsers(a *ApplicationsConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"APPLICATIONS_PRODUCTS", func(s string) error { return parseProductSlugs(s, &a.Products) }},
	}
}

var productSlugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$`)

func parseProductSlugs(s string, out *[]string) error {
	var slugs []string
	for v := range strings.SplitSeq(s, ",") {
		v = strings.TrimSpace(v)
		if !productSlugRE.MatchString(v) {
			return fmt.Errorf("want comma-separated product slugs such as nebius/mlflow, got %q", v)
		}
		if slices.Contains(slugs, v) {
			return fmt.Errorf("product %s listed twice", v)
		}
		slugs = append(slugs, v)
	}
	*out = slugs
	return nil
}
