// Copyright 2026 Sneat.app

package dbo4media

import (
	"testing"
	"time"

	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
)

func validReadyAsset() MediaAsset {
	return MediaAsset{
		Status: models4media.AssetStatusReady, Access: models4media.AccessPrivate,
		Storage:     StorageRef{Provider: "gcs", Bucket: "media", ObjectKey: "originals/m_1", Generation: 42},
		ContentType: "image/png", Size: 5, Width: 10, Height: 10,
		SHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		CreatedAt: time.Now(), CreatedBy: "u1", FinalizedAt: time.Now(),
	}
}

func TestReadyAssetRequiresKnownAccessAndPinnedGeneration(t *testing.T) {
	asset := validReadyAsset()
	if err := asset.Validate(); err != nil {
		t.Fatalf("valid asset: %v", err)
	}

	// AccessPublic is also valid
	asset.Access = models4media.AccessPublic
	if err := asset.Validate(); err != nil {
		t.Fatalf("valid public asset: %v", err)
	}

	asset.Access = models4media.Access("future-access")
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted unsupported access")
	}

	// Missing required fields
	for _, a := range []MediaAsset{
		{Status: "", Access: models4media.AccessPrivate, CreatedAt: time.Now(), CreatedBy: "u1"},
		{Status: models4media.AssetStatusReady, Access: "", CreatedAt: time.Now(), CreatedBy: "u1"},
		{Status: models4media.AssetStatusReady, Access: models4media.AccessPrivate, CreatedAt: time.Time{}, CreatedBy: "u1"},
		{Status: models4media.AssetStatusReady, Access: models4media.AccessPrivate, CreatedAt: time.Now(), CreatedBy: ""},
	} {
		if err := a.Validate(); err == nil {
			t.Fatal("Validate() accepted asset missing required fields")
		}
	}

	// Storage validations
	asset = validReadyAsset()
	asset.Storage.Provider = "s3"
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted provider != gcs")
	}

	asset = validReadyAsset()
	asset.Storage.Bucket = ""
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted empty bucket")
	}

	asset = validReadyAsset()
	asset.Storage.ObjectKey = ""
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted empty object key")
	}

	// RefCount validations
	asset = validReadyAsset()
	asset.RefCount = -1
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted negative RefCount")
	}

	asset = validReadyAsset()
	asset.RefCount = 2
	asset.DurableRefCount = 1
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted DurableRefCount < RefCount")
	}

	// Ready/Orphaned metadata validations
	for _, status := range []models4media.AssetStatus{models4media.AssetStatusReady, models4media.AssetStatusOrphaned} {
		asset = validReadyAsset()
		asset.Status = status
		asset.ContentType = ""
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted empty ContentType")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.Size = 0
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted zero Size")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.Width = 0
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted zero Width")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.Height = 0
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted zero Height")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.SHA256 = "short"
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted invalid SHA256")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.FinalizedAt = time.Time{}
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted zero FinalizedAt")
		}

		asset = validReadyAsset()
		asset.Status = status
		asset.Storage.Generation = 0
		if err := asset.Validate(); err == nil {
			t.Fatal("Validate() accepted zero Generation")
		}
	}

	// Uploading status
	uploadingAsset := MediaAsset{
		Status:    models4media.AssetStatusUploading,
		Access:    models4media.AccessPrivate,
		Storage:   StorageRef{Provider: "gcs", Bucket: "media", ObjectKey: "originals/m_1"},
		CreatedAt: time.Now(),
		CreatedBy: "u1",
	}
	if err := uploadingAsset.Validate(); err == nil {
		t.Fatal("Validate() accepted uploading asset without UploadExpiresAt")
	}

	uploadingAsset.UploadExpiresAt = time.Now().Add(time.Hour)
	if err := uploadingAsset.Validate(); err != nil {
		t.Fatalf("valid uploading asset: %v", err)
	}
}

func TestMediaLinkValidate(t *testing.T) {
	validTarget := models4media.Target{
		Scope:   models4media.TargetScopeSpace,
		SpaceID: "s1",
		Type:    "contact",
		ID:      "c1",
	}

	link := MediaLink{
		Target:    validTarget,
		Role:      "avatar",
		Retention: "durable",
		Status:    "active",
		CreatedAt: time.Now(),
		CreatedBy: "u1",
	}
	if err := link.Validate(); err != nil {
		t.Fatalf("valid link: %v", err)
	}

	// Target invalid
	invalidTargetLink := link
	invalidTargetLink.Target = models4media.Target{}
	if err := invalidTargetLink.Validate(); err == nil {
		t.Fatal("Validate() accepted invalid target")
	}

	// Missing fields
	for _, l := range []MediaLink{
		{Target: validTarget, Role: "", Status: "active", Retention: "durable", CreatedAt: time.Now(), CreatedBy: "u1"},
		{Target: validTarget, Role: "avatar", Status: "", Retention: "durable", CreatedAt: time.Now(), CreatedBy: "u1"},
		{Target: validTarget, Role: "avatar", Status: "active", Retention: "", CreatedAt: time.Now(), CreatedBy: "u1"},
		{Target: validTarget, Role: "avatar", Status: "active", Retention: "durable", CreatedAt: time.Time{}, CreatedBy: "u1"},
		{Target: validTarget, Role: "avatar", Status: "active", Retention: "durable", CreatedAt: time.Now(), CreatedBy: ""},
	} {
		if err := l.Validate(); err == nil {
			t.Fatal("Validate() accepted link missing required fields")
		}
	}

	// Crop valid
	link.Crop = &models4media.Crop{CenterX: 0.5, CenterY: 0.5, Zoom: 1.0}
	if err := link.Validate(); err != nil {
		t.Fatalf("valid crop: %v", err)
	}

	// Crop invalid
	link.Crop = &models4media.Crop{CenterX: -1.0}
	if err := link.Validate(); err == nil {
		t.Fatal("Validate() accepted invalid crop")
	}
}
