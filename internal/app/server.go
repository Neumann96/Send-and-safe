package app

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxChunkBytes = int64(4*1024*1024 + 16)
	maxChunks     = 128
	transferTTL   = 48 * time.Hour
)

type Config struct {
	Addr            string
	DataDir         string
	WebDir          string
	MaxFileBytes    int64
	MaxStorageBytes int64
	CleanupPeriod   time.Duration
}

type Server struct {
	config Config
	db     *sql.DB
	http   *http.Server
}

type CryptoManifest struct {
	Version           int    `json:"version"`
	AccessMode        string `json:"accessMode"`
	NoncePrefix       string `json:"noncePrefix"`
	MetadataNonce     string `json:"metadataNonce"`
	EncryptedMetadata string `json:"encryptedMetadata"`
	KeySalt           string `json:"keySalt,omitempty"`
	KeyNonce          string `json:"keyNonce,omitempty"`
	EncryptedKey      string `json:"encryptedKey,omitempty"`
	KDFIterations     int    `json:"kdfIterations,omitempty"`
}

type transfer struct {
	ID             string
	TokenHash      string
	ClaimTokenHash string
	PlainSize      int64
	ChunkSize      int64
	ChunkCount     int
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ClaimedAt      *time.Time
	Complete       bool
	Crypto         CryptoManifest
}

type createRequest struct {
	PlainSize  int64          `json:"plainSize"`
	ChunkSize  int64          `json:"chunkSize"`
	ChunkCount int            `json:"chunkCount"`
	Crypto     CryptoManifest `json:"crypto"`
}

type publicManifest struct {
	ID         string         `json:"id"`
	PlainSize  int64          `json:"plainSize"`
	ChunkSize  int64          `json:"chunkSize"`
	ChunkCount int            `json:"chunkCount"`
	ExpiresAt  time.Time      `json:"expiresAt"`
	Crypto     CryptoManifest `json:"crypto"`
}

