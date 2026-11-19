// Copyright 2026 Sneat.app

package facade4media

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/dal-go/dalgo/dal"
	"github.com/dal-go/record"
	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-images/backend/blob4media"
	"github.com/sneat-co/sneat-images/backend/dal4media"
	"github.com/sneat-co/sneat-images/backend/dbo4media"
)

const MaxImageBytes int64 = 15 * 1024 * 1024

var ErrUnauthorized = errors.New("media operation is not authorized")

type TargetAdapter interface {
	Current(ctx context.Context, tx dal.ReadwriteTransaction, target models4media.Target, role string) (*models4media.Ref, error)
	Set(ctx context.Context, tx dal.ReadwriteTransaction, target models4media.Target, role string, ref *models4media.Ref) error
}

type Authorizer interface {
	Authorize(ctx context.Context, userID string, target models4media.Target) error
}

type Service struct {
	DB          dal.DB
	Blob        blob4media.Store
	Bucket      string
	Adapters    map[string]TargetAdapter
	Authorizer  Authorizer
	Now         func() time.Time
	Recovery    time.Duration
	OrphanGrace time.Duration
	UploadGrace time.Duration
	AccessKey   ed25519.PrivateKey
	// TrustedReleaseTypes lists the target types ReleaseLink may release,
	// set by the host (for example videostreaming.channelBackground).
	TrustedReleaseTypes []string
}

type BeginUploadRequest struct {
	RequestID        string              `json:"requestID"`
	ContentType      string              `json:"contentType"`
	Size             int64               `json:"size"`
	OriginalFilename string              `json:"originalFilename,omitempty"`
	Access           models4media.Access `json:"access"`
}

func (v BeginUploadRequest) Validate() error { return validateBegin(v) }

type BeginUploadResponse struct {
	MediaID       string            `json:"mediaID"`
	UploadURL     string            `json:"uploadURL"`
	UploadMethod  string            `json:"uploadMethod"`
	UploadHeaders map[string]string `json:"uploadHeaders"`
	ExpiresAt     time.Time         `json:"expiresAt"`
}

type FinalizeUploadRequest struct {
	MediaID string `json:"mediaID"`
	SHA256  string `json:"sha256"`
}

func (v FinalizeUploadRequest) Validate() error {
	if strings.TrimSpace(v.MediaID) == "" || len(strings.TrimSpace(v.SHA256)) != 64 {
		return errors.New("mediaID and a 64-character SHA-256 are required")
	}
	return nil
}

type LinkRequest struct {
	RequestID string                     `json:"requestID"`
	MediaID   string                     `json:"mediaID"`
	Target    models4media.Target        `json:"target"`
	Role      string                     `json:"role"`
	Crop      *models4media.Crop         `json:"crop,omitempty"`
	Retention models4media.LinkRetention `json:"retention,omitempty"`
}

func (v LinkRequest) Validate() error {
	if v.RequestID == "" || v.MediaID == "" || v.Role == "" {
		return errors.New("requestID, mediaID and role are required")
	}
	if err := v.Target.Validate(); err != nil {
		return err
	}
	if v.Crop != nil {
		return v.Crop.Validate()
	}
	return nil
}

type TargetRequest struct {
	MediaID string              `json:"mediaID,omitempty"`
	Target  models4media.Target `json:"target"`
	Role    string              `json:"role"`
}

func (v TargetRequest) Validate() error {
	if v.Role == "" {
		return errors.New("role is required")
	}
	return v.Target.Validate()
}

type RestoreRequest struct {
	TargetRequest
}

func (v RestoreRequest) Validate() error {
	if strings.TrimSpace(v.MediaID) == "" {
		return errors.New("mediaID is required")
	}
	return v.TargetRequest.Validate()
}

type LinkResult struct {
	MediaID         string                  `json:"mediaID"`
	LinkID          string                  `json:"linkID"`
	RefCount        int                     `json:"refCount"`
	DurableRefCount int                     `json:"durableRefCount"`
	Status          models4media.LinkStatus `json:"status"`
	PurgeAfter      time.Time               `json:"purgeAfter,omitempty"`
}

func (s *Service) defaults() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Recovery == 0 {
		s.Recovery = 7 * 24 * time.Hour
	}
	if s.OrphanGrace == 0 {
		s.OrphanGrace = 30 * 24 * time.Hour
	}
	if s.UploadGrace == 0 {
		s.UploadGrace = 24 * time.Hour
	}
}

