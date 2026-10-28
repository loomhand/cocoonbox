package vault

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/loomhand/cocoonbox/api/utils"
)

func TestCheckNameRefusesWhatTheBoxUses(t *testing.T) {
	for _, name := range []string{"ATTIO_API_KEY", "OPENAI_API_KEY", "GITHUB_TOKEN", "_X", "A1"} {
		if err := checkName(name); err != nil {
			t.Errorf("checkName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{
		"PATH", "HOME", "HTTPS_PROXY", "GW_KEY", "COCOONBOX_VAULT", "CB_X", "CBV_1", "SSL_CERT_FILE", "NODE_OPTIONS",
		"LD_PRELOAD", "path", "1A", "A-B", "", strings.Repeat("A", 65),
	} {
		if err := checkName(name); !errors.Is(err, errInvalid) {
			t.Errorf("checkName(%q) = %v, want errInvalid", name, err)
		}
	}
}

func TestCheckHostAdmitsPublicHostsAndRefusesOpenSuffixes(t *testing.T) {
	for _, h := range []string{
		"api.attio.com", "*.attio.com", "api.ashbyhq.com", "vercel.app", "my-app.vercel.app", "abc.trycloudflare.com",
		"*.example.co.uk", "postman-echo.com",
	} {
		if err := checkHost(h); err != nil {
			t.Errorf("checkHost(%q) = %v, want nil", h, err)
		}
	}
	for _, h := range []string{
		"*.vercel.app", "*.workers.dev", "*.github.io", "*.co.uk", "*.com", "*", "co.uk", "com", "1.2.3.4", "10.0.0.1",
		"localhost", "metadata.google.internal", "box.local", "1.0.0.10.in-addr.arpa", "api.attio.com:443", "https://api.attio.com",
		"a.*.com", "*.*.com", "[::1]", "api_x.com",
	} {
		if err := checkHost(h); !errors.Is(err, errInvalid) {
			t.Errorf("checkHost(%q) = %v, want errInvalid", h, err)
		}
	}
}

func TestCheckSpecFillsDefaultsAndRefusesTransportHeaders(t *testing.T) {
	tests := []struct {
		hosts                   []string
		header, template, query string
		want                    spec
	}{
		{[]string{"API.Attio.com."}, "", "", "", spec{hosts: []string{"api.attio.com"}, header: "Authorization", template: "Bearer {}"}},
		{[]string{"a.com", "a.com"}, "x-api-key", "", "", spec{hosts: []string{"a.com"}, header: "X-Api-Key", template: "{}"}},
		{[]string{"a.com"}, "", "Basic {}:", "", spec{hosts: []string{"a.com"}, header: "Authorization", template: "Basic {}:"}},
		{[]string{"serpapi.com"}, "", "", "api_key", spec{hosts: []string{"serpapi.com"}, query: "api_key"}},
	}
	for _, tt := range tests {
		got, err := checkSpec(tt.hosts, tt.header, tt.template, tt.query)
		if err != nil || !slices.Equal(got.hosts, tt.want.hosts) || got.header != tt.want.header || got.template != tt.want.template || got.query != tt.want.query {
			t.Errorf("checkSpec(%v, %q, %q, %q) = %+v, %v, want %+v", tt.hosts, tt.header, tt.template, tt.query, got, err, tt.want)
		}
	}
	refused := []struct {
		hosts                   []string
		header, template, query string
	}{
		{nil, "", "", ""},
		{slices.Repeat([]string{"a.com"}, 9), "", "", ""},
		{[]string{"a.com"}, "Cookie", "", ""},
		{[]string{"a.com"}, "host", "", ""},
		{[]string{"a.com"}, "Proxy-Authorization", "", ""},
		{[]string{"a.com"}, "Transfer-Encoding", "", ""},
		{[]string{"a.com"}, "bad header", "", ""},
		{[]string{"a.com"}, "", "Bearer", ""},
		{[]string{"a.com"}, "", "{} {}", ""},
		{[]string{"a.com"}, "", " {}", ""},
		{[]string{"a.com"}, "", "Basic {}", ""},
		{[]string{"a.com"}, "X-Key", "", "key"},
		{[]string{"a.com"}, "", "", "a b"},
	}
	for _, tt := range refused {
		if _, err := checkSpec(tt.hosts, tt.header, tt.template, tt.query); !errors.Is(err, errInvalid) {
			t.Errorf("checkSpec(%v, %q, %q, %q) = %v, want errInvalid", tt.hosts, tt.header, tt.template, tt.query, err)
		}
	}
}

func TestCheckValue(t *testing.T) {
	if err := checkValue("sk-proj-abc"); err != nil {
		t.Errorf("checkValue = %v", err)
	}
	for _, v := range []string{"", "a\nb", " a", "a ", "a\x00", strings.Repeat("a", maxValue+1)} {
		if err := checkValue(v); !errors.Is(err, errInvalid) {
			t.Errorf("checkValue(%q) = %v, want errInvalid", v, err)
		}
	}
}

func TestRenderFillsTheSlotAndEncodesBasic(t *testing.T) {
	if got := render("Bearer {}", "v"); got != "Bearer v" {
		t.Errorf("Bearer = %q", got)
	}
	if got, want := render("Basic acct:{}", "v"), "Basic "+base64.StdEncoding.EncodeToString([]byte("acct:v")); got != want {
		t.Errorf("Basic = %q, want %q", got, want)
	}
	if got, want := render("Basic {}:", "sk_x"), "Basic "+base64.StdEncoding.EncodeToString([]byte("sk_x:")); got != want {
		t.Errorf("Basic key-as-user = %q, want %q", got, want)
	}
}

func TestPlaceholderKeepsOnlyAVendorPrefix(t *testing.T) {
	tests := map[string]string{
		"sk-proj-0123456789abcdef":      "sk-proj-",
		"sk-ant-api03-0123456789abcdef": "sk-ant-api03-",
		"ghp_0123456789abcdef":          "ghp_",
		"sk_live_0123456789":            "sk_live_",
		"A1b2C3d4E5f6":                  "",
		"0123_abcdef":                   "",
		"abcdefghij_xyz":                "",
	}
	for value, prefix := range tests {
		p := newPlaceholder(value)
		rest, ok := strings.CutPrefix(p, prefix+placeholderMark)
		if !ok || len(rest) != 2*placeholderBytes || strings.Contains(p, value[len(prefix):]) {
			t.Errorf("newPlaceholder(%q) = %q, want %q%s and 32 hex", value, p, prefix, placeholderMark)
		}
	}
	if newPlaceholder("ghp_x") == newPlaceholder("ghp_x") {
		t.Error("two placeholders of one value are equal")
	}
}

func TestRecordEscapesTheSendAs(t *testing.T) {
	header := &secret{name: "ATTIO_API_KEY", placeholder: "cbv_1", hosts: []string{"api.attio.com", "*.attio.com"}, header: "Authorization", template: "Bearer {}"}
	if got := header.record(); got != "ATTIO_API_KEY=cbv_1;api.attio.com,*.attio.com;Authorization:%20Bearer%20{}" {
		t.Errorf("header record = %q", got)
	}
	query := &secret{name: "SERP", placeholder: "cbv_2", hosts: []string{"serpapi.com"}, query: "api_key"}
	if got := query.record(); got != "SERP=cbv_2;serpapi.com;?api_key={}" {
		t.Errorf("query record = %q", got)
	}
	odd := &secret{name: "X", placeholder: "cbv_3", hosts: []string{"a.com"}, header: "X-Key", template: "k;%{}"}
	if got := odd.record(); strings.ContainsAny(got, " ") || strings.Count(got, ";") != 2 {
		t.Errorf("record with ; and %% = %q, want them escaped", got)
	}
}

func TestEntryGatesEveryKeyOnItsPlaceholder(t *testing.T) {
	sec := &secret{id: "vs_00aa", placeholder: "cbv_1", hosts: []string{"a.com"}, header: "Authorization", template: "Bearer {}"}
	if e := sec.entry("v"); e.Value != "Bearer v" || e.Placeholder != "Bearer cbv_1" || e.Header != "Authorization" || e.Query != "" {
		t.Errorf("header entry = %+v", e)
	}
	sec.header, sec.template, sec.query = "", "", "api_key"
	if e := sec.entry("v"); e.Value != "v" || e.Placeholder != "cbv_1" || e.Query != "api_key" || e.Header != "" {
		t.Errorf("query entry = %+v", e)
	}
	if got := envName(sec.id); got != "CBV_00aa" {
		t.Errorf("envName = %q", got)
	}
}

func TestFitsBoundsCountAndClaimEnvSize(t *testing.T) {
	small := func(i int) *secret {
		return &secret{
			id: idPrefix + utils.RandomHex(idBytes), name: "K" + strings.Repeat("A", i%10), placeholder: placeholderMark + utils.RandomHex(placeholderBytes),
			hosts: []string{"api.example.com"}, header: "Authorization", template: "Bearer {}", valueLen: 64,
		}
	}
	var list []*secret
	for i := range maxKeys {
		list = append(list, small(i))
	}
	if err := fits(list); err != nil {
		t.Errorf("fits(%d small keys) = %v", maxKeys, err)
	}
	if err := fits(append(list, small(0))); !errors.Is(err, errQuota) {
		t.Errorf("fits(%d keys) = %v, want errQuota", maxKeys+1, err)
	}
	wide := make([]*secret, 0, maxKeys)
	for range maxKeys {
		sec := small(0)
		sec.hosts = slices.Repeat([]string{strings.Repeat("a", 60) + ".example.com"}, maxHosts)
		wide = append(wide, sec)
	}
	if err := fits(wide); !errors.Is(err, errQuota) {
		t.Errorf("fits(keys past the guest budget) = %v, want errQuota", err)
	}
	h, _ := (&secret{id: "vs_00", placeholder: "cbv_1", hosts: []string{"a.com"}, template: "Basic u:{}", header: "Authorization", valueLen: 10}).size()
	if want := len("CBV_00") + len("Authorization") + len("a.com") + len(render("Basic u:{}", "0123456789")) + len(render("Basic u:{}", "cbv_1")); h != want {
		t.Errorf("size of a Basic key = %d, want %d", h, want)
	}
}

func TestDigestIgnoresOrderAndIsEmptyWithoutKeys(t *testing.T) {
	if digest(nil) != "" {
		t.Error("digest of no keys is not empty")
	}
	a, b := digest([]string{"vs_1:1", "vs_2:3"}), digest([]string{"vs_2:3", "vs_1:1"})
	if a != b || len(a) != digestLen || a == digest([]string{"vs_1:2", "vs_2:3"}) {
		t.Errorf("digests %q %q", a, b)
	}
}
