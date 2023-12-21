package mox

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/exp/maps"
	"golang.org/x/exp/slog"

	"github.com/mjl-/adns"

	"github.com/mjl-/mox/config"
	"github.com/mjl-/mox/dkim"
	"github.com/mjl-/mox/dmarc"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/junk"
	"github.com/mjl-/mox/mtasts"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/tlsrpt"
)

// TXTStrings returns a TXT record value as one or more quoted strings, each max
// 100 characters. In case of multiple strings, a multi-line record is returned.
func TXTStrings(s string) string {
	if len(s) <= 100 {
		return `"` + s + `"`
	}

	r := "(\n"
	for len(s) > 0 {
		n := len(s)
		if n > 100 {
			n = 100
		}
		if r != "" {
			r += " "
		}
		r += "\t\t\"" + s[:n] + "\"\n"
		s = s[n:]
	}
	r += "\t)"
	return r
}

// MakeDKIMEd25519Key returns a PEM buffer containing an ed25519 key for use
// with DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMEd25519Key(selector, domain dns.Domain) ([]byte, error) {
	_, privKey, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("ed25519", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

func dkimKeyNote(kind string, selector, domain dns.Domain) string {
	s := kind + " dkim private key"
	var zero dns.Domain
	if selector != zero && domain != zero {
		s += fmt.Sprintf(" for %s._domainkey.%s", selector.ASCII, domain.ASCII)
	}
	s += fmt.Sprintf(", generated by mox on %s", time.Now().Format(time.RFC3339))
	return s
}

// MakeDKIMEd25519Key returns a PEM buffer containing an rsa key for use with
// DKIM.
// selector and domain can be empty. If not, they are used in the note.
func MakeDKIMRSAKey(selector, domain dns.Domain) ([]byte, error) {
	// 2048 bits seems reasonable in 2022, 1024 is on the low side, larger
	// keys may not fit in UDP DNS response.
	privKey, err := rsa.GenerateKey(cryptorand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, fmt.Errorf("marshal key: %w", err)
	}

	block := &pem.Block{
		Type: "PRIVATE KEY",
		Headers: map[string]string{
			"Note": dkimKeyNote("rsa-2048", selector, domain),
		},
		Bytes: pkcs8,
	}
	b := &bytes.Buffer{}
	if err := pem.Encode(b, block); err != nil {
		return nil, fmt.Errorf("encoding pem: %w", err)
	}
	return b.Bytes(), nil
}

// MakeAccountConfig returns a new account configuration for an email address.
func MakeAccountConfig(addr smtp.Address) config.Account {
	account := config.Account{
		Domain: addr.Domain.Name(),
		Destinations: map[string]config.Destination{
			addr.String(): {},
		},
		RejectsMailbox: "Rejects",
		JunkFilter: &config.JunkFilter{
			Threshold: 0.95,
			Params: junk.Params{
				Onegrams:    true,
				MaxPower:    .01,
				TopWords:    10,
				IgnoreWords: .1,
				RareWords:   2,
			},
		},
	}
	account.AutomaticJunkFlags.Enabled = true
	account.AutomaticJunkFlags.JunkMailboxRegexp = "^(junk|spam)"
	account.AutomaticJunkFlags.NeutralMailboxRegexp = "^(inbox|neutral|postmaster|dmarc|tlsrpt|rejects)"
	account.SubjectPass.Period = 12 * time.Hour
	return account
}

// MakeDomainConfig makes a new config for a domain, creating DKIM keys, using
// accountName for DMARC and TLS reports.
func MakeDomainConfig(ctx context.Context, domain, hostname dns.Domain, accountName string, withMTASTS bool) (config.Domain, []string, error) {
	log := pkglog.WithContext(ctx)

	now := time.Now()
	year := now.Format("2006")
	timestamp := now.Format("20060102T150405")

	var paths []string
	defer func() {
		for _, p := range paths {
			err := os.Remove(p)
			log.Check(err, "removing path for domain config", slog.String("path", p))
		}
	}()

	writeFile := func(path string, data []byte) error {
		os.MkdirAll(filepath.Dir(path), 0770)

		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0660)
		if err != nil {
			return fmt.Errorf("creating file %s: %s", path, err)
		}
		defer func() {
			if f != nil {
				err := f.Close()
				log.Check(err, "closing file after error")
				err = os.Remove(path)
				log.Check(err, "removing file after error", slog.String("path", path))
			}
		}()
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("writing file %s: %s", path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close file: %v", err)
		}
		f = nil
		return nil
	}

	confDKIM := config.DKIM{
		Selectors: map[string]config.Selector{},
	}

	addSelector := func(kind, name string, privKey []byte) error {
		record := fmt.Sprintf("%s._domainkey.%s", name, domain.ASCII)
		keyPath := filepath.Join("dkim", fmt.Sprintf("%s.%s.%s.privatekey.pkcs8.pem", record, timestamp, kind))
		p := configDirPath(ConfigDynamicPath, keyPath)
		if err := writeFile(p, privKey); err != nil {
			return err
		}
		paths = append(paths, p)
		confDKIM.Selectors[name] = config.Selector{
			// Example from RFC has 5 day between signing and expiration. ../rfc/6376:1393
			// Expiration is not intended as antireplay defense, but it may help. ../rfc/6376:1340
			// Messages in the wild have been observed with 2 hours and 1 year expiration.
			Expiration:     "72h",
			PrivateKeyFile: keyPath,
		}
		return nil
	}

	addEd25519 := func(name string) error {
		key, err := MakeDKIMEd25519Key(dns.Domain{ASCII: name}, domain)
		if err != nil {
			return fmt.Errorf("making dkim ed25519 private key: %s", err)
		}
		return addSelector("ed25519", name, key)
	}

	addRSA := func(name string) error {
		key, err := MakeDKIMRSAKey(dns.Domain{ASCII: name}, domain)
		if err != nil {
			return fmt.Errorf("making dkim rsa private key: %s", err)
		}
		return addSelector("rsa2048", name, key)
	}

	if err := addEd25519(year + "a"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addRSA(year + "b"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addEd25519(year + "c"); err != nil {
		return config.Domain{}, nil, err
	}
	if err := addRSA(year + "d"); err != nil {
		return config.Domain{}, nil, err
	}

	// We sign with the first two. In case they are misused, the switch to the other
	// keys is easy, just change the config. Operators should make the public key field
	// of the misused keys empty in the DNS records to disable the misused keys.
	confDKIM.Sign = []string{year + "a", year + "b"}

	confDomain := config.Domain{
		LocalpartCatchallSeparator: "+",
		DKIM:                       confDKIM,
		DMARC: &config.DMARC{
			Account:   accountName,
			Localpart: "dmarc-reports",
			Mailbox:   "DMARC",
		},
		TLSRPT: &config.TLSRPT{
			Account:   accountName,
			Localpart: "tls-reports",
			Mailbox:   "TLSRPT",
		},
	}

	if withMTASTS {
		confDomain.MTASTS = &config.MTASTS{
			PolicyID: time.Now().UTC().Format("20060102T150405"),
			Mode:     mtasts.ModeEnforce,
			// We start out with 24 hour, and warn in the admin interface that users should
			// increase it to weeks once the setup works.
			MaxAge: 24 * time.Hour,
			MX:     []string{hostname.ASCII},
		}
	}

	rpaths := paths
	paths = nil

	return confDomain, rpaths, nil
}

// DomainAdd adds the domain to the domains config, rewriting domains.conf and
// marking it loaded.
//
// accountName is used for DMARC/TLS report and potentially for the postmaster address.
// If the account does not exist, it is created with localpart. Localpart must be
// set only if the account does not yet exist.
func DomainAdd(ctx context.Context, domain dns.Domain, accountName string, localpart smtp.Localpart) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding domain", rerr,
				slog.Any("domain", domain),
				slog.String("account", accountName),
				slog.Any("localpart", localpart))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Domains[domain.Name()]; ok {
		return fmt.Errorf("domain already present")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	for name, d := range c.Domains {
		nc.Domains[name] = d
	}

	// Only enable mta-sts for domain if there is a listener with mta-sts.
	var withMTASTS bool
	for _, l := range Conf.Static.Listeners {
		if l.MTASTSHTTPS.Enabled {
			withMTASTS = true
			break
		}
	}

	confDomain, cleanupFiles, err := MakeDomainConfig(ctx, domain, Conf.Static.HostnameDomain, accountName, withMTASTS)
	if err != nil {
		return fmt.Errorf("preparing domain config: %v", err)
	}
	defer func() {
		for _, f := range cleanupFiles {
			err := os.Remove(f)
			log.Check(err, "cleaning up file after error", slog.String("path", f))
		}
	}()

	if _, ok := c.Accounts[accountName]; ok && localpart != "" {
		return fmt.Errorf("account already exists (leave localpart empty when using an existing account)")
	} else if !ok && localpart == "" {
		return fmt.Errorf("account does not yet exist (specify a localpart)")
	} else if accountName == "" {
		return fmt.Errorf("account name is empty")
	} else if !ok {
		nc.Accounts[accountName] = MakeAccountConfig(smtp.Address{Localpart: localpart, Domain: domain})
	} else if accountName != Conf.Static.Postmaster.Account {
		nacc := nc.Accounts[accountName]
		nd := map[string]config.Destination{}
		for k, v := range nacc.Destinations {
			nd[k] = v
		}
		pmaddr := smtp.Address{Localpart: "postmaster", Domain: domain}
		nd[pmaddr.String()] = config.Destination{}
		nacc.Destinations = nd
		nc.Accounts[accountName] = nacc
	}

	nc.Domains[domain.Name()] = confDomain

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("domain added", slog.Any("domain", domain))
	cleanupFiles = nil // All good, don't cleanup.
	return nil
}

// DomainRemove removes domain from the config, rewriting domains.conf.
//
// No accounts are removed, also not when they still reference this domain.
func DomainRemove(ctx context.Context, domain dns.Domain) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing domain", rerr, slog.Any("domain", domain))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	domConf, ok := c.Domains[domain.Name()]
	if !ok {
		return fmt.Errorf("domain does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Domains = map[string]config.Domain{}
	s := domain.Name()
	for name, d := range c.Domains {
		if name != s {
			nc.Domains[name] = d
		}
	}

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}

	// Move away any DKIM private keys to a subdirectory "old". But only if
	// they are not in use by other domains.
	usedKeyPaths := map[string]bool{}
	for _, dc := range nc.Domains {
		for _, sel := range dc.DKIM.Selectors {
			usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] = true
		}
	}
	for _, sel := range domConf.DKIM.Selectors {
		if sel.PrivateKeyFile == "" || usedKeyPaths[filepath.Clean(sel.PrivateKeyFile)] {
			continue
		}
		src := ConfigDirPath(sel.PrivateKeyFile)
		dst := ConfigDirPath(filepath.Join(filepath.Dir(sel.PrivateKeyFile), "old", filepath.Base(sel.PrivateKeyFile)))
		_, err := os.Stat(dst)
		if err == nil {
			err = fmt.Errorf("destination already exists")
		} else if os.IsNotExist(err) {
			os.MkdirAll(filepath.Dir(dst), 0770)
			err = os.Rename(src, dst)
		}
		if err != nil {
			log.Errorx("renaming dkim private key file for removed domain", err, slog.String("src", src), slog.String("dst", dst))
		}
	}

	log.Info("domain removed", slog.Any("domain", domain))
	return nil
}