func validateBegin(request BeginUploadRequest) error {
	if strings.TrimSpace(request.RequestID) == "" {
		return errors.New("requestID is required")
	}
	if request.Size <= 0 || request.Size > MaxImageBytes {
		return fmt.Errorf("size must be between 1 and %d", MaxImageBytes)
	}
	switch request.ContentType {
	case "image/jpeg", "image/png":
	default:
		return fmt.Errorf("unsupported content type %q", request.ContentType)
	}
	if request.Access != models4media.AccessPublic && request.Access != models4media.AccessPrivate {
		return fmt.Errorf("unsupported access %q", request.Access)
	}
	return nil
}

func (s *Service) BeginUpload(ctx context.Context, userID string, request BeginUploadRequest) (BeginUploadResponse, error) {
	s.defaults()
	if s.DB == nil || s.Blob == nil || strings.TrimSpace(userID) == "" {
		return BeginUploadResponse{}, errors.New("media service is not configured")
	}
	if err := validateBegin(request); err != nil {
		return BeginUploadResponse{}, err
	}
	mediaID := dal4media.MediaID(userID, request.RequestID)
	objectKey := "originals/" + mediaID
	now := s.Now().UTC()
	asset := &dbo4media.MediaAsset{Status: models4media.AssetStatusUploading, Access: request.Access,
		Storage: dbo4media.StorageRef{Provider: "gcs", Bucket: s.Bucket, ObjectKey: objectKey}, ContentType: request.ContentType,
		Size: request.Size, OriginalFilename: filepath.Base(strings.TrimSpace(request.OriginalFilename)), CreatedAt: now, CreatedBy: userID,
		UploadExpiresAt: now.Add(s.UploadGrace)}
	err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		existing := new(dbo4media.MediaAsset)
		record := dal4media.NewMediaRecord(mediaID, existing)
		if err := tx.Get(ctx, record); err == nil {
			if existing.CreatedBy != userID || existing.ContentType != request.ContentType || existing.Size != request.Size || existing.Status != models4media.AssetStatusUploading {
				return errors.New("requestID already belongs to a different or finalized upload")
			}
			return nil
		} else if !recordpkgIsNotFound(err) {
			return fmt.Errorf("read idempotent upload: %w", err)
		}
		return tx.Insert(ctx, dal4media.NewMediaRecord(mediaID, asset))
	})
	if err != nil {
		return BeginUploadResponse{}, err
	}
	expiresAt := now.Add(15 * time.Minute)
	capability, err := s.Blob.BeginResumableUpload(ctx, objectKey, request.ContentType, expiresAt)
	if err != nil {
		return BeginUploadResponse{}, err
	}
	return BeginUploadResponse{MediaID: mediaID, UploadURL: capability.URL, UploadMethod: capability.Method, UploadHeaders: capability.Headers, ExpiresAt: capability.ExpiresAt}, nil
}

func recordpkgIsNotFound(err error) bool { return record.IsNotFound(err) }

func (s *Service) FinalizeUpload(ctx context.Context, userID string, request FinalizeUploadRequest) (*dbo4media.MediaAsset, error) {
	s.defaults()
	asset := new(dbo4media.MediaAsset)
	if err := s.DB.Get(ctx, dal4media.NewMediaRecord(request.MediaID, asset)); err != nil {
		return nil, fmt.Errorf("get media asset: %w", err)
	}
	if asset.CreatedBy != userID {
		return nil, ErrUnauthorized
	}
	if asset.Status == models4media.AssetStatusReady && asset.SHA256 == strings.ToLower(request.SHA256) {
		return asset, nil
	}
	if asset.Status != models4media.AssetStatusUploading {
		return nil, fmt.Errorf("cannot finalize media in %q state", asset.Status)
	}
	info, err := s.Blob.InspectImage(ctx, asset.Storage.ObjectKey, MaxImageBytes)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(info.SHA256, request.SHA256) {
		return nil, errors.New("client SHA-256 does not match stored object")
	}
	if info.ContentType != asset.ContentType || info.Size != asset.Size {
		return nil, errors.New("stored object metadata does not match upload declaration")
	}
	asset.Status, asset.Width, asset.Height, asset.SHA256, asset.Storage.Generation = models4media.AssetStatusReady, info.Width, info.Height, info.SHA256, info.Generation
	asset.FinalizedAt = s.Now().UTC()
	if err = asset.Validate(); err != nil {
		return nil, err
	}
	if err = s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		current := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(request.MediaID, current)); err != nil {
			return err
		}
		if current.Status == models4media.AssetStatusReady && current.SHA256 == asset.SHA256 {
			return nil
		}
		if current.Status != models4media.AssetStatusUploading {
			return fmt.Errorf("media state changed to %q", current.Status)
		}
		return tx.Set(ctx, dal4media.NewMediaRecord(request.MediaID, asset))
	}); err != nil {
		return nil, err
	}
	return asset, nil
}

