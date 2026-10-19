// Copyright 2026 Sneat.app

package api4media

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-go-core/apicore"
	"github.com/sneat-co/sneat-go-core/apicore/verify"
	"github.com/sneat-co/sneat-go-core/facade"
	"github.com/sneat-co/sneat-go-core/sneatcoretesting"
	"github.com/sneat-co/sneat-images/backend/blob4media"
	"github.com/sneat-co/sneat-images/backend/dal4media"
	"github.com/sneat-co/sneat-images/backend/dbo4media"
	"github.com/sneat-co/sneat-images/backend/facade4media"
)

type mockAdapter struct {
	ref *models4media.Ref
}

func (m *mockAdapter) Current(_ context.Context, _ dal.ReadwriteTransaction, _ models4media.Target, _ string) (*models4media.Ref, error) {
	return m.ref, nil
}

func (m *mockAdapter) Set(_ context.Context, _ dal.ReadwriteTransaction, _ models4media.Target, _ string, ref *models4media.Ref) error {
	m.ref = ref
	return nil
}

type mockAuthorizer struct {
	err error
}

func (m mockAuthorizer) Authorize(_ context.Context, _ string, _ models4media.Target) error {
	return m.err
}

type errBlob struct {
	originBlob
}

func (b *errBlob) BeginRead(_ context.Context, _ string, _ int64, _ time.Time) (blob4media.ReadCapability, error) {
	return blob4media.ReadCapability{}, errors.New("read failed")
}

func TestAccessRequestValidate(t *testing.T) {
	validTarget := models4media.Target{
		Scope:   models4media.TargetScopeSpace,
		SpaceID: "s1",
		Type:    "user",
		ID:      "u1",
	}

	for _, tc := range []struct {
		req accessRequest
		ok  bool
	}{
		{req: accessRequest{MediaID: "", Role: "avatar", Target: validTarget}, ok: false},
		{req: accessRequest{MediaID: "m1", Role: "", Target: validTarget}, ok: false},
		{req: accessRequest{MediaID: "m1", Role: "avatar", Target: models4media.Target{}}, ok: false},
		{req: accessRequest{MediaID: "m1", Role: "avatar", Target: validTarget}, ok: true},
	} {
		err := tc.req.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", tc.req, err, tc.ok)
		}
	}
}

func TestOriginErrorPaths(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	blob := &originBlob{info: blob4media.ImageInfo{
		ContentType: "image/png", Size: 5, Width: 100, Height: 80,
		SHA256: facade4media.HashBytes([]byte("image")), Generation: 9,
	}}
	service := &facade4media.Service{
		DB: sneatcoretesting.NewMemoryDB(), Blob: blob, Bucket: "private-media", Now: func() time.Time { return now },
	}
	handler := Handler{Service: service, OriginSecret: "test-secret"}

	// 1. Unauthorized
	req := httptest.NewRequest(http.MethodGet, "/v0/media/origin", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}

	// 2. Missing mediaID
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}

	// 3. Media not found
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID=nonexistent", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}

	// Create an asset that is still uploading (not ready/orphaned)
	uploadingAsset, err := service.BeginUpload(context.Background(), "u1", facade4media.BeginUploadRequest{
		RequestID: "uploading", ContentType: "image/png", Size: 5, Access: models4media.AccessPublic,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 4. Media not ready
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+uploadingAsset.MediaID, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", rec.Code)
	}

	// Finalize upload as private asset
	privateBegin, err := service.BeginUpload(context.Background(), "u1", facade4media.BeginUploadRequest{
		RequestID: "private-asset", ContentType: "image/png", Size: 5, Access: models4media.AccessPrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.FinalizeUpload(context.Background(), "u1", facade4media.FinalizeUploadRequest{MediaID: privateBegin.MediaID, SHA256: blob.info.SHA256}); err != nil {
		t.Fatal(err)
	}

	// 5. Origin access refused for private media without header
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+privateBegin.MediaID, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}

	// 6. Origin access with private verified header succeeds
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+privateBegin.MediaID, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("X-Media-Access-Verified", "private")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307, got %d", rec.Code)
	}

	// 7. BeginRead error
	errService := &facade4media.Service{
		DB: service.DB, Blob: &errBlob{}, Bucket: "private-media", Now: func() time.Time { return now },
	}
	errHandler := Handler{Service: errService, OriginSecret: "test-secret"}
	req = httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+privateBegin.MediaID, nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	req.Header.Set("X-Media-Access-Verified", "private")
	rec = httptest.NewRecorder()
	errHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", rec.Code)
	}
}