func WebserverConfigSet(ctx context.Context, domainRedirects map[string]string, webhandlers []config.WebHandler) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving webserver config", rerr)
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := Conf.Dynamic
	nc.WebDomainRedirects = domainRedirects
	nc.WebHandlers = webhandlers

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}

	log.Info("webserver config saved")
	return nil
}

// todo: find a way to automatically create the dns records as it would greatly simplify setting up email for a domain. we could also dynamically make changes, e.g. providing grace periods after disabling a dkim key, only automatically removing the dkim dns key after a few days. but this requires some kind of api and authentication to the dns server. there doesn't appear to be a single commonly used api for dns management. each of the numerous cloud providers have their own APIs and rather large SKDs to use them. we don't want to link all of them in.

// DomainRecords returns text lines describing DNS records required for configuring
// a domain.
//
// If certIssuerDomainName is set, CAA records to limit TLS certificate issuance to
// that caID will be suggested. If acmeAccountURI is also set, CAA records also
// restricting issuance to that account ID will be suggested.
func DomainRecords(domConf config.Domain, domain dns.Domain, hasDNSSEC bool, certIssuerDomainName, acmeAccountURI string) ([]string, error) {
	d := domain.ASCII
	h := Conf.Static.HostnameDomain.ASCII

	// The first line with ";" is used by ../testdata/integration/moxacmepebble.sh and
	// ../testdata/integration/moxmail2.sh for selecting DNS records
	records := []string{
		"; Time To Live of 5 minutes, may be recognized if importing as a zone file.",
		"; Once your setup is working, you may want to increase the TTL.",
		"$TTL 300",
		"",
	}

	if public, ok := Conf.Static.Listeners["public"]; ok && public.TLS != nil && (len(public.TLS.HostPrivateRSA2048Keys) > 0 || len(public.TLS.HostPrivateECDSAP256Keys) > 0) {
		records = append(records,
			`; DANE: These records indicate that a remote mail server trying to deliver email`,
			`; with SMTP (TCP port 25) must verify the TLS certificate with DANE-EE (3), based`,
			`; on the certificate public key ("SPKI", 1) that is SHA2-256-hashed (1) to the`,
			`; hexadecimal hash. DANE-EE verification means only the certificate or public`,
			`; key is verified, not whether the certificate is signed by a (centralized)`,
			`; certificate authority (CA), is expired, or matches the host name.`,
			`;`,
			`; NOTE: Create the records below only once: They are for the machine, and apply`,
			`; to all hosted domains.`,
		)
		if !hasDNSSEC {
			records = append(records,
				";",
				"; WARNING: Domain does not appear to be DNSSEC-signed. To enable DANE, first",
				"; enable DNSSEC on your domain, then add the TLSA records. Records below have been",
				"; commented out.",
			)
		}
		addTLSA := func(privKey crypto.Signer) error {
			spkiBuf, err := x509.MarshalPKIXPublicKey(privKey.Public())
			if err != nil {
				return fmt.Errorf("marshal SubjectPublicKeyInfo for DANE record: %v", err)
			}
			sum := sha256.Sum256(spkiBuf)
			tlsaRecord := adns.TLSA{
				Usage:     adns.TLSAUsageDANEEE,
				Selector:  adns.TLSASelectorSPKI,
				MatchType: adns.TLSAMatchTypeSHA256,
				CertAssoc: sum[:],
			}
			var s string
			if hasDNSSEC {
				s = fmt.Sprintf("_25._tcp.%-*s TLSA %s", 20+len(d)-len("_25._tcp."), h+".", tlsaRecord.Record())
			} else {
				s = fmt.Sprintf(";; _25._tcp.%-*s TLSA %s", 20+len(d)-len(";; _25._tcp."), h+".", tlsaRecord.Record())
			}
			records = append(records, s)
			return nil
		}
		for _, privKey := range public.TLS.HostPrivateECDSAP256Keys {
			if err := addTLSA(privKey); err != nil {
				return nil, err
			}
		}
		for _, privKey := range public.TLS.HostPrivateRSA2048Keys {
			if err := addTLSA(privKey); err != nil {
				return nil, err
			}
		}
		records = append(records, "")
	}

	if d != h {
		records = append(records,
			"; For the machine, only needs to be created once, for the first domain added:",
			"; ",
			"; SPF-allow host for itself, resulting in relaxed DMARC pass for (postmaster)",
			"; messages (DSNs) sent from host:",
			fmt.Sprintf(`%-*s TXT "v=spf1 a -all"`, 20+len(d), h+"."), // ../rfc/7208:2263 ../rfc/7208:2287
			"",
		)
	}
	if d != h && Conf.Static.HostTLSRPT.ParsedLocalpart != "" {
		uri := url.URL{
			Scheme: "mailto",
			Opaque: smtp.NewAddress(Conf.Static.HostTLSRPT.ParsedLocalpart, Conf.Static.HostnameDomain).Pack(false),
		}
		tlsrptr := tlsrpt.Record{Version: "TLSRPTv1", RUAs: [][]tlsrpt.RUA{{tlsrpt.RUA(uri.String())}}}
		records = append(records,
			"; For the machine, only needs to be created once, for the first domain added:",
			"; ",
			"; Request reporting about success/failures of TLS connections to (MX) host, for DANE.",
			fmt.Sprintf(`_smtp._tls.%-*s         TXT "%s"`, 20+len(d)-len("_smtp._tls."), h+".", tlsrptr.String()),
			"",
		)
	}

	records = append(records,
		"; Deliver email for the domain to this host.",
		fmt.Sprintf("%s.                    MX 10 %s.", d, h),
		"",

		"; Outgoing messages will be signed with the first two DKIM keys. The other two",
		"; configured for backup, switching to them is just a config change.",
	)
	var selectors []string
	for name := range domConf.DKIM.Selectors {
		selectors = append(selectors, name)
	}
	sort.Slice(selectors, func(i, j int) bool {
		return selectors[i] < selectors[j]
	})
	for _, name := range selectors {
		sel := domConf.DKIM.Selectors[name]
		dkimr := dkim.Record{
			Version:   "DKIM1",
			Hashes:    []string{"sha256"},
			PublicKey: sel.Key.Public(),
		}
		if _, ok := sel.Key.(ed25519.PrivateKey); ok {
			dkimr.Key = "ed25519"
		} else if _, ok := sel.Key.(*rsa.PrivateKey); !ok {
			return nil, fmt.Errorf("unrecognized private key for DKIM selector %q: %T", name, sel.Key)
		}
		txt, err := dkimr.Record()
		if err != nil {
			return nil, fmt.Errorf("making DKIM DNS TXT record: %v", err)
		}

		if len(txt) > 100 {
			records = append(records,
				"; NOTE: The following strings must be added to DNS as single record.",
			)
		}
		s := fmt.Sprintf("%s._domainkey.%s.   TXT %s", name, d, TXTStrings(txt))
		records = append(records, s)

	}
	dmarcr := dmarc.DefaultRecord
	dmarcr.Policy = "reject"
	if domConf.DMARC != nil {
		uri := url.URL{
			Scheme: "mailto",
			Opaque: smtp.NewAddress(domConf.DMARC.ParsedLocalpart, domConf.DMARC.DNSDomain).Pack(false),
		}
		dmarcr.AggregateReportAddresses = []dmarc.URI{
			{Address: uri.String(), MaxSize: 10, Unit: "m"},
		}
	}
	records = append(records,
		"",

		"; Specify the MX host is allowed to send for our domain and for itself (for DSNs).",
		"; ~all means softfail for anything else, which is done instead of -all to prevent older",
		"; mail servers from rejecting the message because they never get to looking for a dkim/dmarc pass.",
		fmt.Sprintf(`%s.                    TXT "v=spf1 mx ~all"`, d),
		"",

		"; Emails that fail the DMARC check (without aligned DKIM and without aligned SPF)",
		"; should be rejected, and request reports. If you email through mailing lists that",
		"; strip DKIM-Signature headers and don't rewrite the From header, you may want to",
		"; set the policy to p=none.",
		fmt.Sprintf(`_dmarc.%s.             TXT "%s"`, d, dmarcr.String()),
		"",
	)

	if sts := domConf.MTASTS; sts != nil {
		records = append(records,
			"; Remote servers can use MTA-STS to verify our TLS certificate with the",
			"; WebPKI pool of CA's (certificate authorities) when delivering over SMTP with",
			"; STARTTLSTLS.",
			fmt.Sprintf(`mta-sts.%s.            CNAME %s.`, d, h),
			fmt.Sprintf(`_mta-sts.%s.           TXT "v=STSv1; id=%s"`, d, sts.PolicyID),
			"",
		)
	} else {
		records = append(records,
			"; Note: No MTA-STS to indicate TLS should be used. Either because disabled for the",
			"; domain or because mox.conf does not have a listener with MTA-STS configured.",
			"",
		)
	}

	if domConf.TLSRPT != nil {
		uri := url.URL{
			Scheme: "mailto",
			Opaque: smtp.NewAddress(domConf.TLSRPT.ParsedLocalpart, domConf.TLSRPT.DNSDomain).Pack(false),
		}
		tlsrptr := tlsrpt.Record{Version: "TLSRPTv1", RUAs: [][]tlsrpt.RUA{{tlsrpt.RUA(uri.String())}}}
		records = append(records,
			"; Request reporting about TLS failures.",
			fmt.Sprintf(`_smtp._tls.%s.         TXT "%s"`, d, tlsrptr.String()),
			"",
		)
	}

	records = append(records,
		"; Autoconfig is used by Thunderbird. Autodiscover is (in theory) used by Microsoft.",
		fmt.Sprintf(`autoconfig.%s.         CNAME %s.`, d, h),
		fmt.Sprintf(`_autodiscover._tcp.%s. SRV 0 1 443 %s.`, d, h),
		"",

		// ../rfc/6186:133 ../rfc/8314:692
		"; For secure IMAP and submission autoconfig, point to mail host.",
		fmt.Sprintf(`_imaps._tcp.%s.        SRV 0 1 993 %s.`, d, h),
		fmt.Sprintf(`_submissions._tcp.%s.  SRV 0 1 465 %s.`, d, h),
		"",
		// ../rfc/6186:242
		"; Next records specify POP3 and non-TLS ports are not to be used.",
		"; These are optional and safe to leave out (e.g. if you have to click a lot in a",
		"; DNS admin web interface).",
		fmt.Sprintf(`_imap._tcp.%s.         SRV 0 1 143 .`, d),
		fmt.Sprintf(`_submission._tcp.%s.   SRV 0 1 587 .`, d),
		fmt.Sprintf(`_pop3._tcp.%s.         SRV 0 1 110 .`, d),
		fmt.Sprintf(`_pop3s._tcp.%s.        SRV 0 1 995 .`, d),
	)

	if certIssuerDomainName != "" {
		// ../rfc/8659:18 for CAA records.
		records = append(records,
			"",
			"; Optional:",
			"; You could mark Let's Encrypt as the only Certificate Authority allowed to",
			"; sign TLS certificates for your domain.",
			fmt.Sprintf(`%s.                    CAA 0 issue "%s"`, d, certIssuerDomainName),
		)
		if acmeAccountURI != "" {
			// ../rfc/8657:99 for accounturi.
			// ../rfc/8657:147 for validationmethods.
			records = append(records,
				";",
				"; Optionally limit certificates for this domain to the account ID and methods used by mox.",
				fmt.Sprintf(`;; %s.                 CAA 0 issue "%s; accounturi=%s; validationmethods=tls-alpn-01,http-01"`, d, certIssuerDomainName, acmeAccountURI),
				";",
				"; Or alternatively only limit for email-specific subdomains, so you can use",
				"; other accounts/methods for other subdomains.",
				fmt.Sprintf(`;; autoconfig.%s.      CAA 0 issue "%s; accounturi=%s; validationmethods=tls-alpn-01,http-01"`, d, certIssuerDomainName, acmeAccountURI),
				fmt.Sprintf(`;; mtasts.%s.          CAA 0 issue "%s; accounturi=%s; validationmethods=tls-alpn-01,http-01"`, d, certIssuerDomainName, acmeAccountURI),
			)
			if strings.HasSuffix(h, "."+d) {
				records = append(records,
					";",
					"; And the mail hostname.",
					fmt.Sprintf(`;; %-*s CAA 0 issue "%s; accounturi=%s; validationmethods=tls-alpn-01,http-01"`, 20-3+len(d), h+".", certIssuerDomainName, acmeAccountURI),
				)
			}
		} else {
			// The string "will be suggested" is used by
			// ../testdata/integration/moxacmepebble.sh and ../testdata/integration/moxmail2.sh
			// as end of DNS records.
			records = append(records,
				";",
				"; Note: After starting up, once an ACME account has been created, CAA records",
				"; that restrict issuance to the account will be suggested.",
			)
		}
	}
	return records, nil
}

