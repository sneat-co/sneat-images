// Copyright 2026 Sneat.app

package facade4media

import (
	"context"
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

func TestReleaseLink(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	hdb := &hookDB{DB: sneatcoretesting.NewMemoryDB()}
	adapter := &fakeTargetAdapter{refs: map[string]*models4media.Ref{}}
	// No Authorizer on purpose: ReleaseLink is trusted and must not need one.
	service := Service{DB: hdb, Blob: blob, Bucket: "b", Now: func() time.Time { return now }, Adapters: map[string]TargetAdapter{"user": adapter}, Authorizer: allowAll{}, Recovery: time.Hour, OrphanGrace: time.Hour}
	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}

	begin, err := service.BeginUpload(ctx, "u1", BeginUploadRequest{RequestID: "r-1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.FinalizeUpload(ctx, "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Link(ctx, "u1", LinkRequest{RequestID: "l-1", MediaID: begin.MediaID, Role: "bg", Target: target}); err != nil {
		t.Fatal(err)
	}

	// validation
	if err = service.ReleaseLink(ctx, "", target, "bg"); err == nil {
		t.Fatal("expected mediaID error")
	}
	if err = service.ReleaseLink(ctx, begin.MediaID, models4media.Target{}, "bg"); err == nil {
		t.Fatal("expected target error")
	}
	// missing link is success
	if err = service.ReleaseLink(ctx, begin.MediaID, target, "other"); err != nil {
		t.Fatal(err)
	}

	// target deleted by the module: adapter has no ref, release still works
	delete(adapter.refs, targetRefKey(target, "bg"))
	if err = service.ReleaseLink(ctx, begin.MediaID, target, "bg"); err != nil {
		t.Fatal(err)
	}
	asset := new(dbo4media.MediaAsset)
	if err = service.DB.Get(ctx, dal4media.NewMediaRecord(begin.MediaID, asset)); err != nil {
		t.Fatal(err)
	}
	link := new(dbo4media.MediaLink)
	if err = service.DB.Get(ctx, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(target, "bg"), link)); err != nil {
		t.Fatal(err)
	}
	if asset.RefCount != 0 || link.Status != models4media.LinkStatusDeleted || !link.PurgeAfter.Equal(now.Add(time.Hour)) {
		t.Fatalf("unexpected state: ref=%d link=%+v", asset.RefCount, link)
	}
	// idempotent
	if err = service.ReleaseLink(ctx, begin.MediaID, target, "bg"); err != nil {
		t.Fatal(err)
	}

	// refcount never goes below zero; errors propagate
	dbErr := errors.New("db fail")
	for name, hook := range map[string]func(record.Record) error{
		"get link": func(r record.Record) error {
			if r.Key().Collection() == dbo4media.LinksCollection {
				return dbErr
			}
			return nil
		},
		"get media": func(r record.Record) error {
			if r.Key().Collection() != dbo4media.LinksCollection {
				return dbErr
			}
			return nil
		},
	} {
		link.Status = models4media.LinkStatusActive
		if err = service.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			return tx.Set(ctx, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(target, "bg"), link))
		}); err != nil {
			t.Fatal(err)
		}
		hook := hook
		hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onGet: func(_ context.Context, r record.Record) error { return hook(r) }}
		}
		if err = service.ReleaseLink(ctx, begin.MediaID, target, "bg"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	for name, match := range map[string]func(record.Record) bool{
		"set link":  func(r record.Record) bool { return r.Key().Collection() == dbo4media.LinksCollection },
		"set media": func(r record.Record) bool { return r.Key().Collection() != dbo4media.LinksCollection },
	} {
		match := match
		hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onSet: func(_ context.Context, r record.Record) error {
				if match(r) {
					return dbErr
				}
				return nil
			}}
		}
		if err = service.ReleaseLink(ctx, begin.MediaID, target, "bg"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	hdb.txHook = nil
	if err = service.ReleaseLink(ctx, begin.MediaID, target, "bg"); err != nil {
		t.Fatal(err)
	}
	if err = service.DB.Get(ctx, dal4media.NewMediaRecord(begin.MediaID, asset)); err != nil || asset.RefCount != 0 {
		t.Fatalf("refcount=%d err=%v", asset.RefCount, err)
	}
}
