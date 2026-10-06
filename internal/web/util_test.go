package web

import "testing"

func TestHostOnly(t *testing.T) {
	cases := map[string]string{
		"Example.COM:8080":       "example.com",
		"example.com":            "example.com",
		"EXAMPLE.com.":           "example.com",
		"[::1]:8080":             "::1",
		"1.2.3.4:80":             "1.2.3.4",
		"demo.sites.example.com": "demo.sites.example.com",
		"":                       "",
	}
	for in, want := range cases {
		if got := hostOnly(in); got != want {
			t.Errorf("hostOnly(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidSlug(t *testing.T) {
	for _, s := range []string{"demo", "my-site", "a1", "x1234"} {
		if !validSlug(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range []string{"admin", "www", "-abc", "abc-", "A_upper", "", "a b", "项目"} {
		if validSlug(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}

func TestValidDomain(t *testing.T) {
	for _, s := range []string{"www.example.com", "a-b.cn", "x.io"} {
		if !validDomain(s) {
			t.Errorf("%q 应合法", s)
		}
	}
	for _, s := range []string{"-bad.com", "bad-.com", "localhost", "*-wild.com", "a b.com", ""} {
		if validDomain(s) {
			t.Errorf("%q 应非法", s)
		}
	}
}

func TestNormalizeSlug(t *testing.T) {
	cases := map[string]string{
		"我的 主页":       "",
		"Hello World": "hello-world",
		"  A_b-C  ":   "a-b-c",
	}
	for in, want := range cases {
		if got := normalizeSlug(in); got != want {
			t.Errorf("normalizeSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafeNext(t *testing.T) {
	for _, s := range []string{"/projects/1", "/"} {
		if safeNext(s) != s {
			t.Errorf("safeNext(%q) 应原样返回", s)
		}
	}
	for _, s := range []string{"", "//evil.com", "http://evil.com"} {
		if got := safeNext(s); got != "/" {
			t.Errorf("safeNext(%q) = %q, 应回退 /", s, got)
		}
	}
}