// AccountAdd adds an account and an initial address and reloads the configuration.
//
// The new account does not have a password, so cannot yet log in. Email can be
// delivered.
//
// Catchall addresses are not supported for AccountAdd. Add separately with AddressAdd.
func AccountAdd(ctx context.Context, account, address string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, slog.String("account", account), slog.String("address", address))
		}
	}()

	addr, err := smtp.ParseAddress(address)
	if err != nil {
		return fmt.Errorf("parsing email address: %v", err)
	}

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Accounts[account]; ok {
		return fmt.Errorf("account already present")
	}

	if err := checkAddressAvailable(addr); err != nil {
		return fmt.Errorf("address not available: %v", err)
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[account] = MakeAccountConfig(addr)

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account added", slog.String("account", account), slog.Any("address", addr))
	return nil
}

// AccountRemove removes an account and reloads the configuration.
func AccountRemove(ctx context.Context, account string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding account", rerr, slog.String("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	if _, ok := c.Accounts[account]; !ok {
		return fmt.Errorf("account does not exist")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		if name != account {
			nc.Accounts[name] = a
		}
	}

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account removed", slog.String("account", account))
	return nil
}

// checkAddressAvailable checks that the address after canonicalization is not
// already configured, and that its localpart does not contain the catchall
// localpart separator.
//
// Must be called with config lock held.
func checkAddressAvailable(addr smtp.Address) error {
	if dc, ok := Conf.Dynamic.Domains[addr.Domain.Name()]; !ok {
		return fmt.Errorf("domain does not exist")
	} else if lp, err := CanonicalLocalpart(addr.Localpart, dc); err != nil {
		return fmt.Errorf("canonicalizing localpart: %v", err)
	} else if _, ok := Conf.accountDestinations[smtp.NewAddress(lp, addr.Domain).String()]; ok {
		return fmt.Errorf("canonicalized address %s already configured", smtp.NewAddress(lp, addr.Domain))
	} else if dc.LocalpartCatchallSeparator != "" && strings.Contains(string(addr.Localpart), dc.LocalpartCatchallSeparator) {
		return fmt.Errorf("localpart cannot include domain catchall separator %s", dc.LocalpartCatchallSeparator)
	}
	return nil
}

