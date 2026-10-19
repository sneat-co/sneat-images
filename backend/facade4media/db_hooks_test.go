// Copyright 2026 Sneat.app

package facade4media

import (
	"context"
	"errors"
	"strings"
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

type hookTx struct {
	dal.ReadwriteTransaction
	onGet    func(ctx context.Context, record record.Record) error
	onSet    func(ctx context.Context, record record.Record) error
	onDelete func(ctx context.Context, key *record.Key) error
}

func (h hookTx) Get(ctx context.Context, r record.Record) error {
	if h.onGet != nil {
		if err := h.onGet(ctx, r); err != nil {
			return err
		}
	}
	return h.ReadwriteTransaction.Get(ctx, r)
}

func (h hookTx) Set(ctx context.Context, r record.Record) error {
	if h.onSet != nil {
		if err := h.onSet(ctx, r); err != nil {
			return err
		}
	}
	return h.ReadwriteTransaction.Set(ctx, r)
}

func (h hookTx) Delete(ctx context.Context, k *record.Key) error {
	if h.onDelete != nil {
		if err := h.onDelete(ctx, k); err != nil {
			return err
		}
	}
	return h.ReadwriteTransaction.Delete(ctx, k)
}

type hookDB struct {
	dal.DB
	txHook         func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction
	onExecuteQuery func(ctx context.Context, query dal.Query) (dal.RecordsReader, error)
}

func (h hookDB) RunReadwriteTransaction(ctx context.Context, f func(ctx context.Context, tx dal.ReadwriteTransaction) error, options ...dal.TransactionOption) error {
	return h.DB.RunReadwriteTransaction(ctx, func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		if h.txHook != nil {
			tx = h.txHook(tx)
		}
		return f(ctx, tx)
	}, options...)
}

func (h hookDB) ExecuteQueryToRecordsReader(ctx context.Context, q dal.Query) (dal.RecordsReader, error) {
	if h.onExecuteQuery != nil {
		return h.onExecuteQuery(ctx, q)
	}
	return h.DB.ExecuteQueryToRecordsReader(ctx, q)
}

func TestSweepFullCoverage(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	rawDB := sneatcoretesting.NewMemoryDB()
	hdb := &hookDB{DB: rawDB}
	adapter := &fakeTargetAdapter{refs: make(map[string]*models4media.Ref)}

	service := Service{
		DB:          hdb,
		Blob:        blob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		Adapters:    map[string]TargetAdapter{"user": adapter},
		Authorizer:  allowAll{},
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
	}

	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}

	// 1. Asset with active link and RefCount mismatch
	b1, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "s-1", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: b1.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "l-1", MediaID: b1.MediaID, Role: "avatar", Target: target}); err != nil {
		t.Fatal(err)
	}
	// Introduce ref count mismatch
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord(b1.MediaID, &dbo4media.MediaAsset{
		Status: models4media.AssetStatusReady, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/" + b1.MediaID, Generation: 1},
		ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha,
		CreatedAt: now, CreatedBy: "u1", FinalizedAt: now, RefCount: 99, DurableRefCount: 99,
	}))

	// 2. Query media assets fails
	qErr := errors.New("query media fail")
	hdb.onExecuteQuery = func(ctx context.Context, q dal.Query) (dal.RecordsReader, error) {
		return nil, qErr
	}
	if _, err := service.Sweep(context.Background(), 10); !errors.Is(err, qErr) {
		t.Fatalf("expected query media fail, got: %v", err)
	}

	// 3. Query links fails
	linksQErr := errors.New("query links fail")
	hdb.onExecuteQuery = func(ctx context.Context, q dal.Query) (dal.RecordsReader, error) {
		if strings.Contains(q.String(), "links") {
			return nil, linksQErr
		}
		return rawDB.ExecuteQueryToRecordsReader(ctx, q)
	}
	if _, err := service.Sweep(context.Background(), 10); !errors.Is(err, linksQErr) {
		t.Fatalf("expected query links fail, got: %v", err)
	}
	hdb.onExecuteQuery = nil

	// 3b. Sweep skips empty mediaID or nil data
	hdb.onExecuteQuery = func(ctx context.Context, q dal.Query) (dal.RecordsReader, error) {
		if !strings.Contains(q.String(), "links") {
			return dal.NewRecordsReader([]record.Record{
				record.NewRecordWithData(record.NewKeyWithID(dbo4media.MediaCollection, ""), nil),
			}), nil
		}
		return rawDB.ExecuteQueryToRecordsReader(ctx, q)
	}
	if _, err := service.Sweep(context.Background(), 10); err != nil {
		t.Fatalf("sweep with invalid asset record failed: %v", err)
	}
	hdb.onExecuteQuery = nil

	// 4. Sweep runs successfully, encounters active link, detects refcount mismatch
	res, err := service.Sweep(context.Background(), 10)
	if err != nil {
		t.Fatalf("sweep failed: %v", err)
	}
	if res.RefCountMismatches != 1 {
		t.Fatalf("expected 1 refcount mismatch, got %d", res.RefCountMismatches)
	}

	// 5. PurgeExpiredLink error inside sweep
	// Unlink to make link deleted and expired
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err != nil {
		t.Fatal(err)
	}
	service.Now = func() time.Time { return now.Add(2 * time.Hour) }
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onDelete: func(ctx context.Context, key *record.Key) error {
			return errors.New("delete link fail")
		}}
	}
	if _, err := service.Sweep(context.Background(), 10); err == nil {
		t.Fatal("expected error from sweep when link purge fails")
	}
	hdb.txHook = nil
}

func TestTxErrorBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	blob := &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}
	rawDB := sneatcoretesting.NewMemoryDB()
	hdb := &hookDB{DB: rawDB}
	adapter := &fakeTargetAdapter{refs: make(map[string]*models4media.Ref)}

	service := Service{
		DB:          hdb,
		Blob:        blob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		Adapters:    map[string]TargetAdapter{"user": adapter},
		Authorizer:  allowAll{},
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
	}

	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}

	// 1. BeginUpload: unexpected error from tx.Get (not ErrNotFound)
	dbErr := errors.New("unexpected db error")
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "req-err", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic}); err == nil {
		t.Fatal("expected error on tx.Get failure in BeginUpload")
	}
	hdb.txHook = nil

	// 2. FinalizeUpload: asset.Validate() fails due to bad image dimensions
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "req-fin-val", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	blob.info.Width = 0 // invalid ready width!
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err == nil {
		t.Fatal("expected error when asset.Validate fails in FinalizeUpload")
	}
	blob.info.Width = 10

	// 3. FinalizeUpload: tx.Get error inside transaction
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err == nil {
		t.Fatal("expected tx.Get error in FinalizeUpload tx")
	}
	hdb.txHook = nil

	// Finalize successfully
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	// 4. Link: tx.Get error (not ErrNotFound) for linkRecord
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if strings.Contains(r.Key().String(), "links") {
				return dbErr
			}
			return nil
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r1", MediaID: begin.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected link tx.Get error")
	}
	hdb.txHook = nil

	// 5. Link: tx.Set error
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r1", MediaID: begin.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected link tx.Set error")
	}
	hdb.txHook = nil

	// Link successfully
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r1", MediaID: begin.MediaID, Role: "avatar", Target: target}); err != nil {
		t.Fatal(err)
	}

	// 6. Unlink: adapter.Set error
	errAdapt := &errTargetAdapter{ref: &models4media.Ref{MediaID: begin.MediaID}, setErr: errors.New("adapter set fail")}
	service.Adapters["user"] = errAdapt
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err == nil {
		t.Fatal("expected adapter set error in unlink")
	}
	service.Adapters["user"] = adapter

	// 7. Unlink: tx.Get error on media
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err == nil {
		t.Fatal("expected tx.Get error in unlink")
	}
	hdb.txHook = nil

	// 7b. Unlink: tx.Get error on link
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if r.Key().Collection() == dbo4media.LinksCollection {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err == nil {
		t.Fatal("expected tx.Get link error in unlink")
	}
	hdb.txHook = nil

	// 8. Unlink: tx.Set error
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err == nil {
		t.Fatal("expected tx.Set error in unlink")
	}
	hdb.txHook = nil

	// Unlink successfully
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err != nil {
		t.Fatal(err)
	}

	// 9. Restore: tx.Set error
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected tx.Set error in restore")
	}
	hdb.txHook = nil
}