func TestServeHTTPDefaultNotFound(t *testing.T) {
	handler := Handler{}
	req := httptest.NewRequest(http.MethodGet, "/unknown/path", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestRegisterRoutesAll(t *testing.T) {
	routes := make(map[string]bool)
	Handler{}.RegisterRoutes(func(method, path string, handler http.HandlerFunc) {
		routes[method+" "+path] = true
	})
	expected := []string{
		"POST /v0/media/uploads",
		"POST /v0/media/uploads/finalize",
		"POST /v0/media/links",
		"POST /v0/media/links/remove",
		"POST /v0/media/links/restore",
		"POST /v0/media/access",
		"GET /v0/media/origin",
		"HEAD /v0/media/origin",
	}
	for _, route := range expected {
		if !routes[route] {
			t.Errorf("missing route %s", route)
		}
	}
}

func TestAuthenticatedRoutesUnauthenticated(t *testing.T) {
	origVerify := apicore.VerifyAuthenticatedRequestAndDecodeBody
	defer func() { apicore.VerifyAuthenticatedRequestAndDecodeBody = origVerify }()
	apicore.VerifyAuthenticatedRequestAndDecodeBody = func(
		w http.ResponseWriter, r *http.Request,
		options verify.RequestOptions,
		request facade.Request,
	) (facade.ContextWithUser, error) {
		return nil, errors.New("unauthenticated")
	}

	handler := Handler{}
	paths := []string{
		"/v0/media/uploads",
		"/v0/media/uploads/finalize",
		"/v0/media/links",
		"/v0/media/links/remove",
		"/v0/media/links/restore",
		"/v0/media/access",
	}

	for _, path := range paths {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("bad body"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		// Should fail authentication or body decode and return without panic
	}
}

func TestAuthenticatedRoutesSuccess(t *testing.T) {
	origVerify := apicore.VerifyAuthenticatedRequestAndDecodeBody
	defer func() { apicore.VerifyAuthenticatedRequestAndDecodeBody = origVerify }()

	var decodedBody string
	apicore.VerifyAuthenticatedRequestAndDecodeBody = func(
		w http.ResponseWriter, r *http.Request,
		options verify.RequestOptions,
		request facade.Request,
	) (facade.ContextWithUser, error) {
		if decodedBody != "" {
			if err := json.Unmarshal([]byte(decodedBody), request); err != nil {
				return nil, err
			}
		}
		return facade.NewContextWithUserID(r.Context(), "u1"), nil
	}

	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	blob := &originBlob{info: blob4media.ImageInfo{
		ContentType: "image/png", Size: 5, Width: 100, Height: 80,
		SHA256: facade4media.HashBytes([]byte("image")), Generation: 9,
	}}
	_, accessKey, _ := ed25519.GenerateKey(nil)
	adapter := &mockAdapter{}
	service := &facade4media.Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        blob,
		Bucket:      "private-media",
		Now:         func() time.Time { return now },
		Adapters:    map[string]facade4media.TargetAdapter{"user": adapter},
		Authorizer:  mockAuthorizer{},
		AccessKey:   accessKey,
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
	}
	handler := Handler{Service: service, OriginSecret: "secret"}

	// 1. BeginUpload
	decodedBody = `{"requestID":"req1","contentType":"image/png","size":5,"access":"public"}`
	req := httptest.NewRequest(http.MethodPost, "/v0/media/uploads", strings.NewReader(decodedBody))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("BeginUpload: expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var beginResp facade4media.BeginUploadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &beginResp); err != nil {
		t.Fatal(err)
	}
	mediaID := beginResp.MediaID

	// 2. FinalizeUpload
	decodedBody = `{"mediaID":"` + mediaID + `","sha256":"` + blob.info.SHA256 + `"}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/uploads/finalize", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("FinalizeUpload: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 3. Link
	decodedBody = `{"requestID":"link1","mediaID":"` + mediaID + `","target":{"scope":"space","spaceID":"s1","type":"user","id":"u1"},"role":"avatar","retention":"durable"}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/links", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Link: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 4. Access - AuthorizeMediaAccess fails
	decodedBody = `{"mediaID":"nonexistent","role":"avatar","target":{"scope":"space","spaceID":"s1","type":"user","id":"u1"}}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/access", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("Access error: expected error code, got %d: %s", rec.Code, rec.Body.String())
	}

	// 5. Access - AuthorizeMediaAccess succeeds
	decodedBody = `{"mediaID":"` + mediaID + `","role":"avatar","target":{"scope":"space","spaceID":"s1","type":"user","id":"u1"}}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/access", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Access: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Unlink
	decodedBody = `{"target":{"scope":"space","spaceID":"s1","type":"user","id":"u1"},"role":"avatar"}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/links/remove", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Unlink: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// 7. Restore
	decodedBody = `{"mediaID":"` + mediaID + `","target":{"scope":"space","spaceID":"s1","type":"user","id":"u1"},"role":"avatar"}`
	req = httptest.NewRequest(http.MethodPost, "/v0/media/links/restore", strings.NewReader(decodedBody))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Restore: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOriginOrphanedAssetRedirects(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	blob := &originBlob{info: blob4media.ImageInfo{
		ContentType: "image/png", Size: 5, Width: 100, Height: 80,
		SHA256: facade4media.HashBytes([]byte("image")), Generation: 9,
	}}
	service := &facade4media.Service{
		DB: sneatcoretesting.NewMemoryDB(), Blob: blob, Bucket: "private-media", Now: func() time.Time { return now },
	}
	begin, err := service.BeginUpload(context.Background(), "u1", facade4media.BeginUploadRequest{
		RequestID: "orphan-origin", ContentType: "image/png", Size: 5, Access: models4media.AccessPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.FinalizeUpload(context.Background(), "u1", facade4media.FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: blob.info.SHA256}); err != nil {
		t.Fatal(err)
	}
	// Mark as orphaned
	var asset dbo4media.MediaAsset
	_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		rec := dal4media.NewMediaRecord(begin.MediaID, &asset)
		if err := tx.Get(ctx, rec); err != nil {
			return err
		}
		asset.Status = models4media.AssetStatusOrphaned
		return tx.Set(ctx, rec)
	})

	handler := Handler{Service: service, OriginSecret: "secret"}
	req := httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+begin.MediaID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("expected 307 for orphaned asset, got %d: %s", rec.Code, rec.Body.String())
	}
}
