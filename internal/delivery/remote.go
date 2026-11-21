package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/charles2ke/TitoMail/internal/email"
	"github.com/charles2ke/TitoMail/internal/store"
)

const (
	maxMXHosts    = 5
	maxAddrsPerMX = 2
	connTimeout   = 10 * time.Minute
)

func (d *Deliverer) deliverRemoteBatch(ctx context.Context, b *store.QueueBatch) map[string]*failure {
	rcpts := make([]string, len(b.Rcpts))
	for i, r := range b.Rcpts {
		rcpts[i] = r.Rcpt
	}
	all := func(f *failure) map[string]*failure {
		out := make(map[string]*failure, len(rcpts))
		for _, r := range rcpts {
			out[r] = f
		}
		return out
	}
	if d.opts.DisableOutbound && d.opts.Relay == nil {
		return all(tempFail("outbound delivery is disabled on this server"))
	}
	data := d.signDKIM(ctx, b.Entry.Data, b.Entry.MailFrom)

	if d.opts.Relay != nil {
		r := d.opts.Relay
		addr := net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
		out, err := d.smtpTransaction(ctx, r.Host, addr, transportOpts{relay: r}, b.Entry.MailFrom, rcpts, data)
		if err != nil {
			return all(err)
		}
		return out
	}

	hosts, ferr := d.lookupMX(ctx, b.Domain)
	if ferr != nil {
		return all(ferr)
	}
	var last *failure
	for _, host := range hosts {
		ips, err := d.res.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			last = tempFail("cannot resolve MX host %s: %v", host, err)
			continue
		}
		// Prefer IPv4: many hosts lack IPv6 connectivity or reverse DNS.
		sort.SliceStable(ips, func(i, j int) bool { return ips[i].IP.To4() != nil && ips[j].IP.To4() == nil })
		if len(ips) > maxAddrsPerMX {
			ips = ips[:maxAddrsPerMX]
		}
		for _, ip := range ips {
			addr := net.JoinHostPort(ip.IP.String(), strconv.Itoa(d.opts.OutboundPort))
			out, err := d.smtpTransaction(ctx, host, addr, transportOpts{}, b.Entry.MailFrom, rcpts, data)
			if err == nil {
				return out
			}
			if err.permanent {
				return all(err)
			}
			last = err
		}
	}
	if last == nil {
		last = tempFail("no usable mail exchanger for %s", b.Domain)
	}
	return all(last)
}

// lookupMX returns the mail exchangers for domain in preference order.
func (d *Deliverer) lookupMX(ctx context.Context, domain string) ([]string, *failure) {
	mxs, err := d.res.LookupMX(ctx, domain)
	var dnsErr *net.DNSError
	if err != nil && !(errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
		return nil, tempFail("MX lookup for %s failed: %v", domain, err)
	}
	if len(mxs) == 0 {
		// RFC 5321 section 5.1: fall back to the domain's address records.
		ips, err := d.res.LookupIPAddr(ctx, domain)
		if err != nil {
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				return nil, permFail("550 5.1.2 domain %s does not exist or has no mail server", domain)
			}
			return nil, tempFail("address lookup for %s failed: %v", domain, err)
		}
		if len(ips) == 0 {
			return nil, permFail("550 5.1.2 domain %s has no mail server", domain)
		}
		return []string{domain}, nil
	}
	if len(mxs) == 1 && strings.Trim(mxs[0].Host, ".") == "" {
		return nil, permFail("556 5.1.10 domain %s does not accept mail (null MX)", domain)
	}
	sort.SliceStable(mxs, func(i, j int) bool { return mxs[i].Pref < mxs[j].Pref })
	var hosts []string
	for _, mx := range mxs {
		h := strings.TrimSuffix(mx.Host, ".")
		if h == "" {
			continue
		}
		hosts = append(hosts, h)
		if len(hosts) == maxMXHosts {
			break
		}
	}
	return hosts, nil
}

type transportOpts struct {
	relay *Relay
	// noTLS disables opportunistic STARTTLS (used to retry after a failed
	// handshake).
	noTLS bool
}

// smtpFailure converts an SMTP client error into a failure.
func smtpFailure(stage string, err error) *failure {
	var te *textproto.Error
	if errors.As(err, &te) {
		msg := fmt.Sprintf("%d %s", te.Code, strings.ReplaceAll(te.Msg, "\n", " "))
		if te.Code >= 500 {
			return permFail("%s", msg)
		}
		return tempFail("%s", msg)
	}
	return tempFail("%s: %v", stage, err)
}

