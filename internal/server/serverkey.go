package server

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// serverKeyFile is the filename, under the server data-dir, holding the
// ed25519 signing seed (base64 std of the 32-byte seed). This one key both
// signs feed/response data and (via ed25519->curve25519 conversion on the
// client) lets clients encrypt routing metadata to the server.
const serverKeyFile = "server_ed25519.key"

// LoadOrCreateServerKey returns the server's ed25519 private key, reading it
// from <dataDir>/server_ed25519.key or generating and persisting a new one
// if the file is absent. The accompanying public key (base64url) is what the
// operator pins in configs — print it with ServerPublicKeyString.
//
// The file format is base64-std of the 32-byte seed. A raw 32-byte seed is
// also accepted, so an operator can drop in a key produced elsewhere.
func LoadOrCreateServerKey(dataDir string) (ed25519.PrivateKey, error) {
	path := filepath.Join(dataDir, serverKeyFile)

	if raw, err := os.ReadFile(path); err == nil {
		seed, derr := decodeSeed(raw)
		if derr != nil {
			return nil, fmt.Errorf("server key %s: %w", path, derr)
		}
		return ed25519.NewKeyFromSeed(seed), nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read server key %s: %w", path, err)
	}

	// Absent: generate and persist.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(priv.Seed())
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write server key %s: %w", path, err)
	}
	return priv, nil
}

// decodeSeed parses a 32-byte ed25519 seed from base64-std text or raw bytes.
func decodeSeed(raw []byte) ([]byte, error) {
	if len(raw) == ed25519.SeedSize {
		return raw, nil
	}
	s := strings.TrimSpace(string(raw))
	seed, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("not base64 and not a %d-byte raw seed: %w", ed25519.SeedSize, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize)
	}
	return seed, nil
}

// serverEncKeyFile holds the x25519 chat encryption private key (base64 std
// of the 32-byte scalar). Clients learn the matching public key from the
// signed ChatInfo payload, not from the import URI.
const serverEncKeyFile = "server_x25519.key"

// LoadOrCreateServerEncKey returns the server's x25519 encryption key,
// reading it from <dataDir>/server_x25519.key or generating and persisting a
// new one if the file is absent.
func LoadOrCreateServerEncKey(dataDir string) (*ecdh.PrivateKey, error) {
	path := filepath.Join(dataDir, serverEncKeyFile)

	if raw, err := os.ReadFile(path); err == nil {
		seed, derr := decodeSeed(raw)
		if derr != nil {
			return nil, fmt.Errorf("server enc key %s: %w", path, derr)
		}
		return ecdh.X25519().NewPrivateKey(seed)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read server enc key %s: %w", path, err)
	}

	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate server enc key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	enc := base64.StdEncoding.EncodeToString(key.Bytes())
	if err := os.WriteFile(path, []byte(enc+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("write server enc key %s: %w", path, err)
	}
	return key, nil
}

// SaveServerEncKey overwrites the persisted x25519 chat key. Used on ek
// rotation so a restart loads the rotated key, not the pre-rotation one. The
// previous key lives on only in RAM (its grace window), which is what gives the
// session layer forward secrecy once it expires.
func SaveServerEncKey(dataDir string, key *ecdh.PrivateKey) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dataDir, serverEncKeyFile)
	enc := base64.StdEncoding.EncodeToString(key.Bytes())
	return os.WriteFile(path, []byte(enc+"\n"), 0o600)
}

// ServerPublicKeyString returns the base64url (no padding) encoding of the
// server public key — the value pinned in configs and bundled defaults.
func ServerPublicKeyString(priv ed25519.PrivateKey) string {
	pub := priv.Public().(ed25519.PublicKey)
	return base64.RawURLEncoding.EncodeToString(pub)
}

// ConfigURI builds the thefeed:// import URI advertising this server's main
// domain (path), passphrase, pinned signing public key (sk=), any extra
// sub-domains (d=, comma-separated), and two bootstrap resolvers so a
// freshly-imported client can reach DNS immediately.
func ConfigURI(domain string, extraDomains []string, passphrase string, priv ed25519.PrivateKey) string {
	params := []string{"sk=" + ServerPublicKeyString(priv)}

	var extra []string
	for _, d := range extraDomains {
		if d = strings.TrimSuffix(strings.TrimSpace(d), "."); d != "" {
			extra = append(extra, d)
		}
	}
	if len(extra) > 0 {
		params = append(params, "d="+uriComponent(strings.Join(extra, ",")))
	}

	// Resolvers (r=) go LAST: if the URI is truncated (long resolver list,
	// lost message tail), only trailing resolvers are dropped — domain, key,
	// sk=, and d= survive.
	params = append(params, "r="+uriComponent("1.1.1.1,8.8.8.8"))

	return fmt.Sprintf("thefeed://%s/%s?%s",
		uriComponent(domain),
		uriComponent(passphrase),
		strings.Join(params, "&"),
	)
}

// uriComponent escapes s like JavaScript's encodeURIComponent, so the
// client's URI parser (which decodeURIComponent's each field) round-trips
// it exactly. Non-ASCII bytes are percent-escaped per UTF-8 byte.
func uriComponent(s string) string {
	const safe = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || strings.IndexByte(safe, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