func (s *Service) Link(ctx context.Context, userID string, request LinkRequest) (result LinkResult, err error) {
	s.defaults()
	if err = request.Target.Validate(); err != nil {
		return result, err
	}
	if request.MediaID == "" || request.Role == "" || request.RequestID == "" {
		return result, errors.New("requestID, mediaID and role are required")
	}
	if request.Crop != nil {
		if err = request.Crop.Validate(); err != nil {
			return result, err
		}
	}
	if request.Retention == "" {
		request.Retention = models4media.LinkRetentionRetained
	}
	if s.Authorizer == nil {
		return result, errors.New("media authorizer is not configured")
	}
	if err = s.Authorizer.Authorize(ctx, userID, request.Target); err != nil {
		return result, fmt.Errorf("authorize media target: %w", err)
	}
	adapter := s.Adapters[request.Target.Type]
	if adapter == nil {
		return result, fmt.Errorf("unsupported media target type %q", request.Target.Type)
	}
	linkID := dal4media.LinkID(request.Target, request.Role)
	err = s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		asset := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(request.MediaID, asset)); err != nil {
			return err
		}
		if asset.Status != models4media.AssetStatusReady && asset.Status != models4media.AssetStatusOrphaned {
			return fmt.Errorf("media is not ready: %s", asset.Status)
		}
		previous, err := adapter.Current(ctx, tx, request.Target, request.Role)
		if err != nil {
			return err
		}
		link := new(dbo4media.MediaLink)
		linkRecord := dal4media.NewLinkRecord(request.MediaID, linkID, link)
		linkExists := true
		if err = tx.Get(ctx, linkRecord); err != nil {
			if !record.IsNotFound(err) {
				return err
			}
			linkExists = false
		}
		var previousAsset *dbo4media.MediaAsset
		var previousLink *dbo4media.MediaLink
		var previousLinkRecord record.Record
		if previous != nil && previous.MediaID != "" && previous.MediaID != request.MediaID {
			previousAsset = new(dbo4media.MediaAsset)
			if err = tx.Get(ctx, dal4media.NewMediaRecord(previous.MediaID, previousAsset)); err != nil {
				return err
			}
			previousLink = new(dbo4media.MediaLink)
			previousLinkRecord = dal4media.NewLinkRecord(previous.MediaID, linkID, previousLink)
			if err = tx.Get(ctx, previousLinkRecord); err != nil {
				if record.IsNotFound(err) {
					return fmt.Errorf("media reference points to missing previous link %s: %w", linkID, err)
				}
				return err
			}
		}
		wasActive := linkExists && link.Status == models4media.LinkStatusActive
		now := s.Now().UTC()
		*link = dbo4media.MediaLink{Target: request.Target, Role: request.Role, Retention: request.Retention, Crop: request.Crop,
			Status: models4media.LinkStatusActive, CreatedAt: now, CreatedBy: userID}
		if !wasActive {
			asset.RefCount++
			if !linkExists {
				asset.DurableRefCount++
			}
		}
		asset.Status, asset.OrphanedAt, asset.PurgeAfter = models4media.AssetStatusReady, time.Time{}, time.Time{}
		if err = adapter.Set(ctx, tx, request.Target, request.Role, &models4media.Ref{MediaID: request.MediaID, Crop: request.Crop}); err != nil {
			return err
		}
		if err = tx.Set(ctx, linkRecord); err != nil {
			return err
		}
		if err = tx.Set(ctx, dal4media.NewMediaRecord(request.MediaID, asset)); err != nil {
			return err
		}
		if previousAsset != nil && previousLink != nil && previousLink.Status != models4media.LinkStatusDeleted {
			previousLink.Status, previousLink.DeletedAt, previousLink.PurgeAfter = models4media.LinkStatusDeleted, now, now.Add(s.Recovery)
			if previousAsset.RefCount > 0 {
				previousAsset.RefCount--
			}
			if err = tx.Set(ctx, previousLinkRecord); err != nil {
				return err
			}
			if err = tx.Set(ctx, dal4media.NewMediaRecord(previous.MediaID, previousAsset)); err != nil {
				return err
			}
		}
		result = LinkResult{MediaID: request.MediaID, LinkID: linkID, RefCount: asset.RefCount, DurableRefCount: asset.DurableRefCount, Status: link.Status}
		return nil
	})
	return result, err
}

