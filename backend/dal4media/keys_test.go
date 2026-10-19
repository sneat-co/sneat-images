// Copyright 2026 Sneat.app

package dal4media

import (
	"testing"

	"github.com/sneat-co/sneat-ext-contracts/media/models4media"
	"github.com/sneat-co/sneat-images/backend/dbo4media"
)

func TestMediaID(t *testing.T) {
	id1 := MediaID("user1", "req1")
	if id1 == "" {
		t.Fatal("expected non-empty MediaID")
	}
	if id1[:2] != "m_" {
		t.Fatalf("expected prefix 'm_', got %s", id1)
	}
	id2 := MediaID("user1", "req1")
	if id1 != id2 {
		t.Fatalf("expected deterministic MediaID, got %s and %s", id1, id2)
	}
	id3 := MediaID("user2", "req1")
	if id1 == id3 {
		t.Fatalf("expected different MediaID for different user, got %s", id1)
	}
}

func TestLinkID(t *testing.T) {
	target := models4media.Target{
		Scope:    models4media.TargetScopeSpace,
		SpaceID:  "space1",
		Type:     "user",
		ParentID: "parent1",
		ID:       "id1",
	}
	id1 := LinkID(target, "avatar")
	if id1 == "" {
		t.Fatal("expected non-empty LinkID")
	}
	if id1[:2] != "l_" {
		t.Fatalf("expected prefix 'l_', got %s", id1)
	}
	id2 := LinkID(target, "avatar")
	if id1 != id2 {
		t.Fatalf("expected deterministic LinkID, got %s and %s", id1, id2)
	}
	id3 := LinkID(target, "banner")
	if id1 == id3 {
		t.Fatalf("expected different LinkID for different role, got %s", id1)
	}
}

func TestKeysAndRecords(t *testing.T) {
	mediaKey := NewMediaKey("media1")
	if mediaKey == nil || mediaKey.ID != "media1" {
		t.Fatalf("unexpected media key: %+v", mediaKey)
	}

	linkKey := NewLinkKey("media1", "link1")
	if linkKey == nil || linkKey.ID != "link1" {
		t.Fatalf("unexpected link key: %+v", linkKey)
	}

	mediaRec := NewMediaRecord("media1", &dbo4media.MediaAsset{})
	if mediaRec == nil || mediaRec.Key() == nil || mediaRec.Key().ID != "media1" {
		t.Fatalf("unexpected media record: %+v", mediaRec)
	}

	linkRec := NewLinkRecord("media1", "link1", &dbo4media.MediaLink{})
	if linkRec == nil || linkRec.Key() == nil || linkRec.Key().ID != "link1" {
		t.Fatalf("unexpected link record: %+v", linkRec)
	}
}
