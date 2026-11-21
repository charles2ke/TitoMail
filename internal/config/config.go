// Package config loads TitoMail configuration from command-line flags and
// TITOMAIL_* environment variables.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Config holds the runtime configuration for all TitoMail services.
type Config struct {
	// Hostname is the public FQDN of this mail server (used in SMTP banners,
	// Received headers, Message-IDs and as the MX target).
	Hostname string
	// DataDir holds the SQLite database and ACME certificate cache.
	DataDir string
	// PublicURL is the external URL of the web interface.
	PublicURL string

	HTTPAddr  string
	HTTPSAddr string

	// SMTPAddr receives mail from the internet (MX, normally :25).
	SMTPAddr string
	// SubmissionAddr accepts authenticated mail from clients (normally :587, STARTTLS).
	SubmissionAddr string
	// SubmissionsAddr accepts authenticated mail over implicit TLS (normally :465).
	SubmissionsAddr string
	// IMAPAddr serves IMAP with STARTTLS (normally :143).
	IMAPAddr string
	// IMAPSAddr serves IMAP over implicit TLS (normally :993).
	IMAPSAddr string

	TLSCert       string
	TLSKey        string
	ACME          bool
	ACMEEmail     string
	ACMEDirectory string

	// AllowInsecureAuth permits SMTP/IMAP password authentication without TLS.
	// Only use this for local development.
	AllowInsecureAuth bool

	MaxMessageBytes int64
	MaxUploadBytes  int64

	// Outbound delivery.
	RelayHost     string
	RelayPort     int
	RelayUsername string
	RelayPassword string
	// RelayTLS is one of "starttls", "tls" or "none".
	RelayTLS string
	// OutboundPort is the TCP port used for direct-to-MX delivery (25).
	OutboundPort int
	// DisableOutbound keeps remote mail queued instead of delivering it.
	DisableOutbound bool
	// QueueMaxAge is how long delivery is retried before bouncing.
	QueueMaxAge time.Duration

	// InboundAuthChecks enables SPF, DKIM and DMARC verification for
	// messages received from the internet.
	InboundAuthChecks bool

	// AdminEmail and AdminPassword bootstrap the first administrator.
	AdminEmail    string
	AdminPassword string

	// SecureCookies forces the Secure flag on session cookies.
	SecureCookies bool
	// TrustProxy honours X-Forwarded-For / X-Forwarded-Proto headers.
	TrustProxy bool

	LogLevel string

	// envErrs lists environment variables with invalid values; Validate
	// reports them.
	envErrs []error
}

// Default returns the default configuration, suitable for local development.
func Default() Config {
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	return Config{
		Hostname:          host,
		DataDir:           "./data",
		HTTPAddr:          ":8080",
		SMTPAddr:          ":2525",
		SubmissionAddr:    ":1587",
		IMAPAddr:          ":1143",
		MaxMessageBytes:   50 << 20,
		MaxUploadBytes:    25 << 20,
		RelayPort:         587,
		RelayTLS:          "starttls",
		OutboundPort:      25,
		QueueMaxAge:       5 * 24 * time.Hour,
		InboundAuthChecks: true,
		LogLevel:          "info",
	}
}