func (s *Service) Unlink(ctx context.Context, userID string, target models4media.Target, role string) (LinkResult, error) {
	s.defaults()
	if err := target.Validate(); err != nil {
		return LinkResult{}, err
	}
	if strings.TrimSpace(role) == "" {
		return LinkResult{}, errors.New("role is required")
	}
	if s.Authorizer == nil {
		return LinkResult{}, errors.New("media authorizer is not configured")
	}
	if err := s.Authorizer.Authorize(ctx, userID, target); err != nil {
		return LinkResult{}, err
	}
	adapter := s.Adapters[target.Type]
	if adapter == nil {
		return LinkResult{}, errors.New("unsupported media target")
	}
	linkID := dal4media.LinkID(target, role)
	var result LinkResult
	err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		previous, err := adapter.Current(ctx, tx, target, role)
		if err != nil {
			return err
		}
		if previous == nil || previous.MediaID == "" {
			return nil
		}
		asset := new(dbo4media.MediaAsset)
		if err = tx.Get(ctx, dal4media.NewMediaRecord(previous.MediaID, asset)); err != nil {
			return err
		}
		link := new(dbo4media.MediaLink)
		if err = tx.Get(ctx, dal4media.NewLinkRecord(previous.MediaID, linkID, link)); err != nil {
			return err
		}
		if link.Status != models4media.LinkStatusDeleted {
			now := s.Now().UTC()
			link.Status, link.DeletedAt, link.PurgeAfter = models4media.LinkStatusDeleted, now, now.Add(s.Recovery)
			if asset.RefCount > 0 {
				asset.RefCount--
			}
		}
		if err = adapter.Set(ctx, tx, target, role, nil); err != nil {
			return err
		}
		if err = tx.Set(ctx, dal4media.NewLinkRecord(previous.MediaID, linkID, link)); err != nil {
			return err
		}
		if err = tx.Set(ctx, dal4media.NewMediaRecord(previous.MediaID, asset)); err != nil {
			return err
		}
		result = LinkResult{MediaID: previous.MediaID, LinkID: linkID, RefCount: asset.RefCount, DurableRefCount: asset.DurableRefCount, Status: link.Status, PurgeAfter: link.PurgeAfter}
		return nil
	})
	return result, err
}

// ReleaseLink is a TRUSTED server-side release of one durable media link, for
// host/module-initiated cleanup after the owning module has already deleted its
// target (when TargetAdapter.Current can no longer find the link and the user
// authorizer would refuse). It performs no authorization and never touches the
// target adapter or the target's forward reference (assumed already gone). Only
// target types listed in Service.TrustedReleaseTypes are accepted. Never expose
// it over HTTP; callers must have authorized the action themselves.
//
// It marks the exact link deleted with PurgeAfter = now (no recovery window,
// because the target is gone) and decrements the asset RefCount exactly as
// Unlink does, in one transaction. A missing link is success with a zero
// LinkResult (empty Status) so a caller can log the miss; an already deleted
// link is success with its unchanged state.
func (s *Service) ReleaseLink(ctx context.Context, mediaID string, target models4media.Target, role string) (LinkResult, error) {
	s.defaults()
	if strings.TrimSpace(mediaID) == "" || strings.TrimSpace(role) == "" {
		return LinkResult{}, errors.New("mediaID and role are required")
	}
	if err := target.Validate(); err != nil {
		return LinkResult{}, err
	}
	if !slices.Contains(s.TrustedReleaseTypes, target.Type) {
		return LinkResult{}, fmt.Errorf("target type %q is not trusted for release", target.Type)
	}
	linkID := dal4media.LinkID(target, role)
	var result LinkResult
	err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		link := new(dbo4media.MediaLink)
		if err := tx.Get(ctx, dal4media.NewLinkRecord(mediaID, linkID, link)); err != nil {
			if record.IsNotFound(err) {
				return nil
			}
			return err
		}
		asset := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		result = LinkResult{MediaID: mediaID, LinkID: linkID, RefCount: asset.RefCount, DurableRefCount: asset.DurableRefCount, Status: link.Status, PurgeAfter: link.PurgeAfter}
		if link.Status == models4media.LinkStatusDeleted {
			return nil
		}
		now := s.Now().UTC()
		link.Status, link.DeletedAt, link.PurgeAfter = models4media.LinkStatusDeleted, now, now
		if asset.RefCount > 0 {
			asset.RefCount--
		}
		if err := tx.Set(ctx, dal4media.NewLinkRecord(mediaID, linkID, link)); err != nil {
			return err
		}
		if err := tx.Set(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		result.RefCount, result.Status, result.PurgeAfter = asset.RefCount, link.Status, link.PurgeAfter
		return nil
	})
	return result, err
}

