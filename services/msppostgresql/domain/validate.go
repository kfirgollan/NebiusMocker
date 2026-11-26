package domain

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"google.golang.org/protobuf/reflect/protoreflect"

	pgpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1"
	pgconfigpb "github.com/nebius/gosdk/proto/nebius/msp/postgresql/v1alpha1/config"
	resourcepb "github.com/nebius/gosdk/proto/nebius/msp/v1alpha1/resource"

	"github.com/kfirgollan/NebiusMocker/pkg/config"
	"github.com/kfirgollan/NebiusMocker/pkg/errs"
)

// Spec limits and internal defaults. The defaults are never written into the
// stored spec (the Terraform schema marks those fields Optional only).
const (
	minDiskGiB = 10
	maxDiskGiB = 4096

	defaultPoolMode    = "session"
	defaultMaxPoolSize = 20

	defaultBackupWindow  = 2 * time.Minute // "00:02:00"
	defaultRetentionDays = 7
	minRetentionDays     = 7
	maxRetentionDays     = 30

	// Day is the length of a backup retention day.
	day = 24 * time.Hour
)

var retentionRE = regexp.MustCompile(`^(\d+)d$`)

const specPrefix = "spec."

func notEmulated(field, what string) errs.Violation {
	return errs.Violation{Field: field, Message: what + " is not emulated by NebiusMocker"}
}

func violations(v []errs.Violation) error {
	if len(v) == 0 {
		return nil
	}
	return errs.BadRequest(v...)
}

// checkSpec validates a cluster spec against the catalogs.
func (s *Service) checkSpec(spec *pgpb.ClusterSpec) error { return violations(s.specViolations(spec)) }

// checkNewSpec is checkSpec for Create and Restore, where
// bootstrap.user_password is mandatory.
func (s *Service) checkNewSpec(spec *pgpb.ClusterSpec) error {
	v := s.specViolations(spec)
	if spec.GetBootstrap().GetUserPassword() == "" {
		v = append(v, errs.Violation{Field: "spec.bootstrap.user_password", Message: "spec.bootstrap.user_password is required"})
	}
	return violations(v)
}

func (s *Service) specViolations(spec *pgpb.ClusterSpec) []errs.Violation {
	cfg := spec.GetConfig()
	v := s.checkConfig(cfg)
	v = append(v, s.checkTemplate(cfg.GetTemplate())...)
	_, _, bv := backupSettings(spec.GetBackup())
	v = append(v, bv...)
	if cfg.GetPostgresqlConfig_16() != nil {
		_, pv := postgresParameters(cfg.GetPostgresqlConfig_16())
		v = append(v, pv...)
	}
	return v
}

func (s *Service) checkConfig(cfg *pgpb.ConfigSpec) []errs.Violation {
	var v []errs.Violation
	if cfg == nil {
		return []errs.Violation{{Field: "spec.config", Message: "required"}}
	}
	if _, ok := s.d.Config.MSP.PostgresImages[cfg.GetVersion()]; !ok {
		v = append(v, errs.Violation{Field: "spec.config.version", Message: fmt.Sprintf(
			"unsupported version %q: want one of %v", cfg.GetVersion(), slices.Sorted(maps.Keys(s.d.Config.MSP.PostgresImages)))})
	}
	return v
}

