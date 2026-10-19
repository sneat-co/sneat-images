// Copyright 2026 Sneat.app

package facade4media

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-go-core/sneatcoretesting"
	"github.com/sneat-co/sneat-images/backend/blob4media"
	"github.com/sneat-co/sneat-images/backend/dal4media"
	"github.com/sneat-co/sneat-images/backend/dbo4media"
)

func setRecord(ctx context.Context, db dal.DB, rec record.Record) error {
	return db.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Set(ctx, rec)
	})
}

type errTargetAdapter struct {
	currentErr error
	setErr     error
	ref        *models4media.Ref
}

func (a *errTargetAdapter) Current(_ context.Context, _ dal.ReadwriteTransaction, _ models4media.Target, _ string) (*models4media.Ref, error) {
	if a.currentErr != nil {
		return nil, a.currentErr
	}
	return a.ref, nil
}

func (a *errTargetAdapter) Set(_ context.Context, _ dal.ReadwriteTransaction, _ models4media.Target, _ string, ref *models4media.Ref) error {
	if a.setErr != nil {
		return a.setErr
	}
	a.ref = ref
	return nil
}

type errBlobStore struct {
	fakeBlob
	uploadErr  error
	inspectErr error
	deleteErr  error
	onDelete   func()
}

func (e *errBlobStore) BeginResumableUpload(ctx context.Context, key, contentType string, expires time.Time) (blob4media.UploadCapability, error) {
	if e.uploadErr != nil {
		return blob4media.UploadCapability{}, e.uploadErr
	}
	return e.fakeBlob.BeginResumableUpload(ctx, key, contentType, expires)
}

func (e *errBlobStore) InspectImage(ctx context.Context, key string, maxBytes int64) (blob4media.ImageInfo, error) {
	if e.inspectErr != nil {
		return blob4media.ImageInfo{}, e.inspectErr
	}
	return e.fakeBlob.InspectImage(ctx, key, maxBytes)
}

func (e *errBlobStore) Delete(ctx context.Context, key string, gen int64) error {
	if e.onDelete != nil {
		e.onDelete()
	}
	if e.deleteErr != nil {
		return e.deleteErr
	}
	return e.fakeBlob.Delete(ctx, key, gen)
}

type errAuthorizer struct {
	err error
}

func (a errAuthorizer) Authorize(context.Context, string, models4media.Target) error {
	return a.err
}

