package smtpd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"

	"blitiri.com.ar/go/spf"
	"github.com/emersion/go-msgauth/authres"
	"github.com/emersion/go-msgauth/dkim"
	"github.com/emersion/go-msgauth/dmarc"
	"golang.org/x/net/publicsuffix"

	"github.com/charles2ke/TitoMail/internal/email"
)

// authVerdict is the outcome of SPF, DKIM and DMARC checks on an inbound
// message.
type authVerdict struct {
	spf     spf.Result
	dkim    []*dkim.Verification
	dmarc   authres.ResultValue
	policy  dmarc.Policy
	from    string
	results []authres.Result
	// reject is set when the sender's DMARC policy asks for rejection.
	reject bool
	// spam is set when the message should go to the Junk folder.
	spam   bool
	reason string
}

// header returns the Authentication-Results field for the verdict, folded
// with one result per line.
func (v *authVerdict) header(hostname string) string {
	parts := strings.Split(authres.Format(hostname, v.results), ";")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
		if i < len(parts)-1 {
			parts[i] += ";"
		}
	}
	return foldHeader("Authentication-Results", parts...)
}

func spfValue(r spf.Result) authres.ResultValue {
	switch r {
	case spf.Pass:
		return authres.ResultPass
	case spf.Fail:
		return authres.ResultFail
	case spf.SoftFail:
		return authres.ResultSoftFail
	case spf.Neutral:
		return authres.ResultNeutral
	case spf.TempError:
		return authres.ResultTempError
	case spf.PermError:
		return authres.ResultPermError
	}
	return authres.ResultNone
}

// orgDomain returns the organizational domain (e.g. example.co.uk for
// mail.example.co.uk) used for relaxed DMARC alignment.
func orgDomain(domain string) string {
	domain = strings.ToLower(strings.TrimSuffix(domain, "."))
	if d, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		return d
	}
	return domain
}

func aligned(a, b string, mode dmarc.AlignmentMode) bool {
	if a == "" || b == "" {
		return false
	}
	if mode == dmarc.AlignmentStrict {
		return strings.EqualFold(a, b)
	}
	return orgDomain(a) == orgDomain(b)
}

// checkAuth evaluates SPF for the envelope sender, DKIM signatures and the
// DMARC policy of the From domain. localFrom reports whether the From
// domain is hosted here, in which case unauthenticated mail is treated as
// spoofed even without a DMARC record.
func (s *Server) checkAuth(ctx context.Context, ip net.IP, helo, mailFrom string, raw []byte, p *email.Message, localFrom bool) *authVerdict {
	v := &authVerdict{dmarc: authres.ResultNone}

	spfDomain := email.DomainOf(mailFrom)
	spfFrom := mailFrom
	if spfDomain == "" {
		spfDomain = helo
		spfFrom = "postmaster@" + helo
	}
	v.spf, _ = spf.CheckHostWithSender(ip, helo, mailFrom, spf.WithContext(ctx), spf.WithResolver(s.res))
	v.results = append(v.results, &authres.SPFResult{Value: spfValue(v.spf), From: spfFrom, Helo: helo})

	lookupTXT := func(domain string) ([]string, error) { return s.res.LookupTXT(ctx, domain) }
	verifs, err := dkim.VerifyWithOptions(bytes.NewReader(raw), &dkim.VerifyOptions{LookupTXT: lookupTXT, MaxVerifications: 5})
	if err == nil {
		v.dkim = verifs
	}
	dkimPass := false
	for _, d := range v.dkim {
		var val authres.ResultValue = authres.ResultPass
		switch {
		case d.Err == nil:
			dkimPass = true
		case dkim.IsTempFail(d.Err):
			val = authres.ResultTempError
		case dkim.IsPermFail(d.Err):
			val = authres.ResultPermError
		default:
			val = authres.ResultFail
		}
		v.results = append(v.results, &authres.DKIMResult{Value: val, Domain: d.Domain, Identifier: d.Identifier})
	}
	if len(v.dkim) == 0 {
		v.results = append(v.results, &authres.DKIMResult{Value: authres.ResultNone})
	}

	v.from = email.DomainOf(p.PrimaryFrom().Address)
	if v.from == "" {
		v.spam, v.reason = true, "no From address"
		v.results = append(v.results, &authres.DMARCResult{Value: authres.ResultPermError, Reason: "no From domain"})
		return v
	}

	spfAligned := func(mode dmarc.AlignmentMode) bool {
		return v.spf == spf.Pass && aligned(spfDomain, v.from, mode)
	}
	dkimAligned := func(mode dmarc.AlignmentMode) bool {
		for _, d := range v.dkim {
			if d.Err == nil && aligned(d.Domain, v.from, mode) {
				return true
			}
		}
		return false
	}

	rec, err := dmarc.LookupWithOptions(v.from, &dmarc.LookupOptions{LookupTXT: lookupTXT})
	usedOrg := false
	if errors.Is(err, dmarc.ErrNoPolicy) {
		if org := orgDomain(v.from); org != v.from {
			rec, err = dmarc.LookupWithOptions(org, &dmarc.LookupOptions{LookupTXT: lookupTXT})
			usedOrg = true
		}
	}
	switch {
	case err == nil:
		pass := spfAligned(rec.SPFAlignment) || dkimAligned(rec.DKIMAlignment)
		v.policy = rec.Policy
		if usedOrg && rec.SubdomainPolicy != "" {
			v.policy = rec.SubdomainPolicy
		}
		if pass {
			v.dmarc = authres.ResultPass
		} else {
			v.dmarc = authres.ResultFail
			policy := v.policy
			// pct asks receivers to apply the policy to only a sample of
			// failing mail and the next weaker policy to the rest.
			if rec.Percent != nil && *rec.Percent < 100 && rand.IntN(100) >= *rec.Percent {
				switch policy {
				case dmarc.PolicyReject:
					policy = dmarc.PolicyQuarantine
				case dmarc.PolicyQuarantine:
					policy = dmarc.PolicyNone
				}
			}
			switch policy {
			case dmarc.PolicyReject:
				v.reject, v.reason = true, fmt.Sprintf("DMARC policy of %s", v.from)
			case dmarc.PolicyQuarantine:
				v.spam, v.reason = true, "DMARC quarantine policy"
			}
		}
	case dmarc.IsTempFail(err):
		v.dmarc = authres.ResultTempError
	case errors.Is(err, dmarc.ErrNoPolicy):
		v.dmarc = authres.ResultNone
	default:
		v.dmarc = authres.ResultPermError
	}
	v.results = append(v.results, &authres.DMARCResult{Value: v.dmarc, From: v.from})

	if v.reject || v.spam {
		return v
	}
	switch {
	case localFrom && !spfAligned(dmarc.AlignmentRelaxed) && !dkimAligned(dmarc.AlignmentRelaxed):
		v.spam, v.reason = true, "unauthenticated mail from a local domain"
	case v.spf == spf.Fail && !dkimPass:
		v.spam, v.reason = true, "SPF fail"
	}
	return v
}
