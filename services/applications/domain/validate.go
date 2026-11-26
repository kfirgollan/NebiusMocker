package domain

import (
	"fmt"
	"regexp"

	"go.yaml.in/yaml/v3"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// dns1123Label is a Kubernetes namespace / DNS-1123 label.
var dns1123Label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

const maxLabelLen = 63

// checkLabel requires v to be a DNS-1123 label.
func checkLabel(field, v string) error {
	if len(v) > maxLabelLen || !dns1123Label.MatchString(v) {
		return errs.BadRequest(errs.Violation{
			Field:   field,
			Message: fmt.Sprintf("%q must be a DNS-1123 label: at most %d lowercase alphanumeric characters or '-', starting and ending with an alphanumeric character", v, maxLabelLen),
		})
	}
	return nil
}

// validateValues checks the Helm values (YAML) and --set overrides. The YAML
// error is not echoed back: values are sensitive.
func validateValues(values string, set map[string]string) error {
	if values != "" {
		var doc any
		if err := yaml.Unmarshal([]byte(values), &doc); err != nil {
			return errs.BadRequest(errs.Violation{Field: "spec.values", Message: "must be valid YAML"})
		}
	}
	for k := range set {
		if k == "" {
			return errs.BadRequest(errs.Violation{Field: "spec.set", Message: "keys must not be empty"})
		}
	}
	return nil
}