// AddressAdd adds an email address to an account and reloads the configuration. If
// address starts with an @ it is treated as a catchall address for the domain.
func AddressAdd(ctx context.Context, address, account string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("adding address", rerr, slog.String("address", address), slog.String("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	a, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("account does not exist")
	}

	var destAddr string
	if strings.HasPrefix(address, "@") {
		d, err := dns.ParseDomain(address[1:])
		if err != nil {
			return fmt.Errorf("parsing domain: %v", err)
		}
		dname := d.Name()
		destAddr = "@" + dname
		if _, ok := Conf.Dynamic.Domains[dname]; !ok {
			return fmt.Errorf("domain does not exist")
		} else if _, ok := Conf.accountDestinations[destAddr]; ok {
			return fmt.Errorf("catchall address already configured for domain")
		}
	} else {
		addr, err := smtp.ParseAddress(address)
		if err != nil {
			return fmt.Errorf("parsing email address: %v", err)
		}

		if err := checkAddressAvailable(addr); err != nil {
			return fmt.Errorf("address not available: %v", err)
		}
		destAddr = addr.String()
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nd := map[string]config.Destination{}
	for name, d := range a.Destinations {
		nd[name] = d
	}
	nd[destAddr] = config.Destination{}
	a.Destinations = nd
	nc.Accounts[account] = a

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("address added", slog.String("address", address), slog.String("account", account))
	return nil
}

// AddressRemove removes an email address and reloads the configuration.
func AddressRemove(ctx context.Context, address string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("removing address", rerr, slog.String("address", address))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	ad, ok := Conf.accountDestinations[address]
	if !ok {
		return fmt.Errorf("address does not exists")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	a, ok := Conf.Dynamic.Accounts[ad.Account]
	if !ok {
		return fmt.Errorf("internal error: cannot find account")
	}
	na := a
	na.Destinations = map[string]config.Destination{}
	var dropped bool
	for destAddr, d := range a.Destinations {
		if destAddr != address {
			na.Destinations[destAddr] = d
		} else {
			dropped = true
		}
	}
	if !dropped {
		return fmt.Errorf("address not removed, likely a postmaster/reporting address")
	}
	nc := Conf.Dynamic
	nc.Accounts = map[string]config.Account{}
	for name, a := range Conf.Dynamic.Accounts {
		nc.Accounts[name] = a
	}
	nc.Accounts[ad.Account] = na

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("address removed", slog.String("address", address), slog.String("account", ad.Account))
	return nil
}

// AccountFullNameSave updates the full name for an account and reloads the configuration.
func AccountFullNameSave(ctx context.Context, account, fullName string) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving account full name", rerr, slog.String("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	acc, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("account not present")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}

	acc.FullName = fullName
	nc.Accounts[account] = acc

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account full name saved", slog.String("account", account))
	return nil
}

// DestinationSave updates a destination for an account and reloads the configuration.
func DestinationSave(ctx context.Context, account, destName string, newDest config.Destination) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving destination", rerr,
				slog.String("account", account),
				slog.String("destname", destName),
				slog.Any("destination", newDest))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	acc, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("account not present")
	}

	if _, ok := acc.Destinations[destName]; !ok {
		return fmt.Errorf("destination not present")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	nd := map[string]config.Destination{}
	for dn, d := range acc.Destinations {
		nd[dn] = d
	}
	nd[destName] = newDest
	nacc := nc.Accounts[account]
	nacc.Destinations = nd
	nc.Accounts[account] = nacc

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("destination saved", slog.String("account", account), slog.String("destname", destName))
	return nil
}