func (s *Service) Restore(ctx context.Context, userID, mediaID string, target models4media.Target, role string) (LinkResult, error) {
	s.defaults()
	if strings.TrimSpace(mediaID) == "" || strings.TrimSpace(role) == "" {
		return LinkResult{}, errors.New("mediaID and role are required")
	}
	if err := target.Validate(); err != nil {
		return LinkResult{}, err
	}
	if s.Authorizer == nil {
		return LinkResult{}, errors.New("media authorizer is not configured")
	}
	if err := s.Authorizer.Authorize(ctx, userID, target); err != nil {
		return LinkResult{}, err
	}
	adapter := s.Adapters[target.Type]
	if adapter == nil {
		return LinkResult{}, errors.New("unsupported media target")
	}
	linkID := dal4media.LinkID(target, role)
	var result LinkResult
	err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		asset := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		link := new(dbo4media.MediaLink)
		linkRecord := dal4media.NewLinkRecord(mediaID, linkID, link)
		if err := tx.Get(ctx, linkRecord); err != nil {
			return err
		}
		if link.Status == models4media.LinkStatusActive {
			result = LinkResult{MediaID: mediaID, LinkID: linkID, RefCount: asset.RefCount, DurableRefCount: asset.DurableRefCount, Status: link.Status}
			return nil
		}
		if !link.PurgeAfter.After(s.Now()) {
			return errors.New("media link recovery window expired")
		}
		current, currentErr := adapter.Current(ctx, tx, target, role)
		if currentErr != nil {
			return currentErr
		}
		if current != nil && current.MediaID != "" && current.MediaID != mediaID {
			return errors.New("target already links another media asset")
		}
		if err := adapter.Set(ctx, tx, target, role, &models4media.Ref{MediaID: mediaID, Crop: link.Crop}); err != nil {
			return err
		}
		link.Status, link.DeletedAt, link.PurgeAfter = models4media.LinkStatusActive, time.Time{}, time.Time{}
		asset.RefCount++
		if err := tx.Set(ctx, linkRecord); err != nil {
			return err
		}
		if err := tx.Set(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		result = LinkResult{MediaID: mediaID, LinkID: linkID, RefCount: asset.RefCount, DurableRefCount: asset.DurableRefCount, Status: link.Status}
		return nil
	})
	return result, err
}

func (s *Service) PurgeExpiredLink(ctx context.Context, mediaID, linkID string) error {
	s.defaults()
	return s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		asset := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		link := new(dbo4media.MediaLink)
		linkKey := dal4media.NewLinkKey(mediaID, linkID)
		if err := tx.Get(ctx, dal4media.NewLinkRecord(mediaID, linkID, link)); err != nil {
			if record.IsNotFound(err) {
				return nil
			}
			return err
		}
		if link.Status != models4media.LinkStatusDeleted || link.PurgeAfter.After(s.Now()) {
			return nil
		}
		if err := tx.Delete(ctx, linkKey); err != nil {
			return err
		}
		if asset.DurableRefCount > 0 {
			asset.DurableRefCount--
		}
		if asset.DurableRefCount == 0 {
			asset.Status = models4media.AssetStatusOrphaned
			asset.OrphanedAt = s.Now().UTC()
			asset.PurgeAfter = asset.OrphanedAt.Add(s.OrphanGrace)
		}
		return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, asset))
	})
}

