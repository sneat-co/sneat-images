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

type countingAdapter struct{ calls int }

func (a *countingAdapter) Current(context.Context, dal.ReadwriteTransaction, models4media.Target, string) (*models4media.Ref, error) {
	a.calls++
	return nil, nil
}
func (a *countingAdapter) Set(context.Context, dal.ReadwriteTransaction, models4media.Target, string, *models4media.Ref) error {
	a.calls++
	return nil
}

func TestReleaseLink(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	hdb := &hookDB{DB: sneatcoretesting.NewMemoryDB()}
	const trusted = "videostreaming.channelBackground"
	full := Service{DB: hdb, Blob: blob, Bucket: "b", Now: func() time.Time { return now }, Adapters: map[string]TargetAdapter{trusted: &fakeTargetAdapter{refs: map[string]*models4media.Ref{}}, "user": &fakeTargetAdapter{refs: map[string]*models4media.Ref{}}}, Authorizer: allowAll{}, Recovery: time.Hour, OrphanGrace: time.Hour}
	// Release-only service: no authorizer; the adapter must never be called.
	spy := &countingAdapter{}
	service := Service{DB: hdb, Now: func() time.Time { return now }, Adapters: map[string]TargetAdapter{trusted: spy, "user": spy}, Recovery: time.Hour, TrustedReleaseTypes: []string{trusted}}
	bg := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: trusted, ID: "c1"}
	other := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}

	begin, err := full.BeginUpload(ctx, "u1", BeginUploadRequest{RequestID: "r-1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = full.FinalizeUpload(ctx, "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	for i, target := range []models4media.Target{bg, other} {
		if _, err = full.Link(ctx, "u1", LinkRequest{RequestID: string(rune('a' + i)), MediaID: begin.MediaID, Role: "bg", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	state := func(target models4media.Target) (int, *dbo4media.MediaLink) {
		asset := new(dbo4media.MediaAsset)
		link := new(dbo4media.MediaLink)
		if err := hdb.Get(ctx, dal4media.NewMediaRecord(begin.MediaID, asset)); err != nil {
			t.Fatal(err)
		}
		if err := hdb.Get(ctx, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(target, "bg"), link)); err != nil {
			t.Fatal(err)
		}
		return asset.RefCount, link
	}
	if ref, _ := state(bg); ref != 2 {
		t.Fatalf("refcount=%d, want 2", ref)
	}

	// validation and allow-list
	if _, err = service.ReleaseLink(ctx, "", bg, "bg"); err == nil {
		t.Fatal("expected mediaID error")
	}
	if _, err = service.ReleaseLink(ctx, begin.MediaID, models4media.Target{}, "bg"); err == nil {
		t.Fatal("expected target error")
	}
	if _, err = service.ReleaseLink(ctx, begin.MediaID, other, "bg"); err == nil {
		t.Fatal("expected untrusted target type to be refused")
	}
	if ref, link := state(other); ref != 2 || link.Status != models4media.LinkStatusActive {
		t.Fatalf("other product's link must stay active: ref=%d link=%+v", ref, link)
	}
	// missing link is success with an empty result
	missing, err := service.ReleaseLink(ctx, begin.MediaID, bg, "other-role")
	if err != nil || missing.Status != "" {
		t.Fatalf("missing link: %+v %v", missing, err)
	}

	// release one of two links
	result, err := service.ReleaseLink(ctx, begin.MediaID, bg, "bg")
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != models4media.LinkStatusDeleted || result.RefCount != 1 || !result.PurgeAfter.Equal(now) {
		t.Fatalf("unexpected result: %+v", result)
	}
	ref, link := state(bg)
	if ref != 1 || link.Status != models4media.LinkStatusDeleted || !link.PurgeAfter.Equal(now) {
		t.Fatalf("unexpected released state: ref=%d link=%+v", ref, link)
	}
	if ref, link = state(other); ref != 1 || link.Status != models4media.LinkStatusActive {
		t.Fatalf("other link must stay active: ref=%d link=%+v", ref, link)
	}
	// idempotent
	again, err := service.ReleaseLink(ctx, begin.MediaID, bg, "bg")
	if err != nil || again.Status != models4media.LinkStatusDeleted {
		t.Fatalf("second release: %+v %v", again, err)
	}
	if ref, _ = state(bg); ref != 1 {
		t.Fatalf("second release changed refcount to %d", ref)
	}
	if spy.calls != 0 {
		t.Fatalf("target adapter was called %d times", spy.calls)
	}

	// refcount never goes below zero (drifted record); errors propagate
	reset := func(refCount int) {
		hdb.txHook = nil
		if err := hdb.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			asset := new(dbo4media.MediaAsset)
			if err := tx.Get(ctx, dal4media.NewMediaRecord(begin.MediaID, asset)); err != nil {
				return err
			}
			l := new(dbo4media.MediaLink)
			if err := tx.Get(ctx, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(bg, "bg"), l)); err != nil {
				return err
			}
			asset.RefCount = refCount
			l.Status = models4media.LinkStatusActive
			if err := tx.Set(ctx, dal4media.NewMediaRecord(begin.MediaID, asset)); err != nil {
				return err
			}
			return tx.Set(ctx, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(bg, "bg"), l))
		}); err != nil {
			t.Fatal(err)
		}
	}
	dbErr := errors.New("db fail")
	isLink := func(r record.Record) bool { return r.Key().Collection() == dbo4media.LinksCollection }
	for name, hook := range map[string]func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction{
		"get link": func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onGet: func(_ context.Context, r record.Record) error {
				if isLink(r) {
					return dbErr
				}
				return nil
			}}
		},
		"get media": func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onGet: func(_ context.Context, r record.Record) error {
				if !isLink(r) {
					return dbErr
				}
				return nil
			}}
		},
		"set link": func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onSet: func(_ context.Context, r record.Record) error {
				if isLink(r) {
					return dbErr
				}
				return nil
			}}
		},
		"set media": func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
			return hookTx{ReadwriteTransaction: tx, onSet: func(_ context.Context, r record.Record) error {
				if !isLink(r) {
					return dbErr
				}
				return nil
			}}
		},
	} {
		reset(1)
		hdb.txHook = hook
		if _, err = service.ReleaseLink(ctx, begin.MediaID, bg, "bg"); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
	hdb.txHook = nil
	reset(0)
	if _, err = service.ReleaseLink(ctx, begin.MediaID, bg, "bg"); err != nil {
		t.Fatal(err)
	}
	if ref, _ = state(bg); ref != 0 {
		t.Fatalf("refcount=%d, want 0 floor", ref)
	}
}
