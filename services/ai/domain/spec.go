package domain

import (
	"fmt"
	"path"
	"strings"

	aipb "github.com/nebius/gosdk/proto/nebius/ai/v1"

	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// The edge validator already enforces the proto rules (required fields,
// ranges, the CEL rules on EndpointSpec and CreateEndpointRequest). The checks
// here are the documented rules the protos only describe in comments.

// validateSpec checks the static rules of a create spec and resolves its
// hardware. It returns every violation it finds at once.
func validateSpec(spec *aipb.EndpointSpec, catalog Catalog) (hardware, error) {
	var v []errs.Violation
	add := func(field, msg string) { v = append(v, errs.Violation{Field: field, Message: msg}) }

	if strings.TrimSpace(spec.GetImage()) == "" {
		add("spec.image", "required")
	}
	validateAuth(spec, add)
	validatePorts(spec, add)
	validateEnv(spec, add)
	validateVolumes(spec, add)
	validateInjectedFiles(spec, add)
	validateRegistry(spec, add)
	for i, k := range spec.GetSshAuthorizedKeys() {
		if strings.TrimSpace(k) == "" {
			add(fmt.Sprintf("spec.ssh_authorized_keys[%d]", i), "must not be empty")
		}
	}
	if spec.GetDisk().GetSizeBytes() < 0 {
		add("spec.disk.size_bytes", "must not be negative")
	}
	if len(v) > 0 {
		return hardware{}, errs.BadRequest(v...)
	}

	hw, err := resolveHardware(spec, catalog)
	if err != nil {
		return hardware{}, err
	}
	if size := spec.GetDisk().GetSizeBytes(); size != 0 && size < hw.minDiskSize() {
		return hardware{}, errs.BadRequest(errs.Violation{
			Field:   "spec.disk.size_bytes",
			Message: fmt.Sprintf("must be at least %d GiB on platform %s", hw.minDiskSize()/gib, hw.platform.Name),
		})
	}
	return hw, nil
}

func resolveHardware(spec *aipb.EndpointSpec, catalog Catalog) (hardware, error) {
	switch {
	case len(spec.GetGpu()) > 0:
		return catalog.resolveGPU(spec.GetGpu()[0])
	case len(spec.GetCpu()) > 0:
		return catalog.resolveCPU(spec.GetCpu()[0])
	default:
		return catalog.resolveExplicit(spec.GetPlatform(), spec.GetPreset())
	}
}

// authEnabled reports whether the spec configures endpoint authentication.
func authEnabled(spec *aipb.EndpointSpec) bool {
	return spec.GetAuthToken() != "" || spec.GetAuthTokenMysteryboxSecret() != nil
}

func validateAuth(spec *aipb.EndpointSpec, add func(field, msg string)) {
	if spec.GetAuthToken() != "" && spec.GetAuthTokenMysteryboxSecret() != nil {
		add("spec.auth_token", "mutually exclusive with auth_token_mysterybox_secret")
		add("spec.auth_token_mysterybox_secret", "mutually exclusive with auth_token")
	}
	if authEnabled(spec) && len(httpPorts(spec)) != 1 {
		add("spec.ports", "authentication can only be enabled if the endpoint exposes exactly one HTTP port")
	}
}

// httpPorts returns the HTTP ports of the spec, in order.
func httpPorts(spec *aipb.EndpointSpec) []*aipb.EndpointSpec_Port {
	var out []*aipb.EndpointSpec_Port
	for _, p := range spec.GetPorts() {
		if p.GetProtocol() == aipb.EndpointSpec_Port_HTTP {
			out = append(out, p)
		}
	}
	return out
}

func validatePorts(spec *aipb.EndpointSpec, add func(field, msg string)) {
	seen := map[string]bool{}
	for i, p := range spec.GetPorts() {
		if p.GetProtocol() == aipb.EndpointSpec_Port_UDP {
			// Nebius documents no address format for UDP ports, so the mock
			// cannot produce what the cloud would.
			add(fmt.Sprintf("spec.ports[%d].protocol", i), "UDP ports are not supported by NebiusMocker")
		}
		host := p.GetHostPort()
		if host == 0 {
			host = p.GetContainerPort()
		}
		key := fmt.Sprintf("%d/%s", host, p.GetProtocol())
		if seen[key] {
			add(fmt.Sprintf("spec.ports[%d]", i), "duplicate port "+key)
		}
		seen[key] = true
	}
}

func validateEnv(spec *aipb.EndpointSpec, add func(field, msg string)) {
	seen := map[string]bool{}
	for i, e := range spec.GetEnvironmentVariables() {
		field := fmt.Sprintf("spec.environment_variables[%d]", i)
		if e.GetValue() != "" && e.GetMysteryboxSecret() != nil {
			add(field, "value and mysterybox_secret are mutually exclusive")
		}
		if seen[e.GetName()] {
			add(field+".name", "duplicate variable "+e.GetName())
		}
		seen[e.GetName()] = true
	}
}

func validateVolumes(spec *aipb.EndpointSpec, add func(field, msg string)) {
	seen := map[string]bool{}
	for i, vol := range spec.GetVolumes() {
		field := fmt.Sprintf("spec.volumes[%d]", i)
		if vol.GetSource() == "" {
			add(field+".source", "required")
		}
		cp := vol.GetContainerPath()
		switch {
		case cp == "":
			add(field+".container_path", "required")
		case !path.IsAbs(cp):
			add(field+".container_path", "must be an absolute path")
		case seen[path.Clean(cp)]:
			add(field+".container_path", "duplicate mount path "+cp)
		}
		seen[path.Clean(cp)] = true
		if s3 := vol.GetS3Config(); s3 != nil {
			if !strings.HasPrefix(vol.GetSource(), "s3://") {
				add(field+".source", "must be an s3:// URI when s3_config is set")
			}
			if s3.GetCredentials() != nil && s3.GetMysteryboxSecret() != nil {
				add(field+".s3_config", "credentials and mysterybox_secret are mutually exclusive")
			}
		}
	}
}

func validateInjectedFiles(spec *aipb.EndpointSpec, add func(field, msg string)) {
	seen := map[string]bool{}
	for i, f := range spec.GetInjectedFiles() {
		field := fmt.Sprintf("spec.injected_files[%d]", i)
		if len(f.GetContent()) == 0 {
			add(field+".content", "required on create")
		}
		if seen[f.GetContainerPath()] {
			add(field+".container_path", "duplicate path "+f.GetContainerPath())
		}
		seen[f.GetContainerPath()] = true
	}
}

func validateRegistry(spec *aipb.EndpointSpec, add func(field, msg string)) {
	rc := spec.GetRegistryCredentials()
	if rc == nil {
		return
	}
	inline := rc.GetUsername() != "" || rc.GetPassword() != ""
	if inline && rc.GetMysteryboxSecretVersion() != "" {
		add("spec.registry_credentials", "username/password and mysterybox_secret_version are mutually exclusive")
	}
	if rc.GetPassword() != "" && rc.GetUsername() == "" {
		add("spec.registry_credentials.username", "required with password")
	}
}
