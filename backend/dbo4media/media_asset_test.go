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
	asset.Access = models4media.Access("future-access")
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted unsupported access")
	}
	asset = validReadyAsset()
	asset.Storage.Generation = 0
	if err := asset.Validate(); err == nil {
		t.Fatal("Validate() accepted ready media without a pinned generation")
	}
}
