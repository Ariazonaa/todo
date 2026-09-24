package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
)

func ceremonyServer() *server {
	return &server{cer: newCeremonies(), cfg: config{insecureCookie: true}}
}

// beginFrom is the begin request of a client with address ip, as it
// arrives via a reverse proxy: X-Real-IP set, connection from the proxy.
func beginFrom(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/begin", nil)
	req.RemoteAddr = "10.0.0.2:40000"
	req.Header.Set("X-Real-IP", ip)
	return req
}

// startCeremony hands out the ceremony as a cookie; we simulate that here.
func withCeremonyCookie(rec *httptest.ResponseRecorder) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/finish", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

func startTestCeremony(t *testing.T, s *server, kind, challenge string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := s.startCeremony(rec, beginFrom("198.51.100.7"), kind, &webauthn.SessionData{Challenge: challenge}); err != nil {
		t.Fatalf("startCeremony: %v", err)
	}
	return rec
}

// The actual point: there used to be one slot per kind, so any
// unauthenticated caller could overwrite the user's in-progress login via
// login/begin — finish would then just keep failing.
func TestForeignCeremonyDoesNotClobberOwn(t *testing.T) {
	s := ceremonyServer()
	mine := startTestCeremony(t, s, "login", "meine")
	for range 2000 {
		if err := s.startCeremony(httptest.NewRecorder(), beginFrom("203.0.113.66"), "login", &webauthn.SessionData{Challenge: "fremd"}); err != nil {
			t.Fatalf("startCeremony (fremd): %v", err)
		}
	}
	tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(mine), "login")
	if tk == nil || tk.sd.Challenge != "meine" {
		t.Fatalf("eigene Zeremonie verloren oder vertauscht: %+v", tk)
	}
}

// With a global table (1024 slots, 8 per /64), ~128 /64 networks from a
// single free /48 were enough to evict the user's in-progress login. With
// the ceremony stored in the cookie, there is nothing to evict.
func TestCeremonyFloodFromOneIPv6_48DoesNotEvictOwn(t *testing.T) {
	s := ceremonyServer()
	mine := startTestCeremony(t, s, "login", "meine")
	for i := range 1100 {
		ip := fmt.Sprintf("2001:db8:666:%x::1", i)
		if err := s.startCeremony(httptest.NewRecorder(), beginFrom(ip), "login", &webauthn.SessionData{Challenge: "fremd"}); err != nil {
			t.Fatalf("startCeremony (fremd): %v", err)
		}
	}
	if tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(mine), "login"); tk == nil || tk.sd.Challenge != "meine" {
		t.Fatal("eigene Zeremonie wurde von einer Flut aus einem /48 verdrängt")
	}
}

func TestCeremonyIsSingleUse(t *testing.T) {
	s := ceremonyServer()
	rec := startTestCeremony(t, s, "login", "x")
	tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login")
	if tk == nil || !s.cer.consume(tk) {
		t.Fatal("erstes Einlösen muss klappen")
	}
	if s.cer.consume(tk) {
		t.Fatal("dasselbe Ticket darf nicht zweimal eingelöst werden")
	}
	if tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login"); tk != nil {
		t.Fatal("eine eingelöste Zeremonie darf sich nicht wieder öffnen lassen")
	}
}

// A registration ceremony must not unlock a login.
func TestCeremonyKindIsChecked(t *testing.T) {
	s := ceremonyServer()
	rec := startTestCeremony(t, s, "register", "x")
	if tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login"); tk != nil {
		t.Fatal("Zeremonie der falschen Art wurde akzeptiert")
	}
}