func (s *Service) PurgeOrphan(ctx context.Context, mediaID string) error {
	s.defaults()
	asset := new(dbo4media.MediaAsset)
	claimed := false
	if err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		if asset.Status != models4media.AssetStatusOrphaned || asset.DurableRefCount != 0 || asset.PurgeAfter.After(s.Now()) {
			return nil
		}
		asset.Status = models4media.AssetStatusPurging
		claimed = true
		return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, asset))
	}); err != nil || !claimed {
		return err
	}
	if err := s.Blob.Delete(ctx, asset.Storage.ObjectKey, asset.Storage.Generation); err != nil {
		_ = s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			current := new(dbo4media.MediaAsset)
			if getErr := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, current)); getErr != nil {
				return getErr
			}
			if current.Status == models4media.AssetStatusPurging {
				current.Status = models4media.AssetStatusOrphaned
				return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, current))
			}
			return nil
		})
		return fmt.Errorf("delete orphaned blob: %w", err)
	}
	return s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		current := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, current)); err != nil {
			return err
		}
		if current.Status != models4media.AssetStatusPurging || current.DurableRefCount != 0 {
			return errors.New("orphan purge claim changed while blob was being deleted")
		}
		current.Status = models4media.AssetStatusDeleted
		return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, current))
	})
}

func (s *Service) GetAsset(ctx context.Context, mediaID string) (*dbo4media.MediaAsset, error) {
	asset := new(dbo4media.MediaAsset)
	if err := s.DB.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
		return nil, err
	}
	return asset, nil
}

type SweepResult struct {
	AssetsScanned      int `json:"assetsScanned"`
	LinksPurged        int `json:"linksPurged"`
	AssetsPurged       int `json:"assetsPurged"`
	StaleUploadsPurged int `json:"staleUploadsPurged"`
	RefCountMismatches int `json:"refCountMismatches"`
}

// Sweep reconciles expired soft-deleted links and orphaned originals. It uses
// DALgo collection queries so the lifecycle remains independent of Firestore.
func (s *Service) Sweep(ctx context.Context, limit int) (result SweepResult, err error) {
	s.defaults()
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := dal.From(dal.NewRootCollectionRef(dbo4media.MediaCollection, "")).NewQuery().Limit(limit).
		SelectIntoRecord(func() record.Record {
			return record.NewRecordWithIncompleteKey(dbo4media.MediaCollection, reflect.String, new(dbo4media.MediaAsset))
		})
	assets, err := dal.ExecuteQueryAndReadAllToRecords(ctx, query, s.DB)
	if err != nil {
		return result, fmt.Errorf("query media assets: %w", err)
	}
	result.AssetsScanned = len(assets)
	for _, assetRecord := range assets {
		mediaID, _ := assetRecord.Key().ID.(string)
		asset, _ := assetRecord.Data().(*dbo4media.MediaAsset)
		if mediaID == "" || asset == nil {
			continue
		}
		linksQuery := dal.From(dal.NewCollectionRef(dbo4media.LinksCollection, "", dal4media.NewMediaKey(mediaID))).NewQuery().
			SelectIntoRecord(func() record.Record {
				return record.NewRecordWithIncompleteKey(dbo4media.LinksCollection, reflect.String, new(dbo4media.MediaLink))
			})
		links, queryErr := dal.ExecuteQueryAndReadAllToRecords(ctx, linksQuery, s.DB)
		if queryErr != nil {
			return result, fmt.Errorf("query media links for %s: %w", mediaID, queryErr)
		}
		activeLinks := 0
		for _, linkRecord := range links {
			linkID, _ := linkRecord.Key().ID.(string)
			link, _ := linkRecord.Data().(*dbo4media.MediaLink)
			if link != nil && link.Status == models4media.LinkStatusActive {
				activeLinks++
			}
			if linkID != "" && link != nil && link.Status == models4media.LinkStatusDeleted && !link.PurgeAfter.After(s.Now()) {
				if purgeErr := s.PurgeExpiredLink(ctx, mediaID, linkID); purgeErr != nil {
					return result, purgeErr
				}
				result.LinksPurged++
			}
		}
		if activeLinks != asset.RefCount || len(links) != asset.DurableRefCount {
			result.RefCountMismatches++
			log.Printf("media ref-count mismatch mediaID=%s storedActive=%d observedActive=%d storedDurable=%d observedDurable=%d", mediaID, asset.RefCount, activeLinks, asset.DurableRefCount, len(links))
		}
		if asset.Status == models4media.AssetStatusOrphaned && !asset.PurgeAfter.After(s.Now()) {
			if purgeErr := s.PurgeOrphan(ctx, mediaID); purgeErr != nil {
				return result, purgeErr
			}
			result.AssetsPurged++
		}
		if asset.Status == models4media.AssetStatusUploading && !asset.UploadExpiresAt.After(s.Now()) {
			if purgeErr := s.PurgeStaleUpload(ctx, mediaID); purgeErr != nil {
				return result, purgeErr
			}
			result.StaleUploadsPurged++
		}
	}
	return result, nil
}

