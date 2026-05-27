// Package dlc decrypts and parses JDownloader-style DLC link containers.
//
// The DLC format wraps a list of download URLs in a two-stage AES-CBC envelope.
// The outer envelope's key is held by the AppWork dlcrypt web service; we send
// the trailing 88-byte key blob to that service, decrypt the returned key with
// a hardcoded AES key/IV, then use the resulting 16 bytes as both key and IV
// to decrypt the actual XML payload.
package dlc

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultServiceURL is the AppWork dlcrypt service used by pyLoad and friends.
// Override with the DOWNLOADCLEAN_DLC_SERVICE env var or by setting
// Parser.ServiceURL directly.
const DefaultServiceURL = "http://service.jdownloader.org/dlcrypt/service.php?srcType=dlc&destType=pylo&data=%s"

// Hardcoded first-pass AES-CBC key/IV (from pyLoad's DLC plugin).
var (
	firstPassKey = []byte("cb99b5cbc24db398")
	firstPassIV  = []byte("9bc24cb995cb8db3")
)

// Link is a single downloadable entry inside a DLC.
type Link struct {
	URL     string // download URL
	Name    string // filename from the container (optional)
	Size    int64  // size in bytes from the container (0 if unknown)
	Package string // package name from the container (optional)
}

// Parser decrypts DLC files. The zero value is usable.
type Parser struct {
	// ServiceURL is a printf-style format string with one %s placeholder for
	// the URL-encoded key blob. Empty means use DefaultServiceURL (or the
	// DOWNLOADCLEAN_DLC_SERVICE env var if set).
	ServiceURL string
	// HTTPClient overrides the default client used to call the service.
	HTTPClient *http.Client

	defaultOnce   sync.Once
	defaultClient *http.Client
}

func (p *Parser) client() *http.Client {
	if p.HTTPClient != nil {
		return p.HTTPClient
	}
	p.defaultOnce.Do(func() {
		p.defaultClient = &http.Client{Timeout: 30 * time.Second}
	})
	return p.defaultClient
}

func (p *Parser) serviceURL() string {
	if p.ServiceURL != "" {
		return p.ServiceURL
	}
	if env := os.Getenv("DOWNLOADCLEAN_DLC_SERVICE"); env != "" {
		return env
	}
	return DefaultServiceURL
}

// Parse reads a DLC file from r and returns its links.
func (p *Parser) Parse(r io.Reader) ([]Link, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read dlc: %w", err)
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) < 88 {
		return nil, fmt.Errorf("dlc too short (%d bytes)", len(raw))
	}
	keyBlob := raw[len(raw)-88:]
	body := raw[:len(raw)-88]

	derivedKey, err := p.fetchKey(string(keyBlob))
	if err != nil {
		return nil, err
	}

	ciphertext, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		return nil, fmt.Errorf("dlc body base64: %w", err)
	}
	plaintext, err := aesCBCDecrypt(derivedKey, derivedKey, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("dlc body decrypt: %w", err)
	}
	// AES output is itself base64-encoded XML; trim trailing nulls/whitespace
	// because the DLC body uses zero padding, not PKCS#7.
	plaintext = bytes.TrimRight(plaintext, "\x00 \t\r\n")
	xmlBytes, err := base64.StdEncoding.DecodeString(string(plaintext))
	if err != nil {
		return nil, fmt.Errorf("dlc body inner base64: %w", err)
	}
	return parseXML(xmlBytes)
}