func TestCeremonyWithoutCookieIsRejected(t *testing.T) {
	s := ceremonyServer()
	startTestCeremony(t, s, "login", "x")
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/finish", nil)
	if tk := s.takeCeremony(httptest.NewRecorder(), req, "login"); tk != nil {
		t.Fatal("ohne Cookie darf keine Zeremonie eingelöst werden")
	}
	req.AddCookie(&http.Cookie{Name: ceremonyCookie, Value: "geraten"})
	if tk := s.takeCeremony(httptest.NewRecorder(), req, "login"); tk != nil {
		t.Fatal("ein geratener Wert darf nicht funktionieren")
	}
}

// The cookie carries the state — a single altered byte must be caught.
func TestCeremonyCookieIsTamperProof(t *testing.T) {
	s := ceremonyServer()
	rec := startTestCeremony(t, s, "login", "x")
	c := rec.Result().Cookies()[0]
	b := []byte(c.Value)
	i := len(b) / 2
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/finish", nil)
	req.AddCookie(&http.Cookie{Name: ceremonyCookie, Value: string(b)})
	if tk := s.takeCeremony(httptest.NewRecorder(), req, "login"); tk != nil {
		t.Fatal("verändertes Zeremonie-Cookie wurde akzeptiert")
	}
}

// A cookie from another instance (or from before a restart) is not valid.
func TestCeremonyFromOtherServerIsRejected(t *testing.T) {
	rec := startTestCeremony(t, ceremonyServer(), "login", "x")
	if tk := ceremonyServer().takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login"); tk != nil {
		t.Fatal("Zeremonie eines anderen Schlüssels wurde akzeptiert")
	}
}

func TestCeremonyExpires(t *testing.T) {
	s := ceremonyServer()
	rec := startTestCeremony(t, s, "login", "x")
	s.cer.now = func() time.Time { return time.Now().Add(ceremonyTTL + time.Second) }
	if tk := s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login"); tk != nil {
		t.Fatal("abgelaufene Zeremonie wurde akzeptiert")
	}
}

// The list of consumed ceremonies (for single-use enforcement) stays
// capped.
func TestCeremonyUsedListIsBounded(t *testing.T) {
	s := ceremonyServer()
	for i := range maxUsedCeremonies + 100 {
		rec := startTestCeremony(t, s, "login", fmt.Sprint(i))
		s.cer.consume(s.takeCeremony(httptest.NewRecorder(), withCeremonyCookie(rec), "login"))
	}
	s.cer.mu.Lock()
	n := len(s.cer.used)
	s.cer.mu.Unlock()
	if n > maxUsedCeremonies {
		t.Fatalf("%d eingelöste Zeremonien gemerkt, Limit %d", n, maxUsedCeremonies)
	}
}

// Passkey-only: without VerificationRequired, go-webauthn does not check
// PIN or biometrics at all. For new registrations the setting applies
// directly; for login it's checked per passkey (see
// TestLoginRequiresUVForPasskeysThatHaveIt).
func TestWebAuthnRequiresUserVerification(t *testing.T) {
	s := ceremonyServer()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login/begin", nil)
	req.Host = "todo.example.org"
	wan, err := s.wan(req)
	if err != nil {
		t.Fatalf("wan: %v", err)
	}
	if got := wan.Config.AuthenticatorSelection.UserVerification; got != protocol.VerificationRequired {
		t.Fatalf("UserVerification = %q, erwartet %q", got, protocol.VerificationRequired)
	}
}

func TestStartCeremonySetsHardenedCookie(t *testing.T) {
	s := &server{cer: newCeremonies(), cfg: config{}} // insecureCookie off
	rec := startTestCeremony(t, s, "login", "x")

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("erwartet genau ein Cookie, bekam %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != "__Host-ceremony" || c.Value == "" || c.Path != "/" {
		t.Fatalf("Cookie %q = %q", c.Name, c.Value)
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("Cookie nicht gehärtet: HttpOnly=%v Secure=%v SameSite=%v", c.HttpOnly, c.Secure, c.SameSite)
	}
	if c.MaxAge != int(ceremonyTTL.Seconds()) {
		t.Fatalf("MaxAge = %d", c.MaxAge)
	}
}
