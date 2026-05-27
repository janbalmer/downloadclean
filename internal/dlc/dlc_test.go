package dlc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParse_RoundTrip(t *testing.T) {
	// Plaintext XML with two links across two packages.
	urls := []string{
		"https://rapidgator.net/file/aaaaaaaaaaaaaaaa/first.rar.html",
		"https://rapidgator.net/file/bbbbbbbbbbbbbbbb/second.rar.html",
	}
	plaintextXML := fmt.Sprintf(`<dlc><content>`+
		`<package name="%s">`+
		`<file><url>%s</url><filename>%s</filename><size>%s</size></file>`+
		`</package>`+
		`<package name="%s">`+
		`<file><url>%s</url></file>`+
		`</package>`+
		`</content></dlc>`,
		b64("Pkg One"),
		b64(urls[0]), b64("first.rar"), b64("12345"),
		b64("Pkg Two"),
		b64(urls[1]),
	)

	derivedKey := []byte("0123456789ABCDEF") // exactly 16 bytes
	// DLC body = base64(AES-CBC(derivedKey, derivedKey, base64(xml)))
	innerB64 := base64.StdEncoding.EncodeToString([]byte(plaintextXML))
	body := encryptCBC(t, derivedKey, derivedKey, []byte(innerB64))
	bodyB64 := base64.StdEncoding.EncodeToString(body)

	// rc = AES-CBC(firstPassKey, firstPassIV, derivedKey) — derivedKey is one
	// block; encryptCBC pads it with PKCS#7, producing 32 bytes.
	rcBytes := encryptCBC(t, firstPassKey, firstPassIV, derivedKey)
	rcB64 := base64.StdEncoding.EncodeToString(rcBytes)

	// 88-byte key blob (opaque to us; the service returns rc keyed by it).
	keyBlob := strings.Repeat("K", 88)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("data"); got != keyBlob {
			t.Errorf("service: unexpected data param: got %q want %q", got, keyBlob)
		}
		// AppWork's live service returns a bare <rc>…</rc> root element.
		fmt.Fprintf(w, "<rc>%s</rc>", rcB64)
	}))
	defer srv.Close()

	p := &Parser{ServiceURL: srv.URL + "?data=%s"}
	links, err := p.Parse(t.Context(), bytes.NewReader([]byte(bodyB64 + keyBlob)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2: %#v", len(links), links)
	}
	if links[0].URL != urls[0] || links[1].URL != urls[1] {
		t.Errorf("URLs mismatch: %#v", links)
	}
	if links[0].Name != "first.rar" {
		t.Errorf("Name[0] = %q, want %q", links[0].Name, "first.rar")
	}
	if links[0].Size != 12345 {
		t.Errorf("Size[0] = %d, want 12345", links[0].Size)
	}
	if links[0].Package != "Pkg One" || links[1].Package != "Pkg Two" {
		t.Errorf("Packages mismatch: %q / %q", links[0].Package, links[1].Package)
	}
}

func TestParse_ServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := &Parser{ServiceURL: srv.URL + "?data=%s"}
	_, err := p.Parse(t.Context(), bytes.NewReader([]byte(strings.Repeat("A", 200))))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected HTTP 500 in error, got: %v", err)
	}
}

func TestParse_TooShort(t *testing.T) {
	p := &Parser{ServiceURL: "http://unused/?data=%s"}
	_, err := p.Parse(t.Context(), bytes.NewReader([]byte("short")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParse_ServiceURLMissingPlaceholder(t *testing.T) {
	// A URL without exactly one %s would mangle silently via fmt.Sprintf — the
	// validation must reject it.
	p := &Parser{ServiceURL: "http://nope/no-placeholder"}
	_, err := p.Parse(t.Context(), bytes.NewReader([]byte(strings.Repeat("A", 200))))
	if err == nil {
		t.Fatal("expected error for missing placeholder")
	}
	if !strings.Contains(err.Error(), "placeholder") {
		t.Errorf("error should mention placeholder: %v", err)
	}
}

func TestExtractRC(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{`<rc>iO6aeouMEDemy1tCeJsVwA==</rc>`, "iO6aeouMEDemy1tCeJsVwA==", false},
		{`<dlc><rc>abc==</rc></dlc>`, "abc==", false},
		{`<wrap><rc>  spaced  </rc></wrap>`, "spaced", false},
		{`<rc></rc>`, "", true},
		{`<no-rc-here/>`, "", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := extractRC([]byte(c.in))
			if c.err {
				if err == nil {
					t.Errorf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestStripPKCS7(t *testing.T) {
	cases := []struct {
		in, want []byte
	}{
		{[]byte{1, 2, 3, 0x05, 0x05, 0x05, 0x05, 0x05}, []byte{1, 2, 3}},
		{[]byte{0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10, 0x10}, []byte{}},
		{[]byte{1, 2, 3, 4}, []byte{1, 2, 3, 4}},       // pad value 4 but no preceding 4s -> unchanged
		{[]byte{1, 2, 3, 0}, []byte{1, 2, 3, 0}},       // pad value 0 -> unchanged
		{[]byte{1, 2, 3, 0x20}, []byte{1, 2, 3, 0x20}}, // pad value > blocksize -> unchanged
	}
	for i, c := range cases {
		t.Run(fmt.Sprintf("case%d", i), func(t *testing.T) {
			got := stripPKCS7(c.in)
			if !bytes.Equal(got, c.want) {
				t.Errorf("stripPKCS7(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// --- helpers ---

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func encryptCBC(t *testing.T, key, iv, plaintext []byte) []byte {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	// PKCS#7 pad to block size.
	pad := aes.BlockSize - (len(plaintext) % aes.BlockSize)
	padded := append([]byte(nil), plaintext...)
	for i := 0; i < pad; i++ {
		padded = append(padded, byte(pad))
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out
}