// Load parses configuration from environment variables and the given
// command-line arguments. Flags take precedence over the environment.
func Load(args []string) (Config, error) {
	cfg := Default()
	fs := flag.NewFlagSet("titomail serve", flag.ContinueOnError)
	Register(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

// Register binds all configuration fields to flags on fs, using TITOMAIL_*
// environment variables as defaults.
func Register(fs *flag.FlagSet, cfg *Config) {
	str := func(p *string, name, env, usage string) {
		fromEnv(cfg, p, env, func(s string) (string, error) { return s, nil })
		fs.StringVar(p, name, *p, usage+" (env "+env+")")
	}
	boolean := func(p *bool, name, env, usage string) {
		fromEnv(cfg, p, env, strconv.ParseBool)
		fs.BoolVar(p, name, *p, usage+" (env "+env+")")
	}
	integer := func(p *int, name, env, usage string) {
		fromEnv(cfg, p, env, strconv.Atoi)
		fs.IntVar(p, name, *p, usage+" (env "+env+")")
	}
	int64v := func(p *int64, name, env, usage string) {
		fromEnv(cfg, p, env, func(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) })
		fs.Int64Var(p, name, *p, usage+" (env "+env+")")
	}
	duration := func(p *time.Duration, name, env, usage string) {
		fromEnv(cfg, p, env, time.ParseDuration)
		fs.DurationVar(p, name, *p, usage+" (env "+env+")")
	}

	str(&cfg.Hostname, "hostname", "TITOMAIL_HOSTNAME", "public FQDN of the mail server")
	str(&cfg.DataDir, "data-dir", "TITOMAIL_DATA_DIR", "directory for the database and certificates")
	str(&cfg.PublicURL, "public-url", "TITOMAIL_PUBLIC_URL", "external URL of the web interface")
	str(&cfg.HTTPAddr, "http-addr", "TITOMAIL_HTTP_ADDR", "HTTP listen address (empty to disable)")
	str(&cfg.HTTPSAddr, "https-addr", "TITOMAIL_HTTPS_ADDR", "HTTPS listen address (requires TLS)")
	str(&cfg.SMTPAddr, "smtp-addr", "TITOMAIL_SMTP_ADDR", "inbound SMTP (MX) listen address, normally :25")
	str(&cfg.SubmissionAddr, "submission-addr", "TITOMAIL_SUBMISSION_ADDR", "SMTP submission listen address, normally :587")
	str(&cfg.SubmissionsAddr, "submissions-addr", "TITOMAIL_SUBMISSIONS_ADDR", "SMTP submission over implicit TLS, normally :465")
	str(&cfg.IMAPAddr, "imap-addr", "TITOMAIL_IMAP_ADDR", "IMAP listen address, normally :143")
	str(&cfg.IMAPSAddr, "imaps-addr", "TITOMAIL_IMAPS_ADDR", "IMAP over implicit TLS listen address, normally :993")
	str(&cfg.TLSCert, "tls-cert", "TITOMAIL_TLS_CERT", "TLS certificate file (PEM)")
	str(&cfg.TLSKey, "tls-key", "TITOMAIL_TLS_KEY", "TLS private key file (PEM)")
	boolean(&cfg.ACME, "acme", "TITOMAIL_ACME", "obtain TLS certificates automatically from Let's Encrypt")
	str(&cfg.ACMEEmail, "acme-email", "TITOMAIL_ACME_EMAIL", "contact email for ACME")
	str(&cfg.ACMEDirectory, "acme-directory", "TITOMAIL_ACME_DIRECTORY", "ACME directory URL (default Let's Encrypt)")
	boolean(&cfg.AllowInsecureAuth, "allow-insecure-auth", "TITOMAIL_ALLOW_INSECURE_AUTH", "allow SMTP/IMAP passwords over plaintext connections (development only)")
	int64v(&cfg.MaxMessageBytes, "max-message-bytes", "TITOMAIL_MAX_MESSAGE_BYTES", "maximum accepted message size")
	int64v(&cfg.MaxUploadBytes, "max-upload-bytes", "TITOMAIL_MAX_UPLOAD_BYTES", "maximum size of attachments in a composed message")
	str(&cfg.RelayHost, "relay-host", "TITOMAIL_RELAY_HOST", "send all outbound mail through this smarthost")
	integer(&cfg.RelayPort, "relay-port", "TITOMAIL_RELAY_PORT", "smarthost port")
	str(&cfg.RelayUsername, "relay-username", "TITOMAIL_RELAY_USERNAME", "smarthost username")
	str(&cfg.RelayPassword, "relay-password", "TITOMAIL_RELAY_PASSWORD", "smarthost password")
	str(&cfg.RelayTLS, "relay-tls", "TITOMAIL_RELAY_TLS", "smarthost TLS mode: starttls, tls or none")
	integer(&cfg.OutboundPort, "outbound-port", "TITOMAIL_OUTBOUND_PORT", "TCP port for direct MX delivery")
	boolean(&cfg.DisableOutbound, "disable-outbound", "TITOMAIL_DISABLE_OUTBOUND", "keep remote mail queued instead of delivering it")
	duration(&cfg.QueueMaxAge, "queue-max-age", "TITOMAIL_QUEUE_MAX_AGE", "how long to retry outbound delivery before bouncing")
	boolean(&cfg.InboundAuthChecks, "inbound-auth-checks", "TITOMAIL_INBOUND_AUTH_CHECKS", "verify SPF/DKIM/DMARC on inbound mail")
	str(&cfg.AdminEmail, "admin-email", "TITOMAIL_ADMIN_EMAIL", "bootstrap administrator address (created on first start)")
	str(&cfg.AdminPassword, "admin-password", "TITOMAIL_ADMIN_PASSWORD", "bootstrap administrator password")
	boolean(&cfg.SecureCookies, "secure-cookies", "TITOMAIL_SECURE_COOKIES", "always set the Secure flag on cookies")
	boolean(&cfg.TrustProxy, "trust-proxy", "TITOMAIL_TRUST_PROXY", "trust X-Forwarded-* headers from a reverse proxy")
	str(&cfg.LogLevel, "log-level", "TITOMAIL_LOG_LEVEL", "log level: debug, info, warn or error")
}

// fromEnv sets *p from the environment variable env if it is set. Invalid
// values leave *p unchanged and are recorded for Validate.
func fromEnv[T any](cfg *Config, p *T, env string, parse func(string) (T, error)) {
	s, ok := os.LookupEnv(env)
	if !ok {
		return
	}
	v, err := parse(s)
	if err != nil {
		cfg.envErrs = append(cfg.envErrs, fmt.Errorf("invalid value %q for %s", s, env))
		return
	}
	*p = v
}

// Validate checks the configuration for obvious mistakes.
func (c *Config) Validate() error {
	errs := slices.Clone(c.envErrs)
	c.Hostname = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(c.Hostname)), ".")
	if c.Hostname == "" {
		errs = append(errs, errors.New("hostname must not be empty"))
	}
	if c.DataDir == "" {
		errs = append(errs, errors.New("data-dir must not be empty"))
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("tls-cert and tls-key must be set together"))
	}
	if c.ACME && c.TLSCert != "" {
		errs = append(errs, errors.New("acme and tls-cert are mutually exclusive"))
	}
	if !c.TLSEnabled() && (c.SubmissionsAddr != "" || c.IMAPSAddr != "" || c.HTTPSAddr != "") {
		errs = append(errs, errors.New("submissions-addr, imaps-addr and https-addr require TLS (tls-cert/tls-key or acme)"))
	}
	switch c.RelayTLS {
	case "starttls", "tls", "none":
	default:
		errs = append(errs, fmt.Errorf("relay-tls must be starttls, tls or none (got %q)", c.RelayTLS))
	}
	if c.MaxMessageBytes <= 0 {
		errs = append(errs, errors.New("max-message-bytes must be positive"))
	}
	if c.MaxUploadBytes <= 0 || c.MaxUploadBytes > c.MaxMessageBytes {
		errs = append(errs, errors.New("max-upload-bytes must be positive and not exceed max-message-bytes"))
	}
	if c.PublicURL == "" {
		switch {
		case c.HTTPSAddr != "":
			c.PublicURL = "https://" + c.Hostname + portSuffix(c.HTTPSAddr, "443")
		default:
			c.PublicURL = "http://" + c.Hostname + portSuffix(c.HTTPAddr, "80")
		}
	}
	c.PublicURL = strings.TrimSuffix(c.PublicURL, "/")
	return errors.Join(errs...)
}

// TLSEnabled reports whether a TLS certificate source is configured.
func (c *Config) TLSEnabled() bool {
	return c.ACME || c.TLSCert != ""
}

func portSuffix(addr, defaultPort string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return ""
	}
	port := addr[i+1:]
	if port == "" || port == defaultPort {
		return ""
	}
	return ":" + port
}