func (s *Service) PurgeStaleUpload(ctx context.Context, mediaID string) error {
	s.defaults()
	asset := new(dbo4media.MediaAsset)
	claimed := false
	if err := s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, asset)); err != nil {
			return err
		}
		if asset.Status != models4media.AssetStatusUploading || asset.UploadExpiresAt.After(s.Now()) {
			return nil
		}
		asset.Status = models4media.AssetStatusPurging
		claimed = true
		return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, asset))
	}); err != nil || !claimed {
		return err
	}
	if err := s.Blob.Delete(ctx, asset.Storage.ObjectKey, asset.Storage.Generation); err != nil {
		_ = s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			current := new(dbo4media.MediaAsset)
			if getErr := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, current)); getErr != nil {
				return getErr
			}
			if current.Status == models4media.AssetStatusPurging {
				current.Status = models4media.AssetStatusUploading
				return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, current))
			}
			return nil
		})
		return fmt.Errorf("delete stale upload: %w", err)
	}
	return s.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		current := new(dbo4media.MediaAsset)
		if err := tx.Get(ctx, dal4media.NewMediaRecord(mediaID, current)); err != nil {
			return err
		}
		if current.Status != models4media.AssetStatusPurging {
			return errors.New("stale upload purge claim changed while blob was being deleted")
		}
		current.Status = models4media.AssetStatusFailed
		return tx.Set(ctx, dal4media.NewMediaRecord(mediaID, current))
	})
}

type accessClaims struct {
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	MediaID  string `json:"mediaID"`
	Scope    string `json:"scope"`
	SpaceID  string `json:"spaceID,omitempty"`
	Expires  int64  `json:"exp"`
}

func (s *Service) AuthorizeMediaAccess(ctx context.Context, userID, mediaID string, target models4media.Target, role string) error {
	if s.Authorizer == nil {
		return errors.New("media authorizer is not configured")
	}
	if err := s.Authorizer.Authorize(ctx, userID, target); err != nil {
		return err
	}
	link := new(dbo4media.MediaLink)
	if err := s.DB.Get(ctx, dal4media.NewLinkRecord(mediaID, dal4media.LinkID(target, role), link)); err != nil {
		return err
	}
	if link.Status != models4media.LinkStatusActive || link.Target != target || link.Role != role {
		return ErrUnauthorized
	}
	return nil
}

func (s *Service) AccessToken(userID, mediaID string, target models4media.Target, ttl time.Duration) (string, time.Time, error) {
	s.defaults()
	if len(s.AccessKey) != ed25519.PrivateKeySize {
		return "", time.Time{}, errors.New("media access signing key is not configured")
	}
	if ttl <= 0 || ttl > time.Hour {
		ttl = 15 * time.Minute
	}
	expires := s.Now().UTC().Add(ttl)
	claims := accessClaims{Audience: "media.sneat.co", Subject: userID, MediaID: mediaID, Scope: string(target.Scope), SpaceID: target.SpaceID, Expires: expires.Unix()}
	header, _ := json.Marshal(map[string]string{"alg": "EdDSA", "typ": "JWT"})
	payload, _ := json.Marshal(claims)
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := ed25519.Sign(s.AccessKey, []byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), expires, nil
}

func HashBytes(data []byte) string { sum := sha256.Sum256(data); return fmt.Sprintf("%x", sum[:]) }
