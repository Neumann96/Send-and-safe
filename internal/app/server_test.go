package app

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTransferLifecycleUsesFixedTTL(t *testing.T) {
	server := newTestServer(t)
	requestBody, _ := json.Marshal(createRequest{
		PlainSize: 5, ChunkSize: 5, ChunkCount: 1,
		Crypto: validLinkCrypto(),
	})
	createResponse := request(server, http.MethodPost, "/api/transfers", requestBody, "")
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createResponse.Code, createResponse.Body)
	}

	var created struct {
		ID          string    `json:"id"`
		UploadToken string    `json:"uploadToken"`
		ExpiresAt   time.Time `json:"expiresAt"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	remaining := time.Until(created.ExpiresAt)
	if remaining < 47*time.Hour+59*time.Minute || remaining > 48*time.Hour+time.Minute {
		t.Fatalf("unexpected TTL: %s", remaining)
	}

	chunkResponse := request(server, http.MethodPut,
		"/api/transfers/"+created.ID+"/chunks/0", make([]byte, 21), created.UploadToken)
	if chunkResponse.Code != http.StatusNoContent {
		t.Fatalf("chunk status = %d, body = %s", chunkResponse.Code, chunkResponse.Body)
	}
	completeResponse := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/complete", nil, created.UploadToken)
	if completeResponse.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d, body = %s", completeResponse.Code, completeResponse.Body)
	}
	manifestResponse := request(server, http.MethodGet,
		"/api/transfers/"+created.ID, nil, "")
	if manifestResponse.Code != http.StatusOK {
		t.Fatalf("manifest status = %d, body = %s", manifestResponse.Code, manifestResponse.Body)
	}
	claimResponse := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/claim", nil, "")
	if claimResponse.Code != http.StatusOK {
		t.Fatalf("claim status = %d, body = %s", claimResponse.Code, claimResponse.Body)
	}
	var claimed struct {
		DownloadToken string `json:"downloadToken"`
	}
	if err := json.Unmarshal(claimResponse.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	downloadResponse := request(server, http.MethodGet,
		"/api/transfers/"+created.ID+"/chunks/0", nil, claimed.DownloadToken)
	if downloadResponse.Code != http.StatusOK || downloadResponse.Body.Len() != 21 {
		t.Fatalf("download status = %d, size = %d", downloadResponse.Code, downloadResponse.Body.Len())
	}
}

func TestAcceptsCodeProtection(t *testing.T) {
	server := newTestServer(t)
	crypto := validLinkCrypto()
	crypto.AccessMode = "code"
	crypto.KeySalt = b64(make([]byte, 16))
	crypto.KeyNonce = b64(make([]byte, 12))
	crypto.EncryptedKey = b64(make([]byte, 48))
	crypto.KDFIterations = 310_000
	body, _ := json.Marshal(createRequest{
		PlainSize: 10, ChunkSize: 10, ChunkCount: 1, Crypto: crypto,
	})
	response := request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestRejectsWeakCodeConfiguration(t *testing.T) {
	server := newTestServer(t)
	crypto := validLinkCrypto()
	crypto.AccessMode = "code"
	crypto.KeySalt = b64(make([]byte, 16))
	crypto.KeyNonce = b64(make([]byte, 12))
	crypto.EncryptedKey = b64(make([]byte, 48))
	crypto.KDFIterations = 10
	body, _ := json.Marshal(createRequest{
		PlainSize: 10, ChunkSize: 10, ChunkCount: 1, Crypto: crypto,
	})
	response := request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestRejectsTransferWhenStorageQuotaIsExhausted(t *testing.T) {
	server, err := New(Config{
		Addr: ":0", DataDir: t.TempDir(), MaxFileBytes: 1024,
		MaxStorageBytes: 30, CleanupPeriod: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	body, _ := json.Marshal(createRequest{
		PlainSize: 10, ChunkSize: 10, ChunkCount: 1, Crypto: validLinkCrypto(),
	})
	first := request(server, http.MethodPost, "/api/transfers", body, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body)
	}
	second := request(server, http.MethodPost, "/api/transfers", body, "")
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body)
	}
}

func TestTenGiBTransferBoundary(t *testing.T) {
	const limit = int64(10 * 1024 * 1024 * 1024)
	for _, size := range []int64{512*1024*1024 + 1, limit, limit + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			server := newTestServer(t)
			server.config.MaxFileBytes = limit
			server.config.MaxStorageBytes = 11 * 1024 * 1024 * 1024
			server.availableDiskBytes = func(string) (int64, error) { return 20 * limit, nil }
			const chunkSize = int64(4 * 1024 * 1024)
			body, _ := json.Marshal(createRequest{
				PlainSize: size, ChunkSize: chunkSize,
				ChunkCount: int((size + chunkSize - 1) / chunkSize), Crypto: validLinkCrypto(),
			})
			response := request(server, http.MethodPost, "/api/transfers", body, "")
			want := http.StatusCreated
			if size > limit {
				want = http.StatusBadRequest
			}
			if response.Code != want {
				t.Fatalf("status = %d, want %d; body = %s", response.Code, want, response.Body)
			}
		})
	}
}

func TestDiskReservationsIncludeUnwrittenAndExpiredChunks(t *testing.T) {
	server := newTestServer(t)
	server.availableDiskBytes = func(string) (int64, error) { return diskReserve + 80, nil }
	body, _ := json.Marshal(createRequest{
		PlainSize: 30, ChunkSize: 30, ChunkCount: 1, Crypto: validLinkCrypto(),
	})
	first := request(server, http.MethodPost, "/api/transfers", body, "")
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, body = %s", first.Code, first.Body)
	}
	if _, err := server.db.Exec(`UPDATE transfers SET expires_at = ?`, time.Now().Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	second := request(server, http.MethodPost, "/api/transfers", body, "")
	if second.Code != http.StatusInsufficientStorage {
		t.Fatalf("second status = %d, body = %s", second.Code, second.Body)
	}
}

func TestUploadedChunksAreNotReservedTwice(t *testing.T) {
	server := newTestServer(t)
	server.availableDiskBytes = func(string) (int64, error) { return diskReserve + 80, nil }
	createCompleteTransfer(t, server) // 21 bytes are already on disk.
	server.availableDiskBytes = func(string) (int64, error) { return diskReserve + 59, nil }
	body, _ := json.Marshal(createRequest{
		PlainSize: 30, ChunkSize: 30, ChunkCount: 1, Crypto: validLinkCrypto(),
	})
	response := request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	response = request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("second status = %d, body = %s", response.Code, response.Body)
	}
}

func TestTenGiBTransferRejectedOnSixGiBFreeDisk(t *testing.T) {
	server := newTestServer(t)
	server.config.MaxFileBytes = 10 * 1024 * 1024 * 1024
	server.config.MaxStorageBytes = 11 * 1024 * 1024 * 1024
	server.availableDiskBytes = func(string) (int64, error) { return 6 * 1024 * 1024 * 1024, nil }
	body, _ := json.Marshal(createRequest{
		PlainSize: server.config.MaxFileBytes, ChunkSize: 4 * 1024 * 1024,
		ChunkCount: 2560, Crypto: validLinkCrypto(),
	})
	response := request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	var count int
	if err := server.db.QueryRow(`SELECT COUNT(*) FROM transfers`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected transfer reserved storage: count=%d, err=%v", count, err)
	}
}

func TestChunkWritePreservesDiskReserve(t *testing.T) {
	server := newTestServer(t)
	created := createCompleteTransfer(t, server)
	if _, err := server.db.Exec(`UPDATE transfers SET complete = 0 WHERE id = ?`, created.ID); err != nil {
		t.Fatal(err)
	}
	server.availableDiskBytes = func(string) (int64, error) { return diskReserve + 20, nil }
	response := request(server, http.MethodPut, "/api/transfers/"+created.ID+"/chunks/0", make([]byte, 21), created.UploadToken)
	if response.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
	if _, err := os.Stat(server.chunkPath(created.ID, 0) + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("chunk temporary file should not be created: %v", err)
	}
}

func TestDiskQueryFailureRejectsReservation(t *testing.T) {
	server := newTestServer(t)
	server.availableDiskBytes = func(string) (int64, error) { return 0, errors.New("disk unavailable") }
	body, _ := json.Marshal(createRequest{
		PlainSize: 5, ChunkSize: 5, ChunkCount: 1, Crypto: validLinkCrypto(),
	})
	response := request(server, http.MethodPost, "/api/transfers", body, "")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body)
	}
}

func TestAvailableDiskBytes(t *testing.T) {
	available, err := availableDiskBytes(t.TempDir())
	if err != nil || available <= 0 {
		t.Fatalf("available bytes = %d, err = %v", available, err)
	}
	if _, err := availableDiskBytes(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected error for a missing directory")
	}
}

func TestTransferCanOnlyBeClaimedOnceAndConsumeDeletesIt(t *testing.T) {
	server := newTestServer(t)
	created := createCompleteTransfer(t, server)

	first := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/claim", nil, "")
	if first.Code != http.StatusOK {
		t.Fatalf("first claim status = %d, body = %s", first.Code, first.Body)
	}
	var claim struct {
		DownloadToken string `json:"downloadToken"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &claim); err != nil {
		t.Fatal(err)
	}

	second := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/claim", nil, "")
	if second.Code != http.StatusGone {
		t.Fatalf("second claim status = %d, body = %s", second.Code, second.Body)
	}
	manifest := request(server, http.MethodGet,
		"/api/transfers/"+created.ID, nil, "")
	if manifest.Code != http.StatusNotFound {
		t.Fatalf("claimed manifest status = %d", manifest.Code)
	}
	wrongToken := request(server, http.MethodGet,
		"/api/transfers/"+created.ID+"/chunks/0", nil, "wrong")
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token chunk status = %d", wrongToken.Code)
	}

	consume := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/consume", nil, claim.DownloadToken)
	if consume.Code != http.StatusNoContent {
		t.Fatalf("consume status = %d, body = %s", consume.Code, consume.Body)
	}
	if _, err := os.Stat(server.transferDir(created.ID)); !os.IsNotExist(err) {
		t.Fatalf("transfer directory still exists: %v", err)
	}
	if _, err := server.loadTransfer(created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("transfer row still exists: %v", err)
	}
}

