package sourceorigin

import (
	"strings"
	"testing"
)

func TestNormalizeTable(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "https credentials metadata and default port",
			raw:  "HTTPS://Alice:secret@GitHub.COM:0443/NeuroForge/RKC.git/?token=secret#private",
			want: "https://github.com/NeuroForge/RKC.git/",
		},
		{
			name: "ssh username and default port",
			raw:  "SSH://git@Example.COM:22/Owner/Repo.git",
			want: "ssh://example.com/Owner/Repo.git",
		},
		{
			name: "ssh nondefault port",
			raw:  "ssh://git@Example.COM:002222/Owner/Repo.git",
			want: "ssh://example.com:2222/Owner/Repo.git",
		},
		{
			name: "git transport stays distinct",
			raw:  "GIT://Example.COM:09418/Owner/Repo.git",
			want: "git://example.com:9418/Owner/Repo.git",
		},
		{
			name: "file transport and path case",
			raw:  "FILE://WORKSTATION/C:/Source/RKC.git#ignored",
			want: "file://workstation/C:/Source/RKC.git",
		},
		{
			name: "local file transport",
			raw:  "file:///Users/Alice/Source/RKC.git/",
			want: "file:///Users/Alice/Source/RKC.git/",
		},
		{
			name: "scp shorthand",
			raw:  "git@GitHub.COM:NeuroForge/RKC.git",
			want: "ssh://github.com/NeuroForge/RKC.git",
		},
		{
			name: "scp shorthand absolute path",
			raw:  "deploy@Example.COM:/Srv/Repo.git/",
			want: "ssh://example.com/Srv/Repo.git/",
		},
		{
			name: "scp shorthand ipv6",
			raw:  "git@[2001:DB8::1]:Owner/Repo.git",
			want: "ssh://[2001:db8::1]/Owner/Repo.git",
		},
		{
			name: "escaped path remains byte exact",
			raw:  "https://EXAMPLE.com/%4Fea%2fRepo%2Egit?discarded=true",
			want: "https://example.com/%4Fea%2fRepo%2Egit",
		},
		{
			name: "escaped delimiters remain path data",
			raw:  "https://example.com/a%3Fb%23c.git#discarded",
			want: "https://example.com/a%3Fb%23c.git",
		},
		{
			name: "empty path keeps trailing semantics",
			raw:  "https://EXAMPLE.com?discarded=true",
			want: "https://example.com",
		},
		{
			name: "root path keeps trailing semantics",
			raw:  "https://EXAMPLE.com/?discarded=true",
			want: "https://example.com/",
		},
		{
			name: "ipv6 zone and default port",
			raw:  "ssh://git@[FE80::1%25ETH0]:022/Repo.git",
			want: "ssh://[fe80::1%25eth0]/Repo.git",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Normalize(test.raw)
			if err != nil {
				t.Fatalf("Normalize returned error: %v", err)
			}
			if got != test.want {
				t.Fatalf("Normalize = %q, want %q", got, test.want)
			}
			second, err := Normalize(got)
			if err != nil || second != got {
				t.Fatalf("Normalize is not idempotent: second=%q err=%v", second, err)
			}
			if !IsCanonical(got) {
				t.Fatalf("IsCanonical(%q) = false", got)
			}
			if test.raw != test.want && IsCanonical(test.raw) {
				t.Fatalf("IsCanonical accepted noncanonical input %q", test.raw)
			}
		})
	}
}

func TestNormalizeRejectsInvalidInputsWithoutDisclosure(t *testing.T) {
	invalid := []string{
		"",
		"relative/repository.git",
		"http://example.com/repository.git",
		"ftp://example.com/repository.git",
		"https:opaque",
		"https:///repository.git",
		"https://example.com:/repository.git",
		"https://example.com:0/repository.git",
		"https://example.com:65536/repository.git",
		"https://example.com:notaport/repository.git",
		"file://example.com:22/repository.git",
		"https://example.com/invalid%escape",
		"https://example.com/a path/repository.git",
		"https://example.com/a\\path/repository.git",
		"git@host:",
		"git@@host:repository.git",
		"bad:user@host:repository.git",
		"git@[broken:repository.git",
		"https://example.com/repository.git\nSUPER_SECRET_SENTINEL",
		"https://example.com/\u0085SUPER_SECRET_SENTINEL",
		string([]byte{'h', 't', 't', 'p', 's', ':', '/', '/', 0xff}),
		strings.Repeat("SUPER_SECRET_SENTINEL", maxOriginBytes),
	}
	for index, raw := range invalid {
		got, err := Normalize(raw)
		if err == nil {
			t.Errorf("case %d: Normalize unexpectedly succeeded with %q", index, got)
			continue
		}
		if got != "" {
			t.Errorf("case %d: rejected result = %q, want empty", index, got)
		}
		if strings.Contains(err.Error(), "SUPER_SECRET_SENTINEL") || strings.Contains(err.Error(), raw) && raw != "" {
			t.Errorf("case %d: error disclosed caller input: %q", index, err)
		}
		if IsCanonical(raw) {
			t.Errorf("case %d: IsCanonical accepted invalid input", index)
		}
	}
}