func TestPurgeRollbackStatusNotPurging(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	var onDeleteCallback func()
	blob := &inspectCallbackBlob{
		fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}},
	}
	blob.onDelete = func() {
		if onDeleteCallback != nil {
			onDeleteCallback()
		}
	}
	errBlob := &errBlobStore{
		fakeBlob:  blob.fakeBlob,
		deleteErr: errors.New("delete fail"),
		onDelete: func() {
			if onDeleteCallback != nil {
				onDeleteCallback()
			}
		},
	}
	service := Service{
		DB:          sneatcoretesting.NewMemoryDB(),
		Blob:        errBlob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
		Recovery:    time.Hour,
	}

	// 1. PurgeOrphan: blob delete fails, and in rollback current.Status != AssetStatusPurging
	orphanAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusOrphaned,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_orb", Generation: 1},
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
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_orb", orphanAsset))

	onDeleteCallback = func() {
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			orphanAsset.Status = models4media.AssetStatusDeleted
			return tx.Set(ctx, dal4media.NewMediaRecord("m_orb", orphanAsset))
		})
	}
	// PurgeOrphan should trigger blob.Delete error, then in rollback find status is deleted != Purging, and return error
	if err := service.PurgeOrphan(context.Background(), "m_orb"); err == nil {
		t.Fatal("expected error on purge orphan blob delete failure")
	}

	// 2. PurgeStaleUpload: blob delete fails, and in rollback current.Status != AssetStatusPurging
	staleAsset := &dbo4media.MediaAsset{
		Status:          models4media.AssetStatusUploading,
		Access:          models4media.AccessPublic,
		Storage:         dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_stb", Generation: 1},
		ContentType:     "image/png",
		Size:            10,
		CreatedAt:       now,
		CreatedBy:       "u1",
		UploadExpiresAt: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), service.DB, dal4media.NewMediaRecord("m_stb", staleAsset))

	onDeleteCallback = func() {
		_ = service.DB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
			staleAsset.Status = models4media.AssetStatusFailed
			return tx.Set(ctx, dal4media.NewMediaRecord("m_stb", staleAsset))
		})
	}
	if err := service.PurgeStaleUpload(context.Background(), "m_stb"); err == nil {
		t.Fatal("expected error on purge stale upload blob delete failure")
	}
}