// AccountLimitsSave saves new message sending limits for an account.
func AccountLimitsSave(ctx context.Context, account string, maxOutgoingMessagesPerDay, maxFirstTimeRecipientsPerDay int, quotaMessageSize int64) (rerr error) {
	log := pkglog.WithContext(ctx)
	defer func() {
		if rerr != nil {
			log.Errorx("saving account limits", rerr, slog.String("account", account))
		}
	}()

	Conf.dynamicMutex.Lock()
	defer Conf.dynamicMutex.Unlock()

	c := Conf.Dynamic
	acc, ok := c.Accounts[account]
	if !ok {
		return fmt.Errorf("account not present")
	}

	// Compose new config without modifying existing data structures. If we fail, we
	// leave no trace.
	nc := c
	nc.Accounts = map[string]config.Account{}
	for name, a := range c.Accounts {
		nc.Accounts[name] = a
	}
	acc.MaxOutgoingMessagesPerDay = maxOutgoingMessagesPerDay
	acc.MaxFirstTimeRecipientsPerDay = maxFirstTimeRecipientsPerDay
	acc.QuotaMessageSize = quotaMessageSize
	nc.Accounts[account] = acc

	if err := writeDynamic(ctx, log, nc); err != nil {
		return fmt.Errorf("writing domains.conf: %v", err)
	}
	log.Info("account limits saved", slog.String("account", account))
	return nil
}