// smtpTransaction delivers data to rcpts over one SMTP connection. A nil
// error means a transaction took place and the per-recipient outcome is in
// the map (absent = delivered). A non-nil error means no transaction could
// be completed with this server; permanent errors must not be retried with
// other servers.
func (d *Deliverer) smtpTransaction(ctx context.Context, host, addr string, opts transportOpts, from string, rcpts []string, data []byte) (map[string]*failure, *failure) {
	conn, err := d.opts.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, tempFail("connecting to %s (%s) failed: %v", host, addr, err)
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < connTimeout {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(connTimeout))
	}
	relay := opts.relay
	if relay != nil && relay.TLS == "tls" {
		cfg := d.relayTLSConfig(relay.Host)
		tc := tls.Client(conn, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, tempFail("TLS handshake with relay %s failed: %v", host, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return nil, smtpConnFailure(host, "greeting", err)
	}
	defer c.Close()
	if err := c.Hello(d.opts.Hostname); err != nil {
		return nil, smtpConnFailure(host, "EHLO", err)
	}
	if ok, _ := c.Extension("STARTTLS"); ok && !opts.noTLS && !(relay != nil && relay.TLS != "starttls") {
		var cfg *tls.Config
		if relay != nil {
			cfg = d.relayTLSConfig(relay.Host)
		} else {
			// Opportunistic TLS: encrypt even if the certificate can't be
			// verified, as most MTAs do (no MTA-STS/DANE support yet).
			cfg = &tls.Config{ServerName: host, InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec
		}
		if err := c.StartTLS(cfg); err != nil {
			if relay == nil {
				d.log.Info("STARTTLS failed, retrying without TLS", "host", host, "err", err)
				c.Close()
				opts.noTLS = true
				return d.smtpTransaction(ctx, host, addr, opts, from, rcpts, data)
			}
			return nil, tempFail("STARTTLS with relay %s failed: %v", host, err)
		}
	} else if relay != nil && relay.TLS == "starttls" {
		return nil, tempFail("relay %s does not support STARTTLS", host)
	}
	if relay != nil && relay.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", relay.Username, relay.Password, host)); err != nil {
			return nil, smtpConnFailure(host, "authentication", err)
		}
	}
	if err := c.Mail(from); err != nil {
		// A 5xx reply to MAIL FROM is a policy decision for the whole
		// message; 4xx lets the caller try the next MX host.
		return nil, smtpFailure("MAIL FROM", err)
	}
	out := map[string]*failure{}
	var accepted []string
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			out[r] = smtpFailure("RCPT TO", err)
			continue
		}
		accepted = append(accepted, r)
	}
	if len(accepted) == 0 {
		_ = c.Quit()
		return out, nil
	}
	fail := func(f *failure) (map[string]*failure, *failure) {
		for _, r := range accepted {
			out[r] = f
		}
		return out, nil
	}
	w, err := c.Data()
	if err != nil {
		return fail(smtpFailure("DATA", err))
	}
	if _, err := w.Write(data); err != nil {
		return fail(tempFail("sending message data to %s failed: %v", host, err))
	}
	if err := w.Close(); err != nil {
		return fail(smtpFailure("end of DATA", err))
	}
	_ = c.Quit()
	return out, nil
}

// smtpConnFailure classifies errors before the mail transaction. They are
// always treated as temporary so that other MX hosts are tried.
func smtpConnFailure(host, stage string, err error) *failure {
	f := smtpFailure(stage, err)
	return tempFail("%s: %s failed: %s", host, stage, f.msg)
}

func (d *Deliverer) relayTLSConfig(host string) *tls.Config {
	if d.opts.RelayTLSConfig != nil {
		cfg := d.opts.RelayTLSConfig.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName = host
		}
		return cfg
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// dkimHeaders are the header fields covered by our signatures (RFC 6376
// section 5.4.1).
var dkimHeaders = []string{
	"From", "Reply-To", "Subject", "Date", "To", "Cc", "Message-ID", "In-Reply-To", "References",
	"MIME-Version", "Content-Type", "Content-Transfer-Encoding", "List-Unsubscribe",
}

// signDKIM signs raw with the key of the domain in the From header (or the
// envelope sender) if it is a local domain. Unsigned data is returned when
// signing is not possible.
func (d *Deliverer) signDKIM(ctx context.Context, raw []byte, mailFrom string) []byte {
	p := email.Parse(raw)
	domain := email.DomainOf(p.PrimaryFrom().Address)
	if domain == "" {
		domain = email.DomainOf(mailFrom)
	}
	dom, err := d.st.DomainByName(ctx, domain)
	if err != nil {
		return raw
	}
	signed, err := SignDKIM(dom, raw)
	if err != nil {
		d.log.Warn("DKIM signing failed", "domain", domain, "err", err)
		return raw
	}
	return signed
}

// SignDKIM returns raw with a DKIM-Signature for domain prepended.
func SignDKIM(dom *store.Domain, raw []byte) ([]byte, error) {
	key, err := dom.DKIMSigner()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = dkim.Sign(&buf, bytes.NewReader(raw), &dkim.SignOptions{
		Domain:                 dom.Name,
		Selector:               dom.DKIMSelector,
		Signer:                 key,
		HeaderCanonicalization: dkim.CanonicalizationRelaxed,
		BodyCanonicalization:   dkim.CanonicalizationRelaxed,
		HeaderKeys:             dkimHeaders,
	})
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