func TestCleanupDeletesTransferAfter48Hours(t *testing.T) {
	server := newTestServer(t)
	created := createCompleteTransfer(t, server)
	if _, err := server.db.Exec(
		`UPDATE transfers SET expires_at = ? WHERE id = ?`,
		time.Now().Add(-time.Second).Unix(), created.ID,
	); err != nil {
		t.Fatal(err)
	}
	if err := server.cleanupExpired(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(server.transferDir(created.ID)); !os.IsNotExist(err) {
		t.Fatalf("expired transfer directory still exists: %v", err)
	}
	if _, err := server.loadTransfer(created.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired transfer row still exists: %v", err)
	}
}

func TestNewMigratesExistingTransferDatabase(t *testing.T) {
	dataDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "transfers.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE transfers (
			id TEXT PRIMARY KEY,
			token_hash TEXT NOT NULL,
			plain_size INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL,
			chunk_count INTEGER NOT NULL,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			complete INTEGER NOT NULL DEFAULT 0,
			crypto_json TEXT NOT NULL
		)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	server, err := New(Config{
		Addr: ":0", DataDir: dataDir, MaxFileBytes: 1024,
		MaxStorageBytes: 1024 * 1024, CleanupPeriod: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })

	columns := map[string]bool{}
	rows, err := server.db.Query(`PRAGMA table_info(transfers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	if !columns["claimed_at"] || !columns["claim_token_hash"] {
		t.Fatalf("migration columns missing: %#v", columns)
	}
}

type createdTransfer struct {
	ID          string
	UploadToken string
}

func createCompleteTransfer(t *testing.T, server *Server) createdTransfer {
	t.Helper()
	body, _ := json.Marshal(createRequest{
		PlainSize: 5, ChunkSize: 5, ChunkCount: 1, Crypto: validLinkCrypto(),
	})
	createdResponse := request(server, http.MethodPost, "/api/transfers", body, "")
	if createdResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", createdResponse.Code, createdResponse.Body)
	}
	var created createdTransfer
	if err := json.Unmarshal(createdResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	chunk := request(server, http.MethodPut,
		"/api/transfers/"+created.ID+"/chunks/0", make([]byte, 21), created.UploadToken)
	if chunk.Code != http.StatusNoContent {
		t.Fatalf("chunk status = %d, body = %s", chunk.Code, chunk.Body)
	}
	complete := request(server, http.MethodPost,
		"/api/transfers/"+created.ID+"/complete", nil, created.UploadToken)
	if complete.Code != http.StatusNoContent {
		t.Fatalf("complete status = %d, body = %s", complete.Code, complete.Body)
	}
	return created
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Config{
		Addr: ":0", DataDir: t.TempDir(), MaxFileBytes: 1024,
		MaxStorageBytes: 1024 * 1024,
		CleanupPeriod:   time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	return server
}

func validLinkCrypto() CryptoManifest {
	return CryptoManifest{
		Version: 1, AccessMode: "link",
		NoncePrefix:       b64(make([]byte, 8)),
		MetadataNonce:     b64(make([]byte, 12)),
		EncryptedMetadata: b64(make([]byte, 32)),
	}
}

func b64(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func request(server *Server, method, path string, body []byte, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, req)
	return response
}