type TLSMode uint8

const (
	TLSModeImmediate TLSMode = 0
	TLSModeSTARTTLS  TLSMode = 1
	TLSModeNone      TLSMode = 2
)

type ProtocolConfig struct {
	Host    dns.Domain
	Port    int
	TLSMode TLSMode
}

type ClientConfig struct {
	IMAP       ProtocolConfig
	Submission ProtocolConfig
}

// ClientConfigDomain returns a single IMAP and Submission client configuration for
// a domain.
func ClientConfigDomain(d dns.Domain) (rconfig ClientConfig, rerr error) {
	var haveIMAP, haveSubmission bool

	if _, ok := Conf.Domain(d); !ok {
		return ClientConfig{}, fmt.Errorf("unknown domain")
	}

	gather := func(l config.Listener) (done bool) {
		host := Conf.Static.HostnameDomain
		if l.Hostname != "" {
			host = l.HostnameDomain
		}
		if !haveIMAP && l.IMAPS.Enabled {
			rconfig.IMAP.Host = host
			rconfig.IMAP.Port = config.Port(l.IMAPS.Port, 993)
			rconfig.IMAP.TLSMode = TLSModeImmediate
			haveIMAP = true
		}
		if !haveIMAP && l.IMAP.Enabled {
			rconfig.IMAP.Host = host
			rconfig.IMAP.Port = config.Port(l.IMAP.Port, 143)
			rconfig.IMAP.TLSMode = TLSModeSTARTTLS
			if l.TLS == nil {
				rconfig.IMAP.TLSMode = TLSModeNone
			}
			haveIMAP = true
		}
		if !haveSubmission && l.Submissions.Enabled {
			rconfig.Submission.Host = host
			rconfig.Submission.Port = config.Port(l.Submissions.Port, 465)
			rconfig.Submission.TLSMode = TLSModeImmediate
			haveSubmission = true
		}
		if !haveSubmission && l.Submission.Enabled {
			rconfig.Submission.Host = host
			rconfig.Submission.Port = config.Port(l.Submission.Port, 587)
			rconfig.Submission.TLSMode = TLSModeSTARTTLS
			if l.TLS == nil {
				rconfig.Submission.TLSMode = TLSModeNone
			}
			haveSubmission = true
		}
		return haveIMAP && haveSubmission
	}

	// Look at the public listener first. Most likely the intended configuration.
	if public, ok := Conf.Static.Listeners["public"]; ok {
		if gather(public) {
			return
		}
	}
	// Go through the other listeners in consistent order.
	names := maps.Keys(Conf.Static.Listeners)
	sort.Strings(names)
	for _, name := range names {
		if gather(Conf.Static.Listeners[name]) {
			return
		}
	}
	return ClientConfig{}, fmt.Errorf("no listeners found for imap and/or submission")
}