func (s *Service) checkTemplate(t *pgpb.TemplateSpec) []errs.Violation {
	const p = "spec.config.template."
	var v []errs.Violation
	msp := s.d.Config.MSP
	platform, preset := t.GetResources().GetPlatform(), t.GetResources().GetPreset()
	presets, ok := msp.PostgresPresets[platform]
	switch {
	case !ok:
		v = append(v, errs.Violation{Field: p + "resources.platform", Message: fmt.Sprintf(
			"unknown platform %q: want one of %v", platform, slices.Sorted(maps.Keys(msp.PostgresPresets)))})
	case !slices.Contains(presets, preset):
		v = append(v, errs.Violation{Field: p + "resources.preset", Message: fmt.Sprintf(
			"unknown preset %q for platform %q: want one of %v", preset, platform, presets)})
	}
	if dt := t.GetDisk().GetType(); !slices.Contains(msp.PostgresDiskTypes, dt) {
		v = append(v, errs.Violation{Field: p + "disk.type", Message: fmt.Sprintf("unknown disk type %q: want one of %v", dt, msp.PostgresDiskTypes)})
	}
	if n := t.GetDisk().GetSizeGibibytes(); n < minDiskGiB || n > maxDiskGiB {
		v = append(v, errs.Violation{Field: p + "disk.size_gibibytes", Message: fmt.Sprintf("must be between %d and %d", minDiskGiB, maxDiskGiB)})
	}
	if n := t.GetHosts().GetCount(); n < 1 || n > int64(msp.PostgresMaxHosts) {
		v = append(v, errs.Violation{Field: p + "hosts.count", Message: fmt.Sprintf("must be between 1 and %d", msp.PostgresMaxHosts)})
	}
	return v
}

// checkUpdate rejects changes of IMMUTABLE fields and a shrinking disk.
// prev and next are the stored and the patched spec.
func checkUpdate(prev, next *pgpb.ClusterSpec) error {
	var v []errs.Violation
	immutable := func(field string, changed bool) {
		if changed {
			v = append(v, errs.Violation{Field: field, Message: "field is immutable"})
		}
	}
	immutable(specPrefix+"description", prev.GetDescription() != next.GetDescription())
	immutable(specPrefix+"network_id", prev.GetNetworkId() != next.GetNetworkId())
	immutable("spec.config.version", prev.GetConfig().GetVersion() != next.GetConfig().GetVersion())
	immutable("spec.bootstrap.user_name", prev.GetBootstrap().GetUserName() != next.GetBootstrap().GetUserName())
	immutable("spec.bootstrap.db_name", prev.GetBootstrap().GetDbName() != next.GetBootstrap().GetDbName())
	immutable("spec.backup.backup_window_start", prev.GetBackup().GetBackupWindowStart() != next.GetBackup().GetBackupWindowStart())
	immutable("spec.backup.retention_policy", prev.GetBackup().GetRetentionPolicy() != next.GetBackup().GetRetentionPolicy())
	if p, n := prev.GetConfig().GetTemplate().GetDisk().GetSizeGibibytes(), next.GetConfig().GetTemplate().GetDisk().GetSizeGibibytes(); n < p {
		v = append(v, errs.Violation{Field: "spec.config.template.disk.size_gibibytes", Message: fmt.Sprintf(
			"the disk cannot shrink: %d GiB is less than the current %d GiB", n, p)})
	}
	return violations(v)
}

// checkRestore validates a restore against its source cluster spec.
func checkRestore(src, next *pgpb.ClusterSpec) error {
	var v []errs.Violation
	if sv, nv := src.GetConfig().GetVersion(), next.GetConfig().GetVersion(); sv != nv {
		v = append(v, errs.Violation{Field: "spec.config.version", Message: fmt.Sprintf(
			"must equal the version of the source cluster (%s)", sv)})
	}
	if sd, nd := src.GetConfig().GetTemplate().GetDisk().GetSizeGibibytes(), next.GetConfig().GetTemplate().GetDisk().GetSizeGibibytes(); nd < sd {
		v = append(v, errs.Violation{Field: "spec.config.template.disk.size_gibibytes", Message: fmt.Sprintf(
			"must be at least the source cluster's disk (%d GiB)", sd)})
	}
	return violations(v)
}

