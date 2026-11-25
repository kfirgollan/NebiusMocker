package config

// TLSConfig enables TLS on the mocker's listeners (NEBIUS_MOCKER_TLS_*): the
// gRPC edge and module endpoints such as the registry's docker endpoint.
// Both files empty (the default) means plaintext. TLS is what clients without
// an "insecure" setting need, e.g. the docker credential helper of the nebius
// CLI, whose profile cannot store --insecure.
type TLSConfig struct {
	// CertFile is a PEM certificate chain whose SANs cover the names clients
	// dial (e.g. cpl.iam.api.nebius.cloud, cr.eu-north1.nebius.cloud).
	CertFile string
	// KeyFile is the PEM private key of CertFile.
	KeyFile string
}

// Enabled reports whether a certificate is configured.
func (t TLSConfig) Enabled() bool { return t.CertFile != "" || t.KeyFile != "" }

func tlsParsers(t *TLSConfig) []struct {
	name  string
	apply func(string) error
} {
	return []struct {
		name  string
		apply func(string) error
	}{
		{"TLS_CERT_FILE", func(v string) error { t.CertFile = v; return nil }},
		{"TLS_KEY_FILE", func(v string) error { t.KeyFile = v; return nil }},
	}
}
