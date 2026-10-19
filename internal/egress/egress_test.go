package egress

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRedirectDoesNotLeakSignedURL(t *testing.T) {
	seen := make(chan string, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Get("Referer")
		fmt.Fprint(w, "ok")
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/file", http.StatusFound)
	}))
	defer source.Close()
	client := source.Client()
	client.CheckRedirect = checkRedirect
	resp, err := client.Get(source.URL + "/private?token=test-secret")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if referer := <-seen; referer != "" {
		t.Fatalf("redirect disclosed Referer: %q", referer)
	}
}

func TestRedirectRejectsDowngradeAndEmbeddedCredentials(t *testing.T) {
	previous, _ := http.NewRequest(http.MethodGet, "https://portal.example/login", nil)
	for _, raw := range []string{"http://portal.example/login", "https://user:password@portal.example/login", "ftp://portal.example/file"} {
		next, err := http.NewRequest(http.MethodGet, raw, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := checkRedirect(next, []*http.Request{previous}); err == nil {
			t.Errorf("accepted unsafe redirect %s", raw)
		}
	}
}

func TestChooseDNSUsesOnlySelectedAdapter(t *testing.T) {
	got, err := chooseDNS(42, "", func(index int) (string, error) {
		if index != 42 {
			t.Errorf("wrong adapter index %d", index)
		}
		return "10.20.0.53:53", nil
	})
	if err != nil || got != "10.20.0.53:53" {
		t.Fatalf("got %q, %v", got, err)
	}
	got, err = chooseDNS(42, "", func(int) (string, error) { return "", errors.New("no DNS") })
	if err == nil || got != "" {
		t.Fatal("missing adapter DNS silently fell back")
	}
}

func TestExplicitDNSDoesNotReadOtherAdapters(t *testing.T) {
	got, err := chooseDNS(42, "10.20.0.53:5353", func(int) (string, error) {
		t.Fatal("explicit DNS unexpectedly triggered adapter lookup")
		return "", nil
	})
	if err != nil || got != "10.20.0.53:5353" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestChooseDNSRejectsUnboundOrInvalidResolvers(t *testing.T) {
	for _, raw := range []string{"resolver.example:53", "127.0.0.1:53", "0.0.0.0:53", "224.0.0.1:53", "255.255.255.255:53", "[::1]:53", "10.20.0.53:domain", "10.20.0.53:0", "10.20.0.53:65536"} {
		if _, err := chooseDNS(42, raw, nil); err == nil {
			t.Errorf("accepted invalid resolver %q", raw)
		}
	}
}
