// Copyright 2026 Sneat.app

package dbo4media

import (
	"strings"
	"time"

	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/strongo/validation"
)

const MediaCollection = "media"
const LinksCollection = "links"

type StorageRef struct {
	Provider   string `json:"provider" firestore:"provider"`
	Bucket     string `json:"bucket" firestore:"bucket"`
	ObjectKey  string `json:"objectKey" firestore:"objectKey"`
	Generation int64  `json:"generation,omitempty" firestore:"generation,omitempty"`
}

type MediaAsset struct {
	Status           models4media.AssetStatus `json:"status" firestore:"status"`
	Access           models4media.Access      `json:"access" firestore:"access"`
	Storage          StorageRef               `json:"storage" firestore:"storage"`
	ContentType      string                   `json:"contentType" firestore:"contentType"`
	Size             int64                    `json:"size" firestore:"size"`
	Width            int                      `json:"width,omitempty" firestore:"width,omitempty"`
	Height           int                      `json:"height,omitempty" firestore:"height,omitempty"`
	SHA256           string                   `json:"sha256,omitempty" firestore:"sha256,omitempty"`
	OriginalFilename string                   `json:"originalFilename,omitempty" firestore:"originalFilename,omitempty"`
	CreatedAt        time.Time                `json:"createdAt" firestore:"createdAt"`
	CreatedBy        string                   `json:"createdBy" firestore:"createdBy"`
	UploadExpiresAt  time.Time                `json:"uploadExpiresAt,omitempty" firestore:"uploadExpiresAt,omitempty"`
	FinalizedAt      time.Time                `json:"finalizedAt,omitempty" firestore:"finalizedAt,omitempty"`
	RefCount         int                      `json:"refCount" firestore:"refCount"`
	DurableRefCount  int                      `json:"durableRefCount" firestore:"durableRefCount"`
	OrphanedAt       time.Time                `json:"orphanedAt,omitempty" firestore:"orphanedAt,omitempty"`
	PurgeAfter       time.Time                `json:"purgeAfter,omitempty" firestore:"purgeAfter,omitempty"`
}

func (v MediaAsset) Validate() error {
	if v.Status == "" || v.Access == "" || v.CreatedAt.IsZero() || strings.TrimSpace(v.CreatedBy) == "" {
		return validation.NewErrRecordIsMissingRequiredField("status|access|createdAt|createdBy")
	}
	if v.Access != models4media.AccessPublic && v.Access != models4media.AccessPrivate {
		return validation.NewErrBadRecordFieldValue("access", "must be public or private")
	}
	if v.Storage.Provider != "gcs" || v.Storage.Bucket == "" || v.Storage.ObjectKey == "" {
		return validation.NewErrBadRecordFieldValue("storage", "must identify a private GCS object")
	}
	if v.RefCount < 0 || v.DurableRefCount < v.RefCount {
		return validation.NewErrBadRecordFieldValue("refCount", "counts are inconsistent")
	}
	if v.Status == models4media.AssetStatusReady || v.Status == models4media.AssetStatusOrphaned {
		if v.ContentType == "" || v.Size <= 0 || v.Width <= 0 || v.Height <= 0 || len(v.SHA256) != 64 || v.FinalizedAt.IsZero() || v.Storage.Generation <= 0 {
			return validation.NewErrRecordIsMissingRequiredField("ready media metadata")
		}
	}
	if v.Status == models4media.AssetStatusUploading && v.UploadExpiresAt.IsZero() {
		return validation.NewErrRecordIsMissingRequiredField("uploadExpiresAt")
	}
	return nil
}

type MediaLink struct {
	Target     models4media.Target        `json:"target" firestore:"target"`
	Role       string                     `json:"role" firestore:"role"`
	Retention  models4media.LinkRetention `json:"retention" firestore:"retention"`
	Crop       *models4media.Crop         `json:"crop,omitempty" firestore:"crop,omitempty"`
	Status     models4media.LinkStatus    `json:"status" firestore:"status"`
	CreatedAt  time.Time                  `json:"createdAt" firestore:"createdAt"`
	CreatedBy  string                     `json:"createdBy" firestore:"createdBy"`
	DeletedAt  time.Time                  `json:"deletedAt,omitempty" firestore:"deletedAt,omitempty"`
	PurgeAfter time.Time                  `json:"purgeAfter,omitempty" firestore:"purgeAfter,omitempty"`
}

func (v MediaLink) Validate() error {
	if err := v.Target.Validate(); err != nil {
		return err
	}
	if v.Role == "" || v.Status == "" || v.Retention == "" || v.CreatedAt.IsZero() || v.CreatedBy == "" {
		return validation.NewErrRecordIsMissingRequiredField("role|status|retention|createdAt|createdBy")
	}
	if v.Crop != nil {
		return v.Crop.Validate()
	}
	return nil
}