func TestNormalizeStripsEveryCredentialField(t *testing.T) {
	const sentinel = "TOP_SECRET_SENTINEL"
	inputs := []string{
		"https://alice:" + sentinel + "@example.com/Owner/Repo.git?token=" + sentinel + "#" + sentinel,
		"ssh://" + sentinel + "@example.com/Owner/Repo.git?" + sentinel + "#" + sentinel,
		"file://alice:" + sentinel + "@WORKSTATION/Share/Repo.git?" + sentinel + "#" + sentinel,
		"alice@EXAMPLE.com:Owner/Repo.git?" + sentinel + "#" + sentinel,
	}
	for _, raw := range inputs {
		normalized, err := Normalize(raw)
		if err != nil {
			t.Fatalf("Normalize returned error: %v", err)
		}
		if strings.Contains(normalized, sentinel) || strings.Contains(normalized, "alice@") {
			t.Fatalf("normalized origin retained credentials or metadata: %q", normalized)
		}
	}
}

func TestNormalizeBoundaryAndHelpers(t *testing.T) {
	prefix := "https://example.com/"
	atLimit := prefix + strings.Repeat("a", maxOriginBytes-len(prefix))
	if normalized, err := Normalize(atLimit); err != nil || normalized != atLimit {
		t.Fatalf("max-size origin = %q, %v", normalized, err)
	}
	oversizedAfterSCPExpansion := "g@h:" + strings.Repeat("a", maxOriginBytes-len("g@h:"))
	if normalized, err := Normalize(oversizedAfterSCPExpansion); err == nil || normalized != "" {
		t.Fatalf("oversized expanded SCP origin = %q, %v", normalized, err)
	}
	if normalized, recognized, err := normalizeSCP("https://git@example.com/repo.git"); err != nil || recognized || normalized != "" {
		t.Fatalf("URI recognized as SCP: %q, %t, %v", normalized, recognized, err)
	}
	if _, _, ok := splitSCPRemainder("[::1]:"); ok {
		t.Fatal("empty bracketed SCP path accepted")
	}
	if _, _, ok := splitSCPRemainder("host"); ok {
		t.Fatal("SCP remainder without separator accepted")
	}
	if !validSCPUser("build.bot+release") || validSCPUser("") {
		t.Fatal("SCP user validation is inconsistent")
	}
	if !supportedScheme("https") || supportedScheme("http") {
		t.Fatal("transport allowlist is inconsistent")
	}
	if _, ok := preservedPath("https:/invalid"); ok {
		t.Fatal("non-hierarchical URL produced a preserved path")
	}
	if got, ok := preservedPath("https://example.com#fragment"); !ok || got != "" {
		t.Fatalf("fragment-only path = %q, %t", got, ok)
	}
	if validPath("/%zz") || validPath("/private path") || validPath(`/private\path`) {
		t.Fatal("malformed URL path accepted")
	}
	if validHostname("private host") || validHostname(`private\host`) || validHostname("private/host") || validHostname("user@host") {
		t.Fatal("malformed hostname accepted")
	}
	if !explicitEmptyPort("host:") || !explicitEmptyPort("[::1]:") || explicitEmptyPort("[::1]") {
		t.Fatal("empty port detection is inconsistent")
	}
	if _, err := parsePort("12x"); err == nil {
		t.Fatal("nonnumeric port accepted")
	}
	if !isUnreserved('A') || !isUnreserved('9') || !isUnreserved('~') || isUnreserved('/') {
		t.Fatal("unreserved character classification is inconsistent")
	}
}

func FuzzNormalizeIdempotent(f *testing.F) {
	for _, seed := range []string{
		"https://User:password@EXAMPLE.com:443/Owner/%52epo.git?secret=yes#private",
		"ssh://git@example.com:22/Owner/Repo.git/",
		"git@Example.com:Owner/Repo.git",
		"git://example.com/Owner/Repo.git",
		"file:///tmp/Repo.git",
		"not an origin",
		"https://example.com/%zz",
		"https://example.com/repo.git\x00secret",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		normalized, err := Normalize(raw)
		if err != nil {
			return
		}
		second, err := Normalize(normalized)
		if err != nil {
			t.Fatalf("normalized output was rejected: %v", err)
		}
		if second != normalized {
			t.Fatalf("normalization is not idempotent: first=%q second=%q", normalized, second)
		}
		if !IsCanonical(normalized) {
			t.Fatalf("normalized output is not canonical: %q", normalized)
		}
	})
}
