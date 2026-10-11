package vault

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/netip"
	"net/textproto"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/publicsuffix"
)

// spec is a key's validated send-as: where the value goes and how.
type spec struct {
	hosts    []string
	header   string
	template string
	query    string
}

func checkName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%w: a key name is an env var name: capital letters, digits and _, at most 64, not starting with a digit", errInvalid)
	}
	if slices.Contains(reservedNames, name) || slices.ContainsFunc(reservedPrefixes, func(p string) bool { return strings.HasPrefix(name, p) }) {
		return fmt.Errorf("%w: %s is a name the box itself uses", errInvalid, name)
	}
	return nil
}

func checkValue(value string) error {
	switch {
	case value == "":
		return fmt.Errorf("%w: the value is empty", errInvalid)
	case len(value) > maxValue:
		return fmt.Errorf("%w: the value must be at most %d bytes", errInvalid, maxValue)
	case strings.TrimSpace(value) != value || !httpguts.ValidHeaderFieldValue(value):
		return fmt.Errorf("%w: the value must be one line without surrounding space or control characters", errInvalid)
	}
	return nil
}

// checkSpec fills the defaults of a send-as and refuses one the egress proxy cannot or must not carry.
func checkSpec(hosts []string, header, template, query string) (spec, error) {
	s := spec{query: query}
	if len(hosts) == 0 || len(hosts) > maxHosts {
		return s, fmt.Errorf("%w: a key needs 1 to %d hosts", errInvalid, maxHosts)
	}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(h, "."))
		if err := checkHost(h); err != nil {
			return s, err
		}
		if !slices.Contains(s.hosts, h) {
			s.hosts = append(s.hosts, h)
		}
	}
	if query != "" {
		if header != "" || template != "" {
			return s, fmt.Errorf("%w: a key goes in a header or in the query, not both", errInvalid)
		}
		if !queryPattern.MatchString(query) {
			return s, fmt.Errorf("%w: query parameter %q must be 1 to 64 letters, digits, '.', '-' or '_'", errInvalid, query)
		}
		return s, nil
	}
	s.header = textproto.CanonicalMIMEHeaderKey(cmp.Or(header, defaultHeader))
	if !httpguts.ValidHeaderFieldName(s.header) {
		return s, fmt.Errorf("%w: %q is not a header name", errInvalid, header)
	}
	if slices.ContainsFunc(refusedHeaders, func(r string) bool { return strings.EqualFold(r, s.header) }) {
		return s, fmt.Errorf("%w: the %s header cannot carry a key", errInvalid, s.header)
	}
	s.template = template
	if s.template == "" {
		s.template = "{}"
		if s.header == defaultHeader {
			s.template = defaultTemplate
		}
	}
	switch {
	case strings.Count(s.template, slot) != 1 || len(s.template) > maxTemplate:
		return s, fmt.Errorf("%w: a template holds {} exactly once, in at most %d bytes", errInvalid, maxTemplate)
	case strings.TrimSpace(s.template) != s.template || !httpguts.ValidHeaderFieldValue(s.template):
		return s, fmt.Errorf("%w: a template is one line without surrounding space", errInvalid)
	case strings.HasPrefix(s.template, basicPrefix) && !strings.Contains(s.template, ":"):
		return s, fmt.Errorf("%w: a Basic template is `Basic <user>:{}` or `Basic {}:`", errInvalid)
	}
	return s, nil
}

// checkHost admits an exact public host name, or "*." and a domain below a public suffix.
func checkHost(h string) error {
	base, wild := strings.CutPrefix(h, "*.")
	switch {
	case h == "*" || strings.Contains(base, "*"):
		return fmt.Errorf("%w: host %q: a wildcard is only `*.` before a domain", errInvalid, h)
	case len(h) > maxHostLen || !hostPattern.MatchString(base):
		return fmt.Errorf("%w: host %q is not a host name (no scheme, port or path)", errInvalid, h)
	}
	if _, err := netip.ParseAddr(base); err == nil {
		return fmt.Errorf("%w: host %q is an IP address; name the host", errInvalid, h)
	}
	suffix, icann := publicsuffix.PublicSuffix(base)
	switch {
	case !icann && !strings.Contains(suffix, "."), strings.HasSuffix(base, ".arpa"):
		return fmt.Errorf("%w: host %q is not a public host name", errInvalid, h)
	case suffix == base && (wild || icann):
		return fmt.Errorf("%w: host %q: %s is a public suffix, open to anyone's hosts", errInvalid, h, base)
	}
	return nil
}

// render puts v where template holds {}; a Basic template sends base64 of the user and v.
func render(template, v string) string {
	filled := strings.Replace(template, slot, v, 1)
	if rest, ok := strings.CutPrefix(filled, basicPrefix); ok {
		return basicPrefix + base64.StdEncoding.EncodeToString([]byte(rest))
	}
	return filled
}

// newPlaceholder keeps a vendor prefix such as sk-proj- or ghp_ of value, so clients that check it at start-up accept the placeholder.
func newPlaceholder(value string) string {
	prefix := vendorPrefix.FindString(value)
	if len(prefix) > maxPrefix {
		prefix = ""
	}
	return prefix + placeholderMark + randomHex(placeholderBytes)
}

func valueHint(value string) string {
	if len(value) < hintMinLen {
		return ""
	}
	return value[len(value)-hintLen:]
}

func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}