// fetchKey calls the dlcrypt service with the 88-byte key blob and returns
// the 16-byte derived key used for the second AES-CBC pass.
func (p *Parser) fetchKey(keyBlob string) ([]byte, error) {
	endpoint := fmt.Sprintf(p.serviceURL(), url.QueryEscape(keyBlob))
	resp, err := p.client().Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("dlc service: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dlc service: HTTP %d", resp.StatusCode)
	}
	xmlBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dlc service: read: %w", err)
	}

	rcText, err := extractRC(xmlBody)
	if err != nil {
		return nil, fmt.Errorf("dlc service: %w (response: %q)", err, truncate(string(xmlBody), 200))
	}

	rc, err := base64.StdEncoding.DecodeString(rcText)
	if err != nil {
		return nil, fmt.Errorf("dlc service <rc> base64: %w", err)
	}
	derived, err := aesCBCDecrypt(firstPassKey, firstPassIV, rc)
	if err != nil {
		return nil, fmt.Errorf("dlc service <rc> decrypt: %w", err)
	}
	if len(derived) < 16 {
		return nil, fmt.Errorf("dlc service <rc>: derived key too short (%d bytes)", len(derived))
	}
	return derived[:16], nil
}

// extractRC finds the first <rc> element in xmlBody and returns its text.
// Tolerates either a bare <rc>...</rc> root or any wrapper around it.
func extractRC(xmlBody []byte) (string, error) {
	dec := xml.NewDecoder(bytes.NewReader(xmlBody))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return "", fmt.Errorf("no <rc> element found")
		}
		if err != nil {
			return "", fmt.Errorf("parse: %w", err)
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "rc" {
			continue
		}
		var text string
		if err := dec.DecodeElement(&text, &se); err != nil {
			return "", fmt.Errorf("decode <rc>: %w", err)
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return "", fmt.Errorf("empty <rc>")
		}
		return text, nil
	}
}

func aesCBCDecrypt(key, iv, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("ciphertext not a multiple of %d (%d)", aes.BlockSize, len(ciphertext))
	}
	out := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, ciphertext)
	return stripPKCS7(out), nil
}

// stripPKCS7 removes PKCS#7 padding if present and well-formed; otherwise it
// returns the input unchanged (the DLC payload is sometimes zero-padded or
// has trailing garbage past the XML close tag — the XML parser tolerates that).
func stripPKCS7(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	pad := int(b[len(b)-1])
	if pad == 0 || pad > aes.BlockSize || pad > len(b) {
		return b
	}
	for i := len(b) - pad; i < len(b); i++ {
		if int(b[i]) != pad {
			return b
		}
	}
	return b[:len(b)-pad]
}

// parseXML extracts links from the decrypted DLC XML body.
// Structure: <dlc><content><package name="b64" ...><file><url>b64</url><filename>b64</filename><size>b64</size></file>...</package>...</content></dlc>
func parseXML(body []byte) ([]Link, error) {
	// Trim anything before the first '<' and after the last '>' so a bit of
	// CBC trailing garbage does not break the parser.
	if i := bytes.IndexByte(body, '<'); i > 0 {
		body = body[i:]
	}
	if i := bytes.LastIndexByte(body, '>'); i >= 0 && i < len(body)-1 {
		body = body[:i+1]
	}

	type fileT struct {
		URL  string `xml:"url"`
		Name string `xml:"filename"`
		Size string `xml:"size"`
	}
	type packageT struct {
		Name  string  `xml:"name,attr"`
		Files []fileT `xml:"file"`
	}
	type contentT struct {
		Packages []packageT `xml:"package"`
	}
	type dlcT struct {
		Content contentT `xml:"content"`
	}

	var doc dlcT
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("dlc xml: %w", err)
	}

	var out []Link
	for _, pkg := range doc.Content.Packages {
		pkgName := decodeB64(pkg.Name)
		for _, f := range pkg.Files {
			urlStr := decodeB64(f.URL)
			if urlStr == "" {
				continue
			}
			link := Link{
				URL:     urlStr,
				Name:    decodeB64(f.Name),
				Package: pkgName,
			}
			if s := decodeB64(f.Size); s != "" {
				if n, err := strconv.ParseInt(s, 10, 64); err == nil {
					link.Size = n
				}
			}
			out = append(out, link)
		}
	}
	return out, nil
}

func decodeB64(s string) string {
	if s == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return ""
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