func TestRequestsValidate(t *testing.T) {
	validTarget := models4media.Target{
		Scope:   models4media.TargetScopeSpace,
		SpaceID: "s1",
		Type:    "user",
		ID:      "u1",
	}

	// 1. BeginUploadRequest
	if err := (BeginUploadRequest{}).Validate(); err == nil {
		t.Fatal("expected error on empty BeginUploadRequest")
	}
	if err := (BeginUploadRequest{RequestID: "r", Size: 0}).Validate(); err == nil {
		t.Fatal("expected error on size 0")
	}
	if err := (BeginUploadRequest{RequestID: "r", Size: MaxImageBytes + 1}).Validate(); err == nil {
		t.Fatal("expected error on size > MaxImageBytes")
	}
	if err := (BeginUploadRequest{RequestID: "r", Size: 10, ContentType: "image/gif"}).Validate(); err == nil {
		t.Fatal("expected error on unsupported content type")
	}
	if err := (BeginUploadRequest{RequestID: "r", Size: 10, ContentType: "image/png", Access: "unknown"}).Validate(); err == nil {
		t.Fatal("expected error on unsupported access")
	}
	if err := (BeginUploadRequest{RequestID: "r", Size: 10, ContentType: "image/png", Access: models4media.AccessPublic}).Validate(); err != nil {
		t.Fatalf("unexpected error on valid BeginUploadRequest: %v", err)
	}

	// 2. FinalizeUploadRequest
	if err := (FinalizeUploadRequest{}).Validate(); err == nil {
		t.Fatal("expected error on empty FinalizeUploadRequest")
	}
	if err := (FinalizeUploadRequest{MediaID: "m", SHA256: "short"}).Validate(); err == nil {
		t.Fatal("expected error on short SHA256")
	}
	if err := (FinalizeUploadRequest{MediaID: "m", SHA256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}).Validate(); err != nil {
		t.Fatalf("unexpected error on valid FinalizeUploadRequest: %v", err)
	}

	// 3. LinkRequest
	if err := (LinkRequest{}).Validate(); err == nil {
		t.Fatal("expected error on empty LinkRequest")
	}
	if err := (LinkRequest{RequestID: "r", MediaID: "m", Role: "role", Target: models4media.Target{}}).Validate(); err == nil {
		t.Fatal("expected error on invalid target")
	}
	if err := (LinkRequest{RequestID: "r", MediaID: "m", Role: "role", Target: validTarget, Crop: &models4media.Crop{CenterX: -1}}).Validate(); err == nil {
		t.Fatal("expected error on invalid crop")
	}
	if err := (LinkRequest{RequestID: "r", MediaID: "m", Role: "role", Target: validTarget}).Validate(); err != nil {
		t.Fatalf("unexpected error on valid LinkRequest: %v", err)
	}

	// 4. TargetRequest
	if err := (TargetRequest{}).Validate(); err == nil {
		t.Fatal("expected error on empty TargetRequest")
	}
	if err := (TargetRequest{Role: "role"}).Validate(); err == nil {
		t.Fatal("expected error on invalid TargetRequest target")
	}
	if err := (TargetRequest{Role: "role", Target: validTarget}).Validate(); err != nil {
		t.Fatalf("unexpected error on valid TargetRequest: %v", err)
	}

	// 5. RestoreRequest
	if err := (RestoreRequest{}).Validate(); err == nil {
		t.Fatal("expected error on empty RestoreRequest")
	}
	if err := (RestoreRequest{TargetRequest: TargetRequest{MediaID: "m", Role: "role", Target: validTarget}}).Validate(); err != nil {
		t.Fatalf("unexpected error on valid RestoreRequest: %v", err)
	}
}

func TestBeginUploadErrorBranches(t *testing.T) {
	// Not configured
	s := Service{}
	if _, err := s.BeginUpload(context.Background(), "u1", BeginUploadRequest{}); err == nil {
		t.Fatal("expected error on unconfigured service")
	}

	// Blob upload error
	blobErr := errors.New("blob upload fail")
	store := &errBlobStore{uploadErr: blobErr}
	s = Service{DB: sneatcoretesting.NewMemoryDB(), Blob: store, Bucket: "b"}
	req := BeginUploadRequest{RequestID: "req1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic}
	if _, err := s.BeginUpload(context.Background(), "u1", req); !errors.Is(err, blobErr) {
		t.Fatalf("expected blob upload error, got: %v", err)
	}
}

func TestFinalizeUploadErrorBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	validSHA := HashBytes([]byte("test"))
	blob := &errBlobStore{fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: validSHA, Generation: 1}}}
	service := Service{DB: sneatcoretesting.NewMemoryDB(), Blob: blob, Bucket: "b", Now: func() time.Time { return now }}

	// 1. Media not found
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: "nonexistent", SHA256: validSHA}); err == nil {
		t.Fatal("expected error on nonexistent media")
	}

	// Begin upload
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "fin-test", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}

	// 2. CreatedBy != userID
	if _, err := service.FinalizeUpload(context.Background(), "other-user", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: validSHA}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got: %v", err)
	}

	// 3. InspectImage error
	blob.inspectErr = errors.New("inspect failed")
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: validSHA}); err == nil {
		t.Fatal("expected error on inspect failed")
	}
	blob.inspectErr = nil

	// 4. SHA256 mismatch
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: HashBytes([]byte("wrong"))}); err == nil {
		t.Fatal("expected error on SHA mismatch")
	}

	// 5. Metadata mismatch (e.g. content type or size mismatch)
	blob.info.Size = 999
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: validSHA}); err == nil {
		t.Fatal("expected error on size metadata mismatch")
	}
	blob.info.Size = 10

	// Finalize successfully
	asset, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: validSHA})
	if err != nil {
		t.Fatalf("successful finalize failed: %v", err)
	}

	// 6. Idempotent finalize of already ready asset
	secondAsset, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: validSHA})
	if err != nil || secondAsset.SHA256 != asset.SHA256 {
		t.Fatalf("idempotent finalize failed: %v", err)
	}

	// 7. Cannot finalize in non-uploading state with different SHA
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: HashBytes([]byte("other"))}); err == nil {
		t.Fatal("expected error when trying to re-finalize ready media with different sha")
	}
}

func TestLinkErrorBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	adapter := &errTargetAdapter{}

	service := Service{
		DB:         sneatcoretesting.NewMemoryDB(),
		Blob:       blob,
		Bucket:     "b",
		Now:        func() time.Time { return now },
		Adapters:   map[string]TargetAdapter{"user": adapter},
		Authorizer: allowAll{},
	}

	// 1. Authorizer error
	serviceNoAuth := service
	serviceNoAuth.Authorizer = errAuthorizer{err: errors.New("forbidden")}
	if _, err := serviceNoAuth.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: "m", Role: "role", Target: target}); err == nil {
		t.Fatal("expected authorizer error")
	}

	// 2. Unsupported target type
	serviceNoAdapter := service
	serviceNoAdapter.Adapters = map[string]TargetAdapter{}
	if _, err := serviceNoAdapter.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: "m", Role: "role", Target: target}); err == nil {
		t.Fatal("expected unsupported target type error")
	}

	// 3. Media not found
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: "nonexistent", Role: "role", Target: target}); err == nil {
		t.Fatal("expected media not found error")
	}

	// Upload asset but don't finalize (status is uploading)
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "link-unfinalized", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}

	// 4. Media not ready
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: begin.MediaID, Role: "role", Target: target}); err == nil {
		t.Fatal("expected media not ready error")
	}

	// Finalize upload
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	// 5. adapter.Current error
	adapter.currentErr = errors.New("current fail")
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: begin.MediaID, Role: "role", Target: target}); err == nil {
		t.Fatal("expected adapter.Current error")
	}
	adapter.currentErr = nil

	// 6. adapter.Set error
	adapter.setErr = errors.New("set fail")
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r", MediaID: begin.MediaID, Role: "role", Target: target}); err == nil {
		t.Fatal("expected adapter.Set error")
	}
	adapter.setErr = nil

	// Link successfully (first link)
	res1, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r1", MediaID: begin.MediaID, Role: "avatar", Target: target})
	if err != nil {
		t.Fatalf("first link failed: %v", err)
	}
	if res1.RefCount != 1 || res1.DurableRefCount != 1 {
		t.Fatalf("unexpected counts: %+v", res1)
	}

	// Link again with same role and same target (link was already active, ref counts shouldn't increase)
	res2, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r2", MediaID: begin.MediaID, Role: "avatar", Target: target})
	if err != nil {
		t.Fatalf("second link failed: %v", err)
	}
	if res2.RefCount != 1 || res2.DurableRefCount != 1 {
		t.Fatalf("unexpected counts after re-linking active link: %+v", res2)
	}
}

