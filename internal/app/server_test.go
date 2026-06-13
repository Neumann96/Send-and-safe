package app

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	downloadResponse := request(server, http.MethodGet,
		"/api/transfers/"+created.ID+"/chunks/0", nil, "")
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

func newTestServer(t *testing.T) *Server {
	t.Helper()
	server, err := New(Config{
		Addr: ":0", DataDir: t.TempDir(), MaxFileBytes: 1024,
		CleanupPeriod: time.Hour,
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
