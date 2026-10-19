// Copyright 2026 Sneat.app

package blob4media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
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

func TestGCSStoreSignErrors(t *testing.T) {
	signErr := errors.New("sign error")
	store := GCSStore{
		Bucket:         "media-originals",
		GoogleAccessID: "media-signer@example.iam.gserviceaccount.com",
		SignBytes: func(context.Context, []byte) ([]byte, error) {
			return nil, signErr
		},
	}

	if _, err := store.BeginResumableUpload(context.Background(), "obj", "image/png", time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected error from BeginResumableUpload when SignBytes fails")
	}
	if _, err := store.BeginRead(context.Background(), "obj", 1, time.Now().Add(time.Hour)); err == nil {
		t.Fatal("expected error from BeginRead when SignBytes fails")
	}

	storeNoSigner := GCSStore{Bucket: "media-originals"}
	if storeNoSigner.contextSigner(context.Background()) != nil {
		t.Fatal("expected nil contextSigner when SignBytes is nil")
	}
}

func createTestPNG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

func createTestJPEG(w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, nil)
	return buf.Bytes()
}

func TestGCSStoreInspectImageAndDel(t *testing.T) {
	pngBytes := createTestPNG(10, 20)
	jpegBytes := createTestJPEG(15, 25)

	mockStatus := http.StatusOK
	mockContentType := "image/png"
	mockSize := int64(len(pngBytes))
	var mockGeneration int64 = 100
	mockBody := pngBytes
	downloadStatus := http.StatusOK
	secondDownloadStatus := http.StatusOK
	var downloadCount int
	deleteStatus := http.StatusOK
	var recordedDeleteReq *http.Request
	var truncateBody bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			recordedDeleteReq = r
			w.WriteHeader(deleteStatus)
			return
		}
		if r.URL.Path == "/b/test-bucket/o/test-obj" {
			if mockStatus != http.StatusOK {
				w.WriteHeader(mockStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"name":"test-obj","bucket":"test-bucket","size":"%d","contentType":"%s","generation":"%d"}`,
				mockSize, mockContentType, mockGeneration)
			return
		}
		downloadCount++
		if truncateBody {
			if hj, ok := w.(http.Hijacker); ok {
				conn, bufrw, _ := hj.Hijack()
				_, _ = bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10000\r\n\r\npartial")
				_ = bufrw.Flush()
				_ = conn.Close()
				return
			}
		}
		if downloadCount == 1 && downloadStatus != http.StatusOK {
			w.WriteHeader(downloadStatus)
			return
		}
		if downloadCount == 2 && secondDownloadStatus != http.StatusOK {
			w.WriteHeader(secondDownloadStatus)
			return
		}
		w.Header().Set("Content-Type", mockContentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(mockBody)
	}))
	defer server.Close()

	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(server.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("failed to create client: %v", err)
	}
	defer func() {
		_ = client.Close()
	}()

	store := GCSStore{
		Client: client,
		Bucket: "test-bucket",
	}

	// 1. Success PNG
	info, err := store.InspectImage(context.Background(), "test-obj", 100000)
	if err != nil {
		t.Fatalf("InspectImage failed: %v", err)
	}
	if info.Width != 10 || info.Height != 20 || info.ContentType != "image/png" || info.Size != int64(len(pngBytes)) {
		t.Fatalf("unexpected info: %+v", info)
	}

	// 2. Success JPEG
	mockContentType = "image/jpeg"
	mockSize = int64(len(jpegBytes))
	mockBody = jpegBytes
	downloadCount = 0
	info, err = store.InspectImage(context.Background(), "test-obj", 100000)
	if err != nil {
		t.Fatalf("InspectImage JPEG failed: %v", err)
	}
	if info.Width != 15 || info.Height != 25 || info.ContentType != "image/jpeg" {
		t.Fatalf("unexpected JPEG info: %+v", info)
	}

	// 3. Attrs error
	mockStatus = http.StatusBadRequest
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on attrs error")
	}
	mockStatus = http.StatusOK

	// 4. Size <= 0
	mockSize = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on size 0")
	}

	// 5. Size > maxBytes
	mockSize = 5000
	if _, err := store.InspectImage(context.Background(), "test-obj", 100); err == nil {
		t.Fatal("expected error on size > maxBytes")
	}
	mockSize = int64(len(pngBytes))

	// 6. Unsupported content type
	mockContentType = "application/pdf"
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on unsupported content type")
	}
	mockContentType = "image/png"

	// 7. Generation <= 0
	mockGeneration = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on generation 0")
	}
	mockGeneration = 100

	// 8. First download reader error
	downloadStatus = http.StatusBadRequest
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on first download reader error")
	}
	downloadStatus = http.StatusOK

	// 8b. Truncated stream on copy
	truncateBody = true
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on truncated stream copy")
	}
	truncateBody = false

	// 9. Second download reader error
	secondDownloadStatus = http.StatusBadRequest
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on second download reader error")
	}
	secondDownloadStatus = http.StatusOK

	// 10. Decode error (mock body is corrupted image)
	mockBody = []byte("not an image at all")
	mockSize = int64(len(mockBody))
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on corrupted image")
	}

	// 11. Format mismatch: content type image/png but body is JPEG
	mockBody = jpegBytes
	mockSize = int64(len(jpegBytes))
	mockContentType = "image/png"
	downloadCount = 0
	if _, err := store.InspectImage(context.Background(), "test-obj", 100000); err == nil {
		t.Fatal("expected error on format mismatch")
	}

	// 12. Delete with generation > 0
	deleteStatus = http.StatusNoContent
	if err := store.Delete(context.Background(), "test-obj", 100); err != nil {
		t.Fatalf("delete with gen failed: %v", err)
	}

	// 13. Delete with generation == 0
	if err := store.Delete(context.Background(), "test-obj", 0); err != nil {
		t.Fatalf("delete without gen failed: %v", err)
	}

	// 14. Delete 404 Not Found returns nil
	deleteStatus = http.StatusNotFound
	if err := store.Delete(context.Background(), "test-obj", 100); err != nil {
		t.Fatalf("delete 404 should return nil, got: %v", err)
	}

	// 15. Delete 400 error returns err
	deleteStatus = http.StatusBadRequest
	if err := store.Delete(context.Background(), "test-obj", 100); err == nil {
		t.Fatal("expected error on delete 400")
	}
	_ = recordedDeleteReq
}