func TestUnlinkErrorBranches(t *testing.T) {
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	adapter := &errTargetAdapter{}
	service := Service{
		DB:         sneatcoretesting.NewMemoryDB(),
		Blob:       &fakeBlob{},
		Bucket:     "b",
		Adapters:   map[string]TargetAdapter{"user": adapter},
		Authorizer: allowAll{},
	}

	// 1. Invalid target
	if _, err := service.Unlink(context.Background(), "u1", models4media.Target{}, "role"); err == nil {
		t.Fatal("expected invalid target error")
	}

	// 2. Empty role
	if _, err := service.Unlink(context.Background(), "u1", target, ""); err == nil {
		t.Fatal("expected empty role error")
	}

	// 3. Authorize error
	serviceNoAuth := service
	serviceNoAuth.Authorizer = errAuthorizer{err: errors.New("unauth")}
	if _, err := serviceNoAuth.Unlink(context.Background(), "u1", target, "role"); err == nil {
		t.Fatal("expected authorizer error")
	}

	// 4. Unsupported adapter
	serviceNoAdapter := service
	serviceNoAdapter.Adapters = map[string]TargetAdapter{}
	if _, err := serviceNoAdapter.Unlink(context.Background(), "u1", target, "role"); err == nil {
		t.Fatal("expected unsupported target error")
	}

	// 5. adapter.Current error
	adapter.currentErr = errors.New("current fail")
	if _, err := service.Unlink(context.Background(), "u1", target, "role"); err == nil {
		t.Fatal("expected adapter.Current error")
	}
	adapter.currentErr = nil

	// 6. adapter.Current returns nil (no previous media)
	adapter.ref = nil
	res, err := service.Unlink(context.Background(), "u1", target, "role")
	if err != nil || res.MediaID != "" {
		t.Fatalf("expected empty result when unlinking unlinked target: %+v, %v", res, err)
	}
}

func TestRestoreErrorBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	adapter := &errTargetAdapter{}
	service := Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        &fakeBlob{},
		Bucket:      "b",
		Now:         func() time.Time { return now },
		Adapters:    map[string]TargetAdapter{"user": adapter},
		Authorizer:  allowAll{},
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
	}

	// 1. Invalid inputs
	if _, err := service.Restore(context.Background(), "u1", "", target, "role"); err == nil {
		t.Fatal("expected empty mediaID error")
	}
	if _, err := service.Restore(context.Background(), "u1", "m", models4media.Target{}, "role"); err == nil {
		t.Fatal("expected invalid target error")
	}

	// 2. Authorizer error
	serviceNoAuth := service
	serviceNoAuth.Authorizer = errAuthorizer{err: errors.New("unauth")}
	if _, err := serviceNoAuth.Restore(context.Background(), "u1", "m", target, "role"); err == nil {
		t.Fatal("expected authorizer error")
	}

	// 3. Unsupported adapter
	serviceNoAdapter := service
	serviceNoAdapter.Adapters = map[string]TargetAdapter{}
	if _, err := serviceNoAdapter.Restore(context.Background(), "u1", "m", target, "role"); err == nil {
		t.Fatal("expected unsupported target error")
	}

	// 4. Asset not found
	if _, err := service.Restore(context.Background(), "u1", "m", target, "role"); err == nil {
		t.Fatal("expected asset not found error")
	}

	// Create asset and link
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	service.Blob = blob
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "r-restore", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-link", MediaID: begin.MediaID, Role: "avatar", Target: target}); err != nil {
		t.Fatal(err)
	}

	// 5. Link is already active (returns immediately)
	restored, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar")
	if err != nil || restored.Status != models4media.LinkStatusActive {
		t.Fatalf("expected restore of active link to succeed: %+v, %v", restored, err)
	}

	// Unlink to make it deleted
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err != nil {
		t.Fatal(err)
	}

	// 6. Recovery window expired
	serviceExpired := service
	serviceExpired.Now = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err := serviceExpired.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected recovery window expired error")
	}

	// 7. adapter.Current error
	adapter.currentErr = errors.New("current fail")
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected adapter.Current error")
	}
	adapter.currentErr = nil

	// 8. Target already links another media asset
	adapter.ref = &models4media.Ref{MediaID: "other-media"}
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected already links another asset error")
	}
	adapter.ref = nil

	// 9. adapter.Set error
	adapter.setErr = errors.New("set fail")
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected adapter.Set error")
	}
	adapter.setErr = nil

	// 10. Success restore
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err != nil {
		t.Fatalf("restore failed: %v", err)
	}
}

func TestPurgeAndSweepBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &errBlobStore{fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}}
	service := Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        blob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
		Recovery:    time.Hour,
	}

	// 1. PurgeExpiredLink for nonexistent media returns error
	if err := service.PurgeExpiredLink(context.Background(), "nonexistent", "link1"); err == nil {
		t.Fatal("expected error on nonexistent media")
	}

	// 2. PurgeExpiredLink for nonexistent link returns nil
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "p-exp", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PurgeExpiredLink(context.Background(), begin.MediaID, "nonexistent-link"); err != nil {
		t.Fatalf("expected nil for nonexistent link, got: %v", err)
	}

	// 3. PurgeOrphan on non-orphaned asset returns nil
	if err := service.PurgeOrphan(context.Background(), begin.MediaID); err != nil {
		t.Fatalf("expected nil for non-orphaned asset, got: %v", err)
	}

	// 4. PurgeOrphan blob delete error rolls back status to orphaned
	// Create an orphaned asset in DB
	orphanAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusOrphaned,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_orph", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		Width:           10,
		Height:          10,
		SHA256:          sha,
		CreatedAt:       now,
		CreatedBy:       "u1",
		FinalizedAt:     now,
		OrphanedAt:      now.Add(-2 * time.Hour),
		PurgeAfter:      now.Add(-time.Hour),
		DurableRefCount: 0,
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_orph", orphanAsset))

	blob.deleteErr = errors.New("blob delete fail")
	if err := service.PurgeOrphan(context.Background(), "m_orph"); err == nil {
		t.Fatal("expected error when blob delete fails")
	}
	blob.deleteErr = nil

	// Check that status was rolled back to orphaned
	asset, err := service.GetAsset(context.Background(), "m_orph")
	if err != nil || asset.Status != models4media.AssetStatusOrphaned {
		t.Fatalf("expected status orphaned after rollback, got: %+v, %v", asset, err)
	}

	// Now purge orphan successfully
	if err := service.PurgeOrphan(context.Background(), "m_orph"); err != nil {
		t.Fatalf("purge orphan failed: %v", err)
	}
	asset, err = service.GetAsset(context.Background(), "m_orph")
	if err != nil || asset.Status != models4media.AssetStatusDeleted {
		t.Fatalf("expected status deleted, got: %+v, %v", asset, err)
	}

	// 5. PurgeStaleUpload blob delete error rolls back to uploading
	staleAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusUploading,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_stale", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		CreatedAt:       now,
		CreatedBy:       "u1",
		UploadExpiresAt: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_stale", staleAsset))

	blob.deleteErr = errors.New("blob delete fail")
	if err := service.PurgeStaleUpload(context.Background(), "m_stale"); err == nil {
		t.Fatal("expected error when blob delete fails")
	}
	blob.deleteErr = nil

	asset, err = service.GetAsset(context.Background(), "m_stale")
	if err != nil || asset.Status != models4media.AssetStatusUploading {
		t.Fatalf("expected status uploading after rollback, got: %+v, %v", asset, err)
	}

	// PurgeStaleUpload on non-expired upload returns nil
	staleAsset.UploadExpiresAt = now.Add(time.Hour)
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_stale", staleAsset))
	if err := service.PurgeStaleUpload(context.Background(), "m_stale"); err != nil {
		t.Fatalf("expected nil for non-expired upload, got: %v", err)
	}

	// 6. Sweep with limit clamping
	if _, err := service.Sweep(context.Background(), -1); err != nil {
		t.Fatalf("sweep with negative limit failed: %v", err)
	}
	if _, err := service.Sweep(context.Background(), 1000); err != nil {
		t.Fatalf("sweep with huge limit failed: %v", err)
	}
}

