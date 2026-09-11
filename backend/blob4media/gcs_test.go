// Copyright 2026 Sneat.app

package blob4media

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGCSStoreBeginResumableUploadUsesContextAwareSigner(t *testing.T) {
	type contextKey string
	const requestKey contextKey = "request"
	ctx := context.WithValue(context.Background(), requestKey, "media-upload")
	expiresAt := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	called := false
	store := GCSStore{
		Bucket:         "media-originals",
		GoogleAccessID: "media-signer@example.iam.gserviceaccount.com",
		SignBytes: func(signingContext context.Context, payload []byte) ([]byte, error) {
			called = true
			if got := signingContext.Value(requestKey); got != "media-upload" {
				t.Fatalf("signing context value = %v", got)
			}
			if len(payload) == 0 {
				t.Fatal("signing payload is empty")
			}
			return make([]byte, 64), nil
		},
	}

	capability, err := store.BeginResumableUpload(ctx, "users/u1/media/m_123/original", "image/png", expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("SignBytes was not called")
	}
	if capability.Method != http.MethodPost || capability.ExpiresAt != expiresAt {
		t.Fatalf("capability = %+v", capability)
	}
	if capability.Headers["Content-Type"] != "image/png" || capability.Headers["x-goog-resumable"] != "start" {
		t.Fatalf("capability headers = %#v", capability.Headers)
	}
	if !strings.HasPrefix(capability.URL, "https://storage.googleapis.com/media-originals/") || !strings.Contains(capability.URL, "X-Goog-Signature=") {
		t.Fatalf("signed URL = %q", capability.URL)
	}
}

func TestGCSStoreBeginReadPinsGenerationAndUsesContextAwareSigner(t *testing.T) {
	type contextKey string
	const requestKey contextKey = "request"
	ctx := context.WithValue(context.Background(), requestKey, "media-read")
	expiresAt := time.Now().Add(2 * time.Minute).UTC().Truncate(time.Second)
	called := false
	store := GCSStore{
		Bucket:         "media-originals",
		GoogleAccessID: "media-signer@example.iam.gserviceaccount.com",
		SignBytes: func(signingContext context.Context, payload []byte) ([]byte, error) {
			called = true
			if got := signingContext.Value(requestKey); got != "media-read" {
				t.Fatalf("signing context value = %v", got)
			}
			if len(payload) == 0 {
				t.Fatal("signing payload is empty")
			}
			return make([]byte, 64), nil
		},
	}

	capability, err := store.BeginRead(ctx, "users/u1/media/m_123/original", 42, expiresAt)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("SignBytes was not called")
	}
	if capability.ExpiresAt != expiresAt {
		t.Fatalf("capability = %+v", capability)
	}
	parsed, err := url.Parse(capability.URL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("generation") != "42" || parsed.Query().Get("X-Goog-Signature") == "" {
		t.Fatalf("signed URL query = %q", parsed.RawQuery)
	}
}

func TestGCSStoreBeginReadRejectsMissingGeneration(t *testing.T) {
	store := GCSStore{Bucket: "media-originals"}
	if _, err := store.BeginRead(context.Background(), "users/u1/media/m_123/original", 0, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("BeginRead() accepted an unpinned object generation")
	}
}
