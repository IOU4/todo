package db

import (
	"strings"
	"testing"
)

func TestParseDatabaseURLDecodesTokenAndRemovesItFromEndpoint(t *testing.T) {
	const token = "token+with/encoded=characters&more"
	config, err := ParseDatabaseURL("libsql://my-db.turso.io?authToken=token%2Bwith%2Fencoded%3Dcharacters%26more")
	if err != nil {
		t.Fatal(err)
	}
	if config.URL != "libsql://my-db.turso.io" {
		t.Fatalf("endpoint URL = %q, want credential-free URL", config.URL)
	}
	if config.AuthToken != token {
		t.Fatalf("decoded token = %q, want %q", config.AuthToken, token)
	}
	if strings.Contains(config.URL, token) {
		t.Fatal("endpoint URL retained the auth token")
	}
}

func TestParseDatabaseURLAcceptsDriverSnakeCaseAlias(t *testing.T) {
	config, err := ParseDatabaseURL("libsql://my-db.turso.io?auth_token=token%2Bvalue")
	if err != nil {
		t.Fatal(err)
	}
	if config.AuthToken != "token+value" {
		t.Fatalf("decoded token = %q, want %q", config.AuthToken, "token+value")
	}
}

func TestParseDatabaseURLRejectsInvalidOrCredentialAmbiguousURLs(t *testing.T) {
	const secret = "do-not-leak-this-token"
	cases := []string{
		"",
		"mysql://user:password@host/db?authToken=" + secret,
		"libsql://host.turso.io",
		"libsql://host.turso.io?authToken=",
		"libsql://host.turso.io?authToken=" + secret + "&authToken=second",
		"libsql://host.turso.io?authToken=" + secret + "&auth_token=second",
		"libsql://host.turso.io?authToken=" + secret + "&other=value",
		"libsql://host.turso.io/path?authToken=" + secret,
		"libsql://host.turso.io?authToken=%ZZ",
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			_, err := ParseDatabaseURL(raw)
			if err == nil {
				t.Fatal("ParseDatabaseURL succeeded, want an error")
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaked credential: %v", err)
			}
		})
	}
}

func TestListNamesAreRestricted(t *testing.T) {
	for _, list := range []string{ListShopping, ListTodo} {
		if !ValidListName(list) {
			t.Errorf("ValidListName(%q) = false", list)
		}
	}
	for _, list := range []string{"", "SHOPPING", "mysql", "shopping/todo"} {
		if ValidListName(list) {
			t.Errorf("ValidListName(%q) = true", list)
		}
	}
}

func TestValidItemTextTrimsAndEnforcesUnicodeCodePointLimit(t *testing.T) {
	text, err := ValidItemText(" \t買い物 🛒 ")
	if err != nil {
		t.Fatal(err)
	}
	if text != "買い物 🛒" {
		t.Fatalf("trimmed item = %q", text)
	}

	if _, err := ValidItemText(strings.Repeat("界", MaxItemRunes)); err != nil {
		t.Fatalf("exactly %d Unicode code points should pass: %v", MaxItemRunes, err)
	}
	if _, err := ValidItemText(strings.Repeat("🛒", MaxItemRunes)); err != nil {
		t.Fatalf("exactly %d non-BMP code points should pass: %v", MaxItemRunes, err)
	}
	if _, err := ValidItemText(strings.Repeat("a", MaxItemRunes+1)); err == nil {
		t.Fatal("text over the character limit was accepted")
	}
	if _, err := ValidItemText(" \t\n "); err == nil {
		t.Fatal("whitespace-only text was accepted")
	}
}