func TestTxGetErrorsInPurgeAndLink(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	rawDB := sneatcoretesting.NewMemoryDB()
	hdb := &hookDB{DB: rawDB}
	dbErr := errors.New("db fail")

	service := Service{
		DB:          hdb,
		Blob:        &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}},
		Bucket:      "b",
		Now:         func() time.Time { return now },
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
		Recovery:    time.Hour,
	}

	// 1. PurgeOrphan: tx 1 Get fails
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if err := service.PurgeOrphan(context.Background(), "m1"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr, got: %v", err)
	}
	hdb.txHook = nil

	// 2. PurgeStaleUpload: tx 1 Get fails
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			return dbErr
		}}
	}
	if err := service.PurgeStaleUpload(context.Background(), "m1"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr, got: %v", err)
	}
	hdb.txHook = nil

	// 3. PurgeExpiredLink: tx.Get on linkRecord returns dbErr (non-notfound)
	asset := &dbo4media.MediaAsset{
		Status: models4media.AssetStatusReady, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_pel", Generation: 1},
		ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha,
		CreatedAt: now, CreatedBy: "u1", FinalizedAt: now, RefCount: 1, DurableRefCount: 1,
	}
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_pel", asset))
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if strings.Contains(r.Key().String(), "links") {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if err := service.PurgeExpiredLink(context.Background(), "m_pel", "link1"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr from PurgeExpiredLink, got: %v", err)
	}
	hdb.txHook = nil

	// 4. PurgeOrphan: rollback tx Get error and commit tx Get error
	orphanAsset := &dbo4media.MediaAsset{
		Status: models4media.AssetStatusOrphaned, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_orb2", Generation: 1},
		ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha,
		CreatedAt: now, CreatedBy: "u1", FinalizedAt: now, OrphanedAt: now.Add(-2 * time.Hour), PurgeAfter: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_orb2", orphanAsset))

	delBlob := &errBlobStore{fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}, deleteErr: errors.New("del fail")}
	service.Blob = delBlob
	// Rollback Get fails
	txCount := 0
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		txCount++
		currentTx := txCount
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if currentTx == 2 {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	_ = service.PurgeOrphan(context.Background(), "m_orb2")
	hdb.txHook = nil

	// Commit Get fails in PurgeOrphan
	delBlob.deleteErr = nil
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_orb2", orphanAsset))
	txCount = 0
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		txCount++
		currentTx := txCount
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if currentTx == 2 {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if err := service.PurgeOrphan(context.Background(), "m_orb2"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr in PurgeOrphan commit Get, got: %v", err)
	}
	hdb.txHook = nil

	// 5. PurgeStaleUpload: rollback tx Get error and commit tx Get error
	staleAsset := &dbo4media.MediaAsset{
		Status: models4media.AssetStatusUploading, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_st2", Generation: 1},
		ContentType: "image/png", Size: 10, CreatedAt: now, CreatedBy: "u1", UploadExpiresAt: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_st2", staleAsset))
	delBlob.deleteErr = errors.New("del fail")
	txCount = 0
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		txCount++
		currentTx := txCount
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if currentTx == 2 {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	_ = service.PurgeStaleUpload(context.Background(), "m_st2")
	hdb.txHook = nil

	delBlob.deleteErr = nil
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_st2", staleAsset))
	txCount = 0
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		txCount++
		currentTx := txCount
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if currentTx == 2 {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if err := service.PurgeStaleUpload(context.Background(), "m_st2"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr in PurgeStaleUpload commit Get, got: %v", err)
	}
	hdb.txHook = nil
}

func TestSweepPurgeOrphanAndStaleErrors(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	rawDB := sneatcoretesting.NewMemoryDB()
	hdb := &hookDB{DB: rawDB}
	delBlob := &errBlobStore{fakeBlob: fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}}, deleteErr: errors.New("del fail")}

	service := Service{
		DB:          hdb,
		Blob:        delBlob,
		Bucket:      "b",
		Now:         func() time.Time { return now },
		OrphanGrace: time.Hour,
		UploadGrace: time.Hour,
	}

	// 1. Orphaned asset purge error during sweep
	orphanAsset := &dbo4media.MediaAsset{
		Status: models4media.AssetStatusOrphaned, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_sw_orph", Generation: 1},
		ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha,
		CreatedAt: now, CreatedBy: "u1", FinalizedAt: now, OrphanedAt: now.Add(-2 * time.Hour), PurgeAfter: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_sw_orph", orphanAsset))
	if _, err := service.Sweep(context.Background(), 10); err == nil {
		t.Fatal("expected error from sweep when PurgeOrphan fails")
	}

	// Remove orphaned asset
	_ = rawDB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Delete(ctx, dal4media.NewMediaKey("m_sw_orph"))
	})

	// 2. Stale upload purge error during sweep
	staleAsset := &dbo4media.MediaAsset{
		Status: models4media.AssetStatusUploading, Access: models4media.AccessPublic,
		Storage:     dbo4media.StorageRef{Provider: "gcs", Bucket: "b", ObjectKey: "originals/m_sw_stale", Generation: 1},
		ContentType: "image/png", Size: 10, CreatedAt: now, CreatedBy: "u1", UploadExpiresAt: now.Add(-time.Hour),
	}
	_ = setRecord(context.Background(), rawDB, dal4media.NewMediaRecord("m_sw_stale", staleAsset))
	if _, err := service.Sweep(context.Background(), 10); err == nil {
		t.Fatal("expected error from sweep when PurgeStaleUpload fails")
	}
}

func TestLinkAndRestoreAdditionalErrorBranches(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	sha := HashBytes([]byte("img"))
	rawDB := sneatcoretesting.NewMemoryDB()
	hdb := &hookDB{DB: rawDB}
	adapter := &fakeTargetAdapter{refs: make(map[string]*models4media.Ref)}
	dbErr := errors.New("db fail")

	service := Service{
		DB:          hdb,
		Blob:        &fakeBlob{info: blob4media.ImageInfo{ContentType: "image/png", Size: 10, Width: 10, Height: 10, SHA256: sha, Generation: 1}},
		Bucket:      "b",
		Now:         func() time.Time { return now },
		Adapters:    map[string]TargetAdapter{"user": adapter},
		Authorizer:  allowAll{},
		Recovery:    time.Hour,
		OrphanGrace: time.Hour,
	}

	target := models4media.Target{Scope: models4media.TargetScopeSpace, SpaceID: "s1", Type: "user", ID: "u1"}

	// 1. Link: second tx.Set fails (saving asset record, line 343)
	begin, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "r-set2", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	setCount := 0
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		setCount = 0
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			setCount++
			if setCount == 2 {
				return dbErr
			}
			return tx.Set(ctx, r)
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-set2", MediaID: begin.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected error on second tx.Set in Link")
	}
	hdb.txHook = nil

	// 2. Previous link replacement: previous link tx.Get error (non-notfound, line 322)
	// First link cleanly
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-clean", MediaID: begin.MediaID, Role: "avatar", Target: target}); err != nil {
		t.Fatal(err)
	}
	// Upload second asset
	begin2, err := service.BeginUpload(context.Background(), "u1", BeginUploadRequest{RequestID: "r-clean2", ContentType: "image/png", Size: 10, Access: models4media.AccessPublic})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.FinalizeUpload(context.Background(), "u1", FinalizeUploadRequest{MediaID: begin2.MediaID, SHA256: sha}); err != nil {
		t.Fatal(err)
	}

	// Make tx.Get on previousLinkRecord return non-notfound error (line 322)
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if strings.Contains(r.Key().String(), begin.MediaID+"/links") {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-prev-err", MediaID: begin2.MediaID, Role: "avatar", Target: target}); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr on previous link tx.Get, got: %v", err)
	}
	hdb.txHook = nil

	// 3. Previous link replacement: tx.Set fails on previousLinkRecord (line 351)
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		setCount = 0
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			setCount++
			if setCount == 3 {
				return dbErr
			}
			return tx.Set(ctx, r)
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-prev-set3", MediaID: begin2.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected error on third tx.Set in Link")
	}
	hdb.txHook = nil

	// 4. Previous link replacement: tx.Set fails on previousMediaRecord (line 354)
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		setCount = 0
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			setCount++
			if setCount == 4 {
				return dbErr
			}
			return tx.Set(ctx, r)
		}}
	}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-prev-set4", MediaID: begin2.MediaID, Role: "avatar", Target: target}); err == nil {
		t.Fatal("expected error on fourth tx.Set in Link")
	}
	hdb.txHook = nil

	// 4b. Missing previous link: previous link record deleted from DB (line 320)
	_ = rawDB.RunReadwriteTransaction(context.Background(), func(ctx context.Context, tx dal.ReadwriteTransaction) error {
		return tx.Delete(ctx, dal4media.NewLinkKey(begin.MediaID, dal4media.LinkID(target, "avatar")))
	})
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	if _, err := service.Link(context.Background(), "u1", LinkRequest{RequestID: "r-prev-notfound", MediaID: begin2.MediaID, Role: "avatar", Target: target}); err == nil || !strings.Contains(err.Error(), "media reference points to missing previous link") {
		t.Fatalf("expected missing previous link error, got: %v", err)
	}
	// Recreate link for subsequent tests
	_ = setRecord(context.Background(), rawDB, dal4media.NewLinkRecord(begin.MediaID, dal4media.LinkID(target, "avatar"), &dbo4media.MediaLink{
		Target: target, Role: "avatar", Retention: models4media.LinkRetentionRetained, Status: models4media.LinkStatusActive, CreatedAt: now, CreatedBy: "u1",
	}))

	// 5. Unlink: second tx.Set fails (asset record, line 413)
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		setCount = 0
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			setCount++
			if setCount == 2 {
				return dbErr
			}
			return tx.Set(ctx, r)
		}}
	}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err == nil {
		t.Fatal("expected error on second tx.Set in Unlink")
	}
	hdb.txHook = nil

	// 6. Restore: tx.Get fails on linkRecord (line 449)
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		return hookTx{ReadwriteTransaction: tx, onGet: func(ctx context.Context, r record.Record) error {
			if strings.Contains(r.Key().String(), "links") {
				return dbErr
			}
			return tx.Get(ctx, r)
		}}
	}
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); !errors.Is(err, dbErr) {
		t.Fatalf("expected dbErr on linkRecord tx.Get in Restore, got: %v", err)
	}
	hdb.txHook = nil

	// 7. Restore: second tx.Set fails (asset record, line 474)
	// Unlink cleanly first
	adapter.refs[targetRefKey(target, "avatar")] = &models4media.Ref{MediaID: begin.MediaID}
	if _, err := service.Unlink(context.Background(), "u1", target, "avatar"); err != nil {
		t.Fatal(err)
	}
	hdb.txHook = func(tx dal.ReadwriteTransaction) dal.ReadwriteTransaction {
		setCount = 0
		return hookTx{ReadwriteTransaction: tx, onSet: func(ctx context.Context, r record.Record) error {
			setCount++
			if setCount == 2 {
				return dbErr
			}
			return tx.Set(ctx, r)
		}}
	}
	if _, err := service.Restore(context.Background(), "u1", begin.MediaID, target, "avatar"); err == nil {
		t.Fatal("expected error on second tx.Set in Restore")
	}
	hdb.txHook = nil
}
