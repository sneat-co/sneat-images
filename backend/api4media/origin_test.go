// Copyright 2026 Sneat.app

package api4media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-go-core/sneatcoretesting"
	"github.com/sneat-co/sneat-images/backend/blob4media"
	"github.com/sneat-co/sneat-images/backend/facade4media"
)

type originBlob struct {
	info           blob4media.ImageInfo
	readCalls      int
	readObject     string
	readGeneration int64
}

func (b *originBlob) BeginResumableUpload(_ context.Context, _ string, _ string, expires time.Time) (blob4media.UploadCapability, error) {
	return blob4media.UploadCapability{URL: "https://storage.example/upload", Method: http.MethodPost, ExpiresAt: expires}, nil
}

func (b *originBlob) BeginRead(_ context.Context, objectKey string, generation int64, expires time.Time) (blob4media.ReadCapability, error) {
	b.readCalls++
	b.readObject = objectKey
	b.readGeneration = generation
	return blob4media.ReadCapability{URL: "https://storage.example/original?generation=9&signature=signed", ExpiresAt: expires}, nil
}

func (b *originBlob) InspectImage(context.Context, string, int64) (blob4media.ImageInfo, error) {
	return b.info, nil
}

func (b *originBlob) Delete(context.Context, string, int64) error { return nil }

func TestOriginRedirectsReadyMediaToShortLivedBlobCapability(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	blob := &originBlob{info: blob4media.ImageInfo{
		ContentType: "image/png", Size: 5, Width: 100, Height: 80,
		SHA256: facade4media.HashBytes([]byte("image")), Generation: 9,
	}}
	service := &facade4media.Service{
		DB: sneatcoretesting.NewMemoryDB(), Blob: blob, Bucket: "private-media", Now: func() time.Time { return now },
	}
	begin, err := service.BeginUpload(context.Background(), "u1", facade4media.BeginUploadRequest{
		RequestID: "origin-test", ContentType: "image/png", Size: 5, Access: models4media.AccessPublic,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.FinalizeUpload(context.Background(), "u1", facade4media.FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: blob.info.SHA256}); err != nil {
		t.Fatal(err)
	}

	handler := Handler{Service: service, OriginSecret: "origin-secret"}
	request := httptest.NewRequest(http.MethodGet, "/v0/media/origin?mediaID="+begin.MediaID, nil)
	request.Header.Set("Authorization", "Bearer origin-secret")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, body=%q", recorder.Code, recorder.Body.String())
	}
	if location := recorder.Header().Get("Location"); location != "https://storage.example/original?generation=9&signature=signed" {
		t.Fatalf("location = %q", location)
	}
	if blob.readCalls != 1 || blob.readGeneration != 9 || blob.readObject == "" {
		t.Fatalf("read capability calls=%d object=%q generation=%d", blob.readCalls, blob.readObject, blob.readGeneration)
	}

	head := httptest.NewRequest(http.MethodHead, "/v0/media/origin?mediaID="+begin.MediaID, nil)
	head.Header.Set("Authorization", "Bearer origin-secret")
	headRecorder := httptest.NewRecorder()
	handler.ServeHTTP(headRecorder, head)
	if headRecorder.Code != http.StatusTemporaryRedirect || headRecorder.Header().Get("Location") == "" {
		t.Fatalf("HEAD response = status %d headers %#v", headRecorder.Code, headRecorder.Header())
	}
	if blob.readCalls != 2 {
		t.Fatalf("HEAD did not verify read capability; calls=%d", blob.readCalls)
	}
}

func TestOriginRefusesUnsupportedAssetAccess(t *testing.T) {
	for _, tc := range []struct {
		access   models4media.Access
		verified string
		allowed  bool
	}{
		{access: models4media.AccessPublic, verified: "public", allowed: true},
		{access: models4media.AccessPrivate, verified: "private", allowed: true},
		{access: models4media.AccessPrivate, verified: "public", allowed: false},
		{access: models4media.Access("future-access"), verified: "public", allowed: false},
		{access: "", verified: "private", allowed: false},
	} {
		if got := originAccessAllowed(tc.access, tc.verified); got != tc.allowed {
			t.Errorf("originAccessAllowed(%q, %q) = %v, want %v", tc.access, tc.verified, got, tc.allowed)
		}
	}
}

func TestRegisterRoutesIncludesOriginHead(t *testing.T) {
	routes := make(map[string]http.HandlerFunc)
	Handler{}.RegisterRoutes(func(method, path string, handler http.HandlerFunc) {
		routes[method+" "+path] = handler
	})
	for _, route := range []string{
		http.MethodGet + " /v0/media/origin",
		http.MethodHead + " /v0/media/origin",
	} {
		if routes[route] == nil {
			t.Errorf("route %q is not registered", route)
		}
	}
}
