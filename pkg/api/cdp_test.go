package api

import "testing"

func TestLoginResultFromCDPCookiesPairsWithinOneDomain(t *testing.T) {
	cookies := []cdpCookie{
		{Name: "auth_token", Value: "tw-auth", Domain: ".twitter.com"},
		{Name: "ct0", Value: "x-ct0", Domain: ".x.com"},
		{Name: "auth_token", Value: "x-auth", Domain: ".x.com", Expires: 1900000000},
		{Name: "other", Value: "ignored", Domain: ".example.com"},
	}
	got, err := loginResultFromCDPCookies(cookies)
	if err != nil {
		t.Fatal(err)
	}
	if got.AuthToken != "x-auth" || got.CT0 != "x-ct0" || got.CookieDomain != ".x.com" {
		t.Fatalf("mixed domains: %+v", got)
	}
	if got.ExpiresAt.Unix() != 1900000000 {
		t.Fatalf("expiry not carried: %v", got.ExpiresAt)
	}
}

func TestLoginResultFromCDPCookiesRejectsHalfPair(t *testing.T) {
	_, err := loginResultFromCDPCookies([]cdpCookie{{Name: "auth_token", Value: "a", Domain: ".x.com"}})
	if err == nil {
		t.Fatal("auth_token without ct0 must not produce a session")
	}
}

func TestLoginResultFromCDPCookiesIgnoresForeignDomains(t *testing.T) {
	_, err := loginResultFromCDPCookies([]cdpCookie{
		{Name: "auth_token", Value: "a", Domain: ".notx.com"},
		{Name: "ct0", Value: "c", Domain: ".notx.com"},
	})
	if err == nil {
		t.Fatal("lookalike domain accepted")
	}
}