func New(config Config) (*Server, error) {
	if config.Addr == "" || config.DataDir == "" || config.MaxFileBytes <= 0 ||
		config.MaxStorageBytes <= 0 {
		return nil, errors.New("invalid server configuration")
	}
	if config.CleanupPeriod <= 0 {
		config.CleanupPeriod = time.Minute
	}
	if err := os.MkdirAll(filepath.Join(config.DataDir, "chunks"), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}

	db, err := sql.Open("sqlite", filepath.Join(config.DataDir, "transfers.db"))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		PRAGMA journal_mode = WAL;
		PRAGMA busy_timeout = 5000;
		CREATE TABLE IF NOT EXISTS transfers (
			id TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL,
			plain_size INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL,
			chunk_count INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			complete INTEGER NOT NULL DEFAULT 0,
			crypto_json TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS transfers_expires_at ON transfers(expires_at);
	`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize sqlite: %w", err)
	}
	if err := ensureColumn(db, "transfers", "claimed_at", "INTEGER"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite claimed_at: %w", err)
	}
	if err := ensureColumn(db, "transfers", "claim_token_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate sqlite claim_token_hash: %w", err)
	}

	server := &Server{config: config, db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/transfers", server.handleTransfers)
	mux.HandleFunc("/api/transfers/", server.handleTransfer)
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":          "ok",
			"ttlHours":        int(transferTTL.Hours()),
			"maxFileBytes":    config.MaxFileBytes,
			"maxStorageBytes": config.MaxStorageBytes,
		})
	})
	mux.Handle("/", server.webHandler())

	server.http = &http.Server{
		Addr:              config.Addr,
		Handler:           securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go server.cleanupLoop()
	return server, nil
}

func (s *Server) Addr() string          { return s.http.Addr }
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }
func (s *Server) Handler() http.Handler { return s.http.Handler }
func (s *Server) Close() error          { return s.db.Close() }

func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request createRequest
	if err := decodeJSON(r, &request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.validateCreate(request); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, err := randomString(18)
	if err != nil {
		http.Error(w, "could not create transfer", http.StatusInternalServerError)
		return
	}
	token, err := randomString(32)
	if err != nil {
		http.Error(w, "could not create transfer", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	item := transfer{
		ID: id, TokenHash: hashToken(token), PlainSize: request.PlainSize,
		ChunkSize: request.ChunkSize, ChunkCount: request.ChunkCount,
		CreatedAt: now, ExpiresAt: now.Add(transferTTL), Crypto: request.Crypto,
	}
	if err := s.reserveStorage(item); err != nil {
		if errors.Is(err, errStorageFull) {
			http.Error(w, "storage capacity is temporarily exhausted", http.StatusInsufficientStorage)
			return
		}
		http.Error(w, "could not reserve storage", http.StatusInternalServerError)
		return
	}
	if err := os.MkdirAll(s.transferDir(id), 0o700); err != nil {
		_, _ = s.db.Exec(`DELETE FROM transfers WHERE id = ?`, id)
		http.Error(w, "could not create transfer", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "uploadToken": token, "expiresAt": item.ExpiresAt,
	})
}

func (s *Server) handleTransfer(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/transfers/"), "/")
	if len(parts) == 0 || !validOpaqueID(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch {
	case len(parts) == 1 && r.Method == http.MethodGet:
		s.getManifest(w, r, id)
	case len(parts) == 1 && r.Method == http.MethodDelete:
		s.deleteTransfer(w, r, id)
	case len(parts) == 2 && parts[1] == "complete" && r.Method == http.MethodPost:
		s.completeTransfer(w, r, id)
	case len(parts) == 2 && parts[1] == "claim" && r.Method == http.MethodPost:
		s.claimTransfer(w, r, id)
	case len(parts) == 2 && parts[1] == "consume" && r.Method == http.MethodPost:
		s.consumeTransfer(w, r, id)
	case len(parts) == 3 && parts[1] == "chunks" && r.Method == http.MethodPut:
		s.putChunk(w, r, id, parts[2])
	case len(parts) == 3 && parts[1] == "chunks" && r.Method == http.MethodGet:
		s.getChunk(w, r, id, parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) putChunk(w http.ResponseWriter, r *http.Request, id, rawIndex string) {
	index, err := strconv.Atoi(rawIndex)
	if err != nil {
		http.Error(w, "invalid chunk index", http.StatusBadRequest)
		return
	}
	item, err := s.loadTransfer(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if item.Complete || time.Now().After(item.ExpiresAt) {
		http.Error(w, "transfer is not writable", http.StatusConflict)
		return
	}
	if !authorized(r, item.TokenHash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if index < 0 || index >= item.ChunkCount {
		http.Error(w, "invalid chunk index", http.StatusBadRequest)
		return
	}
	expected := item.ChunkSize + 16
	if index == item.ChunkCount-1 {
		expected = item.PlainSize - int64(index)*item.ChunkSize + 16
	}
	if expected <= 16 || expected > maxChunkBytes {
		http.Error(w, "invalid chunk size", http.StatusBadRequest)
		return
	}
	path := s.chunkPath(id, index)
	temp := path + ".tmp"
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		http.Error(w, "could not store chunk", http.StatusInternalServerError)
		return
	}
	written, copyErr := io.Copy(file, io.LimitReader(r.Body, expected+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != expected {
		_ = os.Remove(temp)
		http.Error(w, "unexpected encrypted chunk size", http.StatusBadRequest)
		return
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		http.Error(w, "could not store chunk", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) completeTransfer(w http.ResponseWriter, r *http.Request, id string) {
	item, err := s.loadTransfer(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !authorized(r, item.TokenHash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if time.Now().After(item.ExpiresAt) {
		http.Error(w, "transfer expired", http.StatusGone)
		return
	}
	for index := 0; index < item.ChunkCount; index++ {
		if _, err := os.Stat(s.chunkPath(id, index)); err != nil {
			http.Error(w, fmt.Sprintf("chunk %d is missing", index), http.StatusConflict)
			return
		}
	}
	if _, err := s.db.Exec(`UPDATE transfers SET complete = 1 WHERE id = ?`, id); err != nil {
		http.Error(w, "could not complete transfer", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getManifest(w http.ResponseWriter, r *http.Request, id string) {
	item, err := s.loadTransfer(id)
	if err != nil || !item.Complete || item.ClaimedAt != nil {
		http.NotFound(w, r)
		return
	}
	if time.Now().After(item.ExpiresAt) {
		http.Error(w, "transfer expired", http.StatusGone)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, publicManifest{
		ID: item.ID, PlainSize: item.PlainSize, ChunkSize: item.ChunkSize,
		ChunkCount: item.ChunkCount, ExpiresAt: item.ExpiresAt, Crypto: item.Crypto,
	})
}

func (s *Server) claimTransfer(w http.ResponseWriter, r *http.Request, id string) {
	token, err := randomString(32)
	if err != nil {
		http.Error(w, "could not claim transfer", http.StatusInternalServerError)
		return
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`
		UPDATE transfers
		SET claimed_at = ?, claim_token_hash = ?
		WHERE id = ? AND complete = 1 AND claimed_at IS NULL AND expires_at > ?`,
		now.Unix(), hashToken(token), id, now.Unix(),
	)
	if err != nil {
		http.Error(w, "could not claim transfer", http.StatusInternalServerError)
		return
	}
	affected, err := result.RowsAffected()
	if err != nil {
		http.Error(w, "could not claim transfer", http.StatusInternalServerError)
		return
	}
	if affected != 1 {
		http.Error(w, "transfer was already received or expired", http.StatusGone)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"downloadToken": token})
}

func (s *Server) getChunk(w http.ResponseWriter, r *http.Request, id, rawIndex string) {
	index, err := strconv.Atoi(rawIndex)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	item, err := s.loadTransfer(id)
	if err != nil || !item.Complete || item.ClaimedAt == nil ||
		index < 0 || index >= item.ChunkCount {
		http.NotFound(w, r)
		return
	}
	if !authorized(r, item.ClaimTokenHash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if time.Now().After(item.ExpiresAt) {
		http.Error(w, "transfer expired", http.StatusGone)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, s.chunkPath(id, index))
}

func (s *Server) consumeTransfer(w http.ResponseWriter, r *http.Request, id string) {
	item, err := s.loadTransfer(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if item.ClaimedAt == nil || !authorized(r, item.ClaimTokenHash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.removeTransfer(id); err != nil {
		http.Error(w, "could not consume transfer", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTransfer(w http.ResponseWriter, r *http.Request, id string) {
	item, err := s.loadTransfer(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !authorized(r, item.TokenHash) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if err := s.removeTransfer(id); err != nil {
		http.Error(w, "could not delete transfer", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) validateCreate(request createRequest) error {
	if request.PlainSize <= 0 || request.PlainSize > s.config.MaxFileBytes {
		return fmt.Errorf("file size must be between 1 and %d bytes", s.config.MaxFileBytes)
	}
	if request.ChunkSize <= 0 || request.ChunkSize+16 > maxChunkBytes {
		return errors.New("invalid chunk size")
	}
	expectedChunks := int((request.PlainSize + request.ChunkSize - 1) / request.ChunkSize)
	if request.ChunkCount != expectedChunks || request.ChunkCount > maxChunks {
		return errors.New("invalid chunk count")
	}
	crypto := request.Crypto
	if crypto.Version != 1 || (crypto.AccessMode != "link" && crypto.AccessMode != "code") {
		return errors.New("unsupported crypto configuration")
	}
	if !validBase64URL(crypto.NoncePrefix, 8) ||
		!validBase64URL(crypto.MetadataNonce, 12) ||
		!validCiphertext(crypto.EncryptedMetadata, 4096) {
		return errors.New("invalid crypto manifest")
	}
	if crypto.AccessMode == "code" {
		if !validBase64URL(crypto.KeySalt, 16) ||
			!validBase64URL(crypto.KeyNonce, 12) ||
			!validBase64URL(crypto.EncryptedKey, 48) ||
			crypto.KDFIterations < 200_000 || crypto.KDFIterations > 1_000_000 {
			return errors.New("invalid code protection")
		}
	}
	return nil
}

var errStorageFull = errors.New("storage capacity exhausted")

func (s *Server) reserveStorage(item transfer) error {
	cryptoJSON, err := json.Marshal(item.Crypto)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var reserved int64
	if err := tx.QueryRow(`
		SELECT COALESCE(SUM(plain_size + chunk_count * 16), 0)
		FROM transfers WHERE expires_at > ?`, time.Now().Unix(),
	).Scan(&reserved); err != nil {
		return err
	}
	required := item.PlainSize + int64(item.ChunkCount)*16
	if reserved > s.config.MaxStorageBytes-required {
		return errStorageFull
	}
	if _, err = tx.Exec(`
		INSERT INTO transfers
			(id, token_hash, plain_size, chunk_size, chunk_count, created_at, expires_at, complete, crypto_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		item.ID, item.TokenHash, item.PlainSize, item.ChunkSize, item.ChunkCount,
		item.CreatedAt.Unix(), item.ExpiresAt.Unix(), string(cryptoJSON),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Server) loadTransfer(id string) (transfer, error) {
	var item transfer
	var createdAt, expiresAt int64
	var claimedAt sql.NullInt64
	var complete int
	var cryptoJSON string
	err := s.db.QueryRow(`
		SELECT id, token_hash, plain_size, chunk_size, chunk_count,
			created_at, expires_at, claimed_at, claim_token_hash, complete, crypto_json
		FROM transfers WHERE id = ?`, id,
	).Scan(
		&item.ID, &item.TokenHash, &item.PlainSize, &item.ChunkSize, &item.ChunkCount,
		&createdAt, &expiresAt, &claimedAt, &item.ClaimTokenHash, &complete, &cryptoJSON,
	)
	if err != nil {
		return item, err
	}
	item.CreatedAt = time.Unix(createdAt, 0).UTC()
	item.ExpiresAt = time.Unix(expiresAt, 0).UTC()
	if claimedAt.Valid {
		value := time.Unix(claimedAt.Int64, 0).UTC()
		item.ClaimedAt = &value
	}
	item.Complete = complete == 1
	if err := json.Unmarshal([]byte(cryptoJSON), &item.Crypto); err != nil {
		return item, err
	}
	return item, nil
}

func (s *Server) removeTransfer(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM transfers WHERE id = ?`, id); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := os.RemoveAll(s.transferDir(id)); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func (s *Server) transferDir(id string) string {
	return filepath.Join(s.config.DataDir, "chunks", id)
}

func (s *Server) chunkPath(id string, index int) string {
	return filepath.Join(s.transferDir(id), fmt.Sprintf("%04d.chunk", index))
}

func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(s.config.CleanupPeriod)
	defer ticker.Stop()
	for range ticker.C {
		if err := s.cleanupExpired(); err != nil {
			log.Printf("cleanup failed: %v", err)
		}
	}
}

func (s *Server) cleanupExpired() error {
	rows, err := s.db.Query(`SELECT id FROM transfers WHERE expires_at <= ?`, time.Now().Unix())
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := s.removeTransfer(id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) webHandler() http.Handler {
	if s.config.WebDir == "" {
		return http.NotFoundHandler()
	}
	web := http.Dir(s.config.WebDir)
	files := http.FileServer(web)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if path == "." {
			path = "index.html"
		}
		if _, err := fs.Stat(os.DirFS(s.config.WebDir), filepath.ToSlash(path)); err == nil {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(s.config.WebDir, "index.html"))
	})
}

func decodeJSON(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func randomString(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func authorized(r *http.Request, expectedHash string) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	actual := hashToken(strings.TrimPrefix(header, prefix))
	return subtle.ConstantTimeCompare([]byte(actual), []byte(expectedHash)) == 1
}

func validOpaqueID(value string) bool {
	if len(value) < 20 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') &&
			!(char >= 'A' && char <= 'Z') &&
			!(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func validBase64URL(value string, expectedBytes int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) == expectedBytes
}

func validCiphertext(value string, maxBytes int) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	return err == nil && len(decoded) >= 16 && len(decoded) <= maxBytes
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' https://telegram.org; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors https://web.telegram.org https://*.telegram.org")
		next.ServeHTTP(w, r)
	})
}

func ensureColumn(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column + ` ` + definition)
	return err
}