func TestAuthorizeMediaAccessAndAccessToken(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	service := Service{
		DB:  sneatcoretesting.NewMemoryDB(),
		Now: func() time.Time { return now },
	}

	// 1. Authorizer not configured
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); err == nil {
		t.Fatal("expected unconfigured authorizer error")
	}

	// 2. Authorizer error
	service.Authorizer = errAuthorizer{err: errors.New("auth fail")}
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); err == nil {
		t.Fatal("expected authorizer error")
	}
	service.Authorizer = allowAll{}

	// 3. Link not in DB
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); err == nil {
		t.Fatal("expected not found error")
	}

	// Create link record in DB with inactive status
	link := &dbo4media.MediaLink{
		Target:    target,
		Role:      "avatar",
		Retention: models4media.LinkRetentionRetained,
		Status:    models4media.LinkStatusDeleted,
		CreatedAt: now,
		CreatedBy: "u1",
	}
	linkKey := dal4media.LinkID(target, "avatar")
	if err := setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m", linkKey, link)); err != nil {
		t.Fatalf("setRecord link failed: %v", err)
	}

	// 4. Status not active -> ErrUnauthorized
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got: %v", err)
	}

	// Set status to active
	link.Status = models4media.LinkStatusActive
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m", linkKey, link))

	// 5. Target mismatch -> ErrUnauthorized
	mismatchLink := *link
	mismatchLink.Target.SpaceID = "other"
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m", linkKey, &mismatchLink))
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on target mismatch, got: %v", err)
	}

	// 6. Role mismatch -> ErrUnauthorized
	roleMismatchLink := *link
	roleMismatchLink.Role = "banner"
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m", linkKey, &roleMismatchLink))
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized on role mismatch, got: %v", err)
	}

	// 7. Success
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m", linkKey, link))
	if err := service.AuthorizeMediaAccess(context.Background(), "u1", "m", target, "avatar"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}

	// AccessToken without key
	if _, _, err := service.AccessToken("u1", "m", target, 0); err == nil {
		t.Fatal("expected error on unconfigured AccessKey")
	}

	// AccessToken with key and long ttl > 1h (clamps to 15m)
	_, priv, _ := ed25519.GenerateKey(nil)
	service.AccessKey = priv
	token, exp, err := service.AccessToken("u1", "m", target, 2*time.Hour)
	if err != nil || token == "" || exp.IsZero() {
		t.Fatalf("AccessToken failed: %v", err)
	}
	if exp.Sub(now) != 15*time.Minute {
		t.Fatalf("expected clamped 15min expiry, got: %v", exp.Sub(now))
	}
}

func TestBeginUploadValidateBeginError(t *testing.T) {
	service := Service{
		DB:   sneatcoretesting.NewMemoryDB(),
		Blob: &fakeBlob{},
	}
	// Missing requestID triggers validateBegin error
	if _, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{}); err == nil {
		t.Fatal("expected validateBegin error")
	}
}