// ClientConfigs holds the client configuration for IMAP/Submission for a
// domain.
type ClientConfigs struct {
	Entries []ClientConfigsEntry
}

type ClientConfigsEntry struct {
	Protocol string
	Host     dns.Domain
	Port     int
	Listener string
	Note     string
}

// ClientConfigsDomain returns the client configs for IMAP/Submission for a
// domain.
func ClientConfigsDomain(d dns.Domain) (ClientConfigs, error) {
	_, ok := Conf.Domain(d)
	if !ok {
		return ClientConfigs{}, fmt.Errorf("unknown domain")
	}

	c := ClientConfigs{}
	c.Entries = []ClientConfigsEntry{}
	var listeners []string

	for name := range Conf.Static.Listeners {
		listeners = append(listeners, name)
	}
	sort.Slice(listeners, func(i, j int) bool {
		return listeners[i] < listeners[j]
	})

	note := func(tls bool, requiretls bool) string {
		if !tls {
			return "plain text, no STARTTLS configured"
		}
		if requiretls {
			return "STARTTLS required"
		}
		return "STARTTLS optional"
	}

	for _, name := range listeners {
		l := Conf.Static.Listeners[name]
		host := Conf.Static.HostnameDomain
		if l.Hostname != "" {
			host = l.HostnameDomain
		}
		if l.Submissions.Enabled {
			c.Entries = append(c.Entries, ClientConfigsEntry{"Submission (SMTP)", host, config.Port(l.Submissions.Port, 465), name, "with TLS"})
		}
		if l.IMAPS.Enabled {
			c.Entries = append(c.Entries, ClientConfigsEntry{"IMAP", host, config.Port(l.IMAPS.Port, 993), name, "with TLS"})
		}
		if l.Submission.Enabled {
			c.Entries = append(c.Entries, ClientConfigsEntry{"Submission (SMTP)", host, config.Port(l.Submission.Port, 587), name, note(l.TLS != nil, !l.Submission.NoRequireSTARTTLS)})
		}
		if l.IMAP.Enabled {
			c.Entries = append(c.Entries, ClientConfigsEntry{"IMAP", host, config.Port(l.IMAPS.Port, 143), name, note(l.TLS != nil, !l.IMAP.NoRequireSTARTTLS)})
		}
	}

	return c, nil
}