// backupSettings returns the backup window offset (from 00:00 UTC) and the
// retention in days, with the internal defaults for what is not set.
func backupSettings(b *pgpb.BackupSpec) (window time.Duration, retentionDays int, v []errs.Violation) {
	window, retentionDays = defaultBackupWindow, defaultRetentionDays
	const p = "spec.backup."
	if len(b.GetReplicationRegions()) > 0 {
		v = append(v, notEmulated(p+"replication_regions", "cross-region backup replication"))
	}
	if b.GetReplicationInterval() != nil {
		v = append(v, notEmulated(p+"replication_interval", "cross-region backup replication"))
	}
	if w := b.GetBackupWindowStart(); w != "" {
		t, err := time.Parse(time.TimeOnly, w)
		if err != nil {
			v = append(v, errs.Violation{Field: p + "backup_window_start", Message: fmt.Sprintf("%q is not a time of day, want HH:MM:SS", w)})
		} else {
			window = time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second
		}
	}
	if r := b.GetRetentionPolicy(); r != "" {
		m := retentionRE.FindStringSubmatch(r)
		n, err := 0, error(nil)
		if m != nil {
			n, err = strconv.Atoi(m[1])
		}
		if m == nil || err != nil || n < minRetentionDays || n > maxRetentionDays {
			v = append(v, errs.Violation{Field: p + "retention_policy", Message: fmt.Sprintf(
				"%q is not valid: want <N>d with N between %d and %d", r, minRetentionDays, maxRetentionDays)})
		} else {
			retentionDays = n
		}
	}
	return window, retentionDays, v
}

// pooler returns the connection pooler settings with defaults applied.
func pooler(c *pgpb.ConnectionPoolerConfig) Pooler {
	p := Pooler{Mode: defaultPoolMode, MaxPoolSize: defaultMaxPoolSize}
	if c.GetPoolingMode() == pgpb.ConnectionPoolerConfig_TRANSACTION {
		p.Mode = "transaction"
	}
	if c != nil && c.MaxPoolSize != nil {
		p.MaxPoolSize = c.GetMaxPoolSize()
	}
	return p
}

// parameterUnits are the postgresql.conf units of the integer settings of
// spec.config.postgresql_config_16 (the proto documents them in its comments).
var parameterUnits = map[protoreflect.Name]string{
	"autovacuum_work_mem":                 "kB",
	"shared_buffers":                      "kB",
	"statement_timeout":                   "ms",
	"idle_in_transaction_session_timeout": "ms",
	"autovacuum_vacuum_cost_delay":        "ms",
	"autovacuum_naptime":                  "s",
}

var onOff = map[bool]string{true: "on", false: "off"}

// postgresParameters renders the set fields of spec.config.postgresql_config_16
// as postgresql.conf settings: the field name is the parameter name, integers
// carry their unit (-1 stays "-1"), booleans are on/off.
func postgresParameters(c *pgconfigpb.PostgresqlConfig16) (map[string]string, []errs.Violation) {
	out := map[string]string{}
	var v []errs.Violation
	c.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, val protoreflect.Value) bool {
		name := string(fd.Name())
		switch fd.Kind() { //nolint:exhaustive // the config has int64, double, bool and string settings; any other kind is rejected by default
		case protoreflect.Int64Kind:
			if n := val.Int(); n == -1 {
				out[name] = "-1"
			} else {
				out[name] = strconv.FormatInt(n, 10) + parameterUnits[fd.Name()]
			}
		case protoreflect.DoubleKind:
			out[name] = strconv.FormatFloat(val.Float(), 'g', -1, 64)
		case protoreflect.BoolKind:
			out[name] = onOff[val.Bool()]
		case protoreflect.StringKind:
			if strings.ContainsFunc(val.String(), unicode.IsControl) {
				v = append(v, errs.Violation{Field: "spec.config.postgresql_config_16." + name, Message: "must not contain control characters"})
			}
			out[name] = val.String()
		default:
			v = append(v, notEmulated("spec.config.postgresql_config_16."+name, "this setting"))
		}
		return true
	})
	return out, v
}

// presetDetails is status.preset_details of a preset ("<n>vcpu-<m>gb").
func presetDetails(preset string) *resourcepb.PresetDetails {
	cpus, mem, _ := config.ParsePreset(preset)
	return &resourcepb.PresetDetails{CpuCount: int32(cpus), MemoryGibibytes: mem} //nolint:gosec // G115: a preset has a handful of vCPUs
}
