// Copyright 2026 Sneat.app

package blob4media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
)

type GCSStore struct {
	Client         *storage.Client
	Bucket         string
	GoogleAccessID string
	PrivateKey     []byte
	SignBytes      func(context.Context, []byte) ([]byte, error)
}

func (s GCSStore) BeginResumableUpload(ctx context.Context, objectKey, contentType string, expiresAt time.Time) (UploadCapability, error) {
	url, err := storage.SignedURL(s.Bucket, objectKey, &storage.SignedURLOptions{
		Scheme: storage.SigningSchemeV4, GoogleAccessID: s.GoogleAccessID, PrivateKey: s.PrivateKey, SignBytes: s.contextSigner(ctx),
		Method: http.MethodPost, ContentType: contentType, Headers: []string{"x-goog-resumable:start"}, Expires: expiresAt,
	})
	if err != nil {
		return UploadCapability{}, fmt.Errorf("sign resumable upload: %w", err)
	}
	return UploadCapability{URL: url, Method: http.MethodPost, Headers: map[string]string{"Content-Type": contentType, "x-goog-resumable": "start"}, ExpiresAt: expiresAt}, nil
}

func (s GCSStore) BeginRead(ctx context.Context, objectKey string, generation int64, expiresAt time.Time) (ReadCapability, error) {
	if generation <= 0 {
		return ReadCapability{}, errors.New("read capability requires a positive object generation")
	}
	query := make(url.Values)
	query.Set("generation", strconv.FormatInt(generation, 10))
	signedURL, err := storage.SignedURL(s.Bucket, objectKey, &storage.SignedURLOptions{
		Scheme: storage.SigningSchemeV4, GoogleAccessID: s.GoogleAccessID, PrivateKey: s.PrivateKey, SignBytes: s.contextSigner(ctx),
		Method: http.MethodGet, Expires: expiresAt, QueryParameters: query,
	})
	if err != nil {
		return ReadCapability{}, fmt.Errorf("sign original read: %w", err)
	}
	return ReadCapability{URL: signedURL, ExpiresAt: expiresAt}, nil
}

func (s GCSStore) contextSigner(ctx context.Context) func([]byte) ([]byte, error) {
	if s.SignBytes == nil {
		return nil
	}
	return func(payload []byte) ([]byte, error) {
		return s.SignBytes(ctx, payload)
	}
}

func (s GCSStore) InspectImage(ctx context.Context, objectKey string, maxBytes int64) (ImageInfo, error) {
	obj := s.Client.Bucket(s.Bucket).Object(objectKey)
	attrs, err := obj.Attrs(ctx)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("read GCS attributes: %w", err)
	}
	if attrs.Size <= 0 || attrs.Size > maxBytes {
		return ImageInfo{}, fmt.Errorf("image size %d outside 1..%d", attrs.Size, maxBytes)
	}
	if attrs.ContentType != "image/jpeg" && attrs.ContentType != "image/png" {
		return ImageInfo{}, fmt.Errorf("unsupported stored content type %q", attrs.ContentType)
	}
	if attrs.Generation <= 0 {
		return ImageInfo{}, errors.New("GCS object has no positive generation")
	}
	versionedObject := obj.Generation(attrs.Generation)
	reader, err := versionedObject.NewReader(ctx)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("open GCS object for hashing: %w", err)
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return ImageInfo{}, fmt.Errorf("hash GCS object: %w", copyErr)
	}
	if closeErr != nil {
		return ImageInfo{}, fmt.Errorf("close GCS object after hashing: %w", closeErr)
	}
	reader, err = versionedObject.NewReader(ctx)
	if err != nil {
		return ImageInfo{}, fmt.Errorf("open GCS object for image validation: %w", err)
	}
	config, format, decodeErr := image.DecodeConfig(io.LimitReader(reader, maxBytes+1))
	closeErr = reader.Close()
	if decodeErr != nil {
		return ImageInfo{}, fmt.Errorf("decode stored image: %w", decodeErr)
	}
	if closeErr != nil {
		return ImageInfo{}, fmt.Errorf("close GCS object after validation: %w", closeErr)
	}
	if format == "jpeg" && attrs.ContentType != "image/jpeg" || format == "png" && attrs.ContentType != "image/png" {
		return ImageInfo{}, fmt.Errorf("stored content type %q does not match image format %q", attrs.ContentType, format)
	}
	return ImageInfo{ContentType: attrs.ContentType, Size: attrs.Size, Width: config.Width, Height: config.Height, SHA256: hex.EncodeToString(hash.Sum(nil)), Generation: attrs.Generation}, nil
}

func (s GCSStore) Delete(ctx context.Context, objectKey string, generation int64) error {
	object := s.Client.Bucket(s.Bucket).Object(objectKey)
	if generation > 0 {
		object = object.If(storage.Conditions{GenerationMatch: generation})
	}
	err := object.Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}
