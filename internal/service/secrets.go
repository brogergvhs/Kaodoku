package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brogergvhs/kaodoku/internal/database"
	"github.com/brogergvhs/kaodoku/internal/util"
)

// encPrefix marks encrypted values; stored strings without it are legacy
// plaintext and get re-encrypted on the next bootstrap.
const encPrefix = "enc:v1:"

// tokenCipher encrypts small secrets (tracker tokens) at rest with
// AES-256-GCM. The key comes from KAODOKU_ENCRYPTION_KEY (64 hex chars) or an
// auto-generated kaodoku.key file next to the database.
type tokenCipher struct {
	aead cipher.AEAD
	key  []byte
}

func newTokenCipher(dbPath string) (*tokenCipher, error) {
	key, err := loadOrCreateKey(dbPath)
	if err != nil {
		return nil, fmt.Errorf("encryption key: %w", err)
	}
	return cipherFromKey(key)
}

func cipherFromKey(key []byte) (*tokenCipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &tokenCipher{aead: aead, key: key}, nil
}

func keyFilePath(dbPath string) string {
	return filepath.Join(filepath.Dir(dbPath), "kaodoku.key")
}

func loadOrCreateKey(dbPath string) ([]byte, error) {
	if env := strings.TrimSpace(os.Getenv("KAODOKU_ENCRYPTION_KEY")); env != "" {
		key, err := hex.DecodeString(env)
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("KAODOKU_ENCRYPTION_KEY must be 64 hex characters")
		}
		return key, nil
	}
	path := keyFilePath(dbPath)
	if data, err := os.ReadFile(path); err == nil {
		key, err := hex.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is corrupt; delete it to generate a new key (stored tokens will need reconnecting)", path)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

// Sign is a short HMAC of v, for URLs only the server should mint.
func (c *tokenCipher) Sign(v string) string {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(v))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

func (c *tokenCipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return encPrefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt returns stored values transparently: encrypted ones are opened,
// legacy plaintext passes through unchanged.
func (c *tokenCipher) Decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, encPrefix) {
		return stored, nil
	}
	sealed, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, encPrefix))
	if err != nil {
		return "", err
	}
	if len(sealed) < c.aead.NonceSize() {
		return "", fmt.Errorf("encrypted value too short")
	}
	plain, err := c.aead.Open(nil, sealed[:c.aead.NonceSize()], sealed[c.aead.NonceSize():], nil)
	if err != nil {
		return "", fmt.Errorf("decrypt token (was the key file replaced?): %w", err)
	}
	return string(plain), nil
}

// RotateEncryptionKey mints a fresh key file, re-encrypts stored secrets with
// it, and returns how many were rewritten. Refused when the key is pinned via
// KAODOKU_ENCRYPTION_KEY. Run with the server stopped.
func RotateEncryptionKey(ctx context.Context, dbPath string) (int, error) {
	if strings.TrimSpace(os.Getenv("KAODOKU_ENCRYPTION_KEY")) != "" {
		return 0, fmt.Errorf("encryption key is pinned via KAODOKU_ENCRYPTION_KEY; rotate it by setting a new value in that env var")
	}
	if dbPath == "" {
		dbPath = database.DefaultPath()
	}
	oldKey, err := loadOrCreateKey(dbPath)
	if err != nil {
		return 0, err
	}
	oldCipher, err := cipherFromKey(oldKey)
	if err != nil {
		return 0, err
	}
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		return 0, err
	}
	newCipher, err := cipherFromKey(newKey)
	if err != nil {
		return 0, err
	}

	db, err := database.Open(ctx, dbPath)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return 0, fmt.Errorf("lock database (is the server running?): %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(ctx, "ROLLBACK")
		}
	}()

	rows, err := conn.QueryContext(ctx, `SELECT rowid, access_token, refresh_token FROM tracker_accounts`)
	if err != nil {
		return 0, err
	}
	type item struct {
		id              int64
		access, refresh string
	}
	var items []item
	reenc := func(stored string) (string, error) {
		if !strings.HasPrefix(stored, encPrefix) {
			return stored, nil
		}
		plain, err := oldCipher.Decrypt(stored)
		if err != nil {
			return "", err
		}
		return newCipher.Encrypt(plain)
	}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.access, &it.refresh); err != nil {
			rows.Close()
			return 0, err
		}
		var err error
		if it.access, err = reenc(it.access); err == nil {
			it.refresh, err = reenc(it.refresh)
		}
		if err != nil {
			rows.Close()
			return 0, fmt.Errorf("re-encrypt tracker token %d: %w", it.id, err)
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, it := range items {
		if _, err := conn.ExecContext(ctx, `UPDATE tracker_accounts SET access_token = ?, refresh_token = ? WHERE rowid = ?`, it.access, it.refresh, it.id); err != nil {
			return 0, err
		}
	}

	keyPath := keyFilePath(dbPath)
	bakPath := keyPath + ".bak"
	if err := util.WriteFileAtomic(bakPath, []byte(hex.EncodeToString(oldKey)+"\n"), 0o600); err != nil {
		return 0, fmt.Errorf("back up key: %w", err)
	}
	if err := util.WriteFileAtomic(keyPath, []byte(hex.EncodeToString(newKey)+"\n"), 0o600); err != nil {
		return 0, fmt.Errorf("write new key: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_ = util.WriteFileAtomic(keyPath, []byte(hex.EncodeToString(oldKey)+"\n"), 0o600)
		return 0, err
	}
	committed = true
	_ = os.Remove(bakPath)
	return len(items), nil
}