// IPs returns ip addresses we may be listening/receiving mail on or
// connecting/sending from to the outside.
func IPs(ctx context.Context, receiveOnly bool) ([]net.IP, error) {
	log := pkglog.WithContext(ctx)

	// Try to gather all IPs we are listening on by going through the config.
	// If we encounter 0.0.0.0 or ::, we'll gather all local IPs afterwards.
	var ips []net.IP
	var ipv4all, ipv6all bool
	for _, l := range Conf.Static.Listeners {
		// If NATed, we don't know our external IPs.
		if l.IPsNATed {
			return nil, nil
		}
		check := l.IPs
		if len(l.NATIPs) > 0 {
			check = l.NATIPs
		}
		for _, s := range check {
			ip := net.ParseIP(s)
			if ip.IsUnspecified() {
				if ip.To4() != nil {
					ipv4all = true
				} else {
					ipv6all = true
				}
				continue
			}
			ips = append(ips, ip)
		}
	}

	// We'll list the IPs on the interfaces. How useful is this? There is a good chance
	// we're listening on all addresses because of a load balancer/firewall.
	if ipv4all || ipv6all {
		ifaces, err := net.Interfaces()
		if err != nil {
			return nil, fmt.Errorf("listing network interfaces: %v", err)
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				return nil, fmt.Errorf("listing addresses for network interface: %v", err)
			}
			if len(addrs) == 0 {
				continue
			}

			for _, addr := range addrs {
				ip, _, err := net.ParseCIDR(addr.String())
				if err != nil {
					log.Errorx("bad interface addr", err, slog.Any("address", addr))
					continue
				}
				v4 := ip.To4() != nil
				if ipv4all && v4 || ipv6all && !v4 {
					ips = append(ips, ip)
				}
			}
		}
	}

	if receiveOnly {
		return ips, nil
	}

	for _, t := range Conf.Static.Transports {
		if t.Socks != nil {
			ips = append(ips, t.Socks.IPs...)
		}
	}

	return ips, nil
}