func TestLinkReplacePreviousMediaAndValidations(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	adapter := &fakeTargetAdapter{refs: make(map[string]*models4media.Ref)}

	service := Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        blob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		Adapters:    map[string]TargetAdapter{"user": adapter},
		Authorizer:  allowAll{},
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
	}

	// 1. Link input validations
	if _, err := service.Link(context.Background(), "u1", LinkRequest{Target: models4media.Target{}}); err == nil {
		t.Fatal("expected invalid target error")
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{Target: target, MediaID: ""}); err == nil {
		t.Fatal("expected missing mediaID error")
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{Target: target, MediaID: "m", RequestID: "r", Role: "role", Crop: &models4media.Crop{CenterX: -1}}); err == nil {
		t.Fatal("expected invalid crop error")
	}
	noAuthService := service
	noAuthService.Authorizer = nil
	if _, err := noAuthService.Link(context.Background(), "u1", LinkRequest{Target: target, MediaID: "m", RequestID: "r", Role: "role"}); err == nil {
		t.Fatal("expected unconfigured authorizer error")
	}

	// 2. Upload media 1 and media 2
	b1, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "req-m1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: b1.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	b2, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "req-m2", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: b2.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	// Link target to media 1
	res1, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-link1", MediaID: b1.MediaID, Role: "avatar", Target: target})
	if err != nil {
		t.Fatalf("first link failed: %v", err)
	}
	if res1.RefCount != 1 {
		t.Fatalf("expected refCount=1, got %d", res1.RefCount)
	}

	// Now link target to media 2! (replaces previous media 1)
	res2, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-link2", MediaID: b2.MediaID, Role: "avatar", Target: target})
	if err != nil {
		t.Fatalf("second link replacing previous failed: %v", err)
	}
	if res2.RefCount != 1 {
		t.Fatalf("expected refCount=1, got %d", res2.RefCount)
	}

	// Verify media 1 refCount was decremented to 0
	a1, err := service.GetAsset(context.Background(), b1.MediaID)
	if err != nil || a1.RefCount != 0 {
		t.Fatalf("expected media 1 refCount 0, got %d, %v", a1.RefCount, err)
	}

	// Missing previous link error case: adapter points to a media whose link doesn't exist
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: "ghost-media"}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-ghost", MediaID: b2.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected error when previous link is missing")
	}
}

type inspectCallbackBlob struct {
	fakeBlob
	onInspect func()
	onDelete  func()
}

func (b *inspectCallbackBlob) InspectImage(ctx context.Context, key string, maxBytes int64) (blob4media.ImageInfo, error) {
	if b.onInspect != nil {
		b.onInspect()
	}
	return b.fakeBlob.InspectImage(ctx, key, maxBytes)
}

func (b *inspectCallbackBlob) Delete(ctx context.Context, key string, gen int64) error {
	if b.onDelete != nil {
		b.onDelete()
	}
	return b.fakeBlob.Delete(ctx, key, gen)
}

func TestFinalizeUploadConcurrentStates(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &inspectCallbackBlob{
		fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}},
	}
	service := Service{
		DB:     sneatcoretesting.NewMemoryDB(),
		Blob:   blob,
		Bucket: "b",
		Now:    func() time.Time { return now },
	}

	// 1. Concurrent change to Ready with matching SHA
	begin1, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "r-c1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	blob.onInspect = func() {
		// Mutate state in DB to ready before transaction
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			rec := dal4media.NewMediaRecord(begin1.MediaID, &dbo4media.MediaAsset{
				Status: models4media.AssetStatusReady, SHA256: sha, Access: models4media.AccessPublic,
				Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/" + begin1.MediaID, Generation: 1},
				ContentType: "image/png", Size: 10, Width: 10, Height: 10, CreatedAt: now, CreatedBy: "u1", FinalizedAt: now,
			})
			return tx.Set(ctx, rec)
		})
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin1.MediaID, SHA256: sha}); err != nil {
		t.Fatalf("expected nil on concurrent ready status, got: %v", err)
	}

	// 2. Concurrent change to Failed status
	begin2, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "r-c2", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	blob.onInspect = func() {
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			rec := dal4media.NewMediaRecord(begin2.MediaID, &dbo4media.MediaAsset{
				Status: models4media.AssetStatusFailed, Access: models4media.AccessPublic,
				Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/" + begin2.MediaID, Generation: 1},
				ContentType: "image/png", Size: 10, CreatedAt: now, CreatedBy: "u1",
			})
			return tx.Set(ctx, rec)
		})
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin2.MediaID, SHA256: sha}); err == nil {
		t.Fatal("expected error on concurrent state change to failed")
	}
}

func TestPurgeClaimsChangedAndRollback(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &inspectCallbackBlob{
		fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}},
	}
	service := Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        blob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
		Recovery:    time.Hour,
	}

	// 1. PurgeOrphan: claim changed while blob deleted
	orphanAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusOrphaned,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_claim", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		Width:           10,
		Height:          10,
		SHA256:          sha,
		CreatedAt:       now,
		CreatedBy:       "u1",
		FinalizedAt:     now,
		OrphanedAt:      now.Add(-2 * time.Hour),
		PurgeAfter:      now.Add(-time.Hour),
		DurableRefCount: 0,
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_claim", orphanAsset))

	blob.onDelete = func() {
		// Mutate status to active or DurableRefCount != 0 during delete
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			orphanAsset.DurableRefCount = 1
			return tx.Set(ctx, dal4media.NewMediaRecord("m_claim", orphanAsset))
		})
	}
	if err := service.PurgeOrphan(context.Background(), "m_claim"); err == nil {
		t.Fatal("expected error on orphan purge claim changed")
	}

	// 2. PurgeStaleUpload: claim changed while blob deleted
	staleAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusUploading,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_stale_claim", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		CreatedAt:       now,
		CreatedBy:       "u1",
		UploadExpiresAt: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_stale_claim", staleAsset))

	blob.onDelete = func() {
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			staleAsset.Status = models4media.AssetStatusFailed
			return tx.Set(ctx, dal4media.NewMediaRecord("m_stale_claim", staleAsset))
		})
	}
	if err := service.PurgeStaleUpload(context.Background(), "m_stale_claim"); err == nil {
		t.Fatal("expected error on stale upload purge claim changed")
	}
}

func TestGetAssetNotFound(t *testing.T) {
	service := Service{DB: sneatcoretesting.NewMemoryDB()}
	if _, err := service.GetAsset(context.Background(), "nonexistent"); err == nil {
		t.Fatal("expected error from GetAsset on nonexistent media")
	}
}

func TestPurgeExpiredLinkConditions(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}
	service := Service{
		DB:  sneatcoretesting.NewMemoryDB(),
		Now: func() time.Time { return now },
	}

	// Asset in DB
	asset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusReady,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_exp", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		Width:           10,
		Height:          10,
		SHA256:          HashBytes([]byte("1")),
		CreatedAt:       now,
		CreatedBy:       "u1",
		FinalizedAt:     now,
		RefCount:        1,
		DurableRefCount: 2,
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_exp", asset))

	linkID := dal4media.LinkID(target, "avatar")
	// 1. Link is active (not deleted) -> PurgeExpiredLink returns nil
	link := &dbo4media.MediaLink{
		Target:    target,
		Role:      "avatar",
		Retention: models4media.LinkRetentionRetained,
		Status:    models4media.LinkStatusActive,
		CreatedAt: now,
		CreatedBy: "u1",
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m_exp", linkID, link))
	if err := service.PurgeExpiredLink(context.Background(), "m_exp", linkID); err != nil {
		t.Fatalf("expected nil for active link, got %v", err)
	}

	// 2. Link is deleted but purgeAfter is in the future -> PurgeExpiredLink returns nil
	link.Status = models4media.LinkStatusDeleted
	link.PurgeAfter = now.Add(time.Hour)
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m_exp", linkID, link))
	if err := service.PurgeExpiredLink(context.Background(), "m_exp", linkID); err != nil {
		t.Fatalf("expected nil for non-expired link, got %v", err)
	}

	// 3. Link is deleted and expired -> purges link and decrements DurableRefCount (2 -> 1)
	link.PurgeAfter = now.Add(-time.Hour)
	_ = setRecord(context.Background(), service.DB, dal4media.NewLinkRecord("m_exp", linkID, link))
	if err := service.PurgeExpiredLink(context.Background(), "m_exp", linkID); err != nil {
		t.Fatalf("expected successful purge of expired link, got %v", err)
	}
	gotAsset, err := service.GetAsset(context.Background(), "m_exp")
	if err != nil || gotAsset.DurableRefCount != 1 {
		t.Fatalf("expected DurableRefCount=1, got: %+v, %v", gotAsset, err)
	}
}
