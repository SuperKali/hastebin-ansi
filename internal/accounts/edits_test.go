package accounts

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRecordEdit(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	addPaste(t, db, NewPaste{Key: "k", UserID: alice.ID, Size: 10})
	until := time.Now().Add(time.Minute)
	edit := func(rev int, size int64) Edit {
		return Edit{Key: "k", UserID: alice.ID, Rev: rev, StoreKey: RevisionKey("k", rev), OldSize: 10, NewSize: size,
			Until: until, MaxRevisions: 2}
	}

	p, err := db.RecordEdit(ctx, edit(1, 7), Limits{})
	require.NoError(t, err)
	require.EqualValues(t, 7, p.Size)
	require.Equal(t, 1, p.EditCount)
	require.False(t, p.EditedAt.IsZero())
	count, next, err := db.NextRevision(ctx, "k")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, 2, next)
	storeKey, err := db.RevisionStoreKey(ctx, alice.ID, "k", 1)
	require.NoError(t, err)
	require.Equal(t, "k.r1", storeKey)
	_, err = db.RevisionStoreKey(ctx, bob.ID, "k", 1)
	require.ErrorIs(t, err, ErrNotFound, "someone else's revision")
	_, err = db.RevisionStoreKey(ctx, alice.ID, "k", 2)
	require.ErrorIs(t, err, ErrNotFound)

	// The old content counts as a revision against the storage limit
	_, err = db.RecordEdit(ctx, edit(2, 7), Limits{MaxBytes: 20})
	require.ErrorIs(t, err, ErrStorageLimit)

	_, err = db.RecordEdit(ctx, edit(2, 7), Limits{})
	require.NoError(t, err)
	_, err = db.RecordEdit(ctx, edit(3, 7), Limits{})
	require.ErrorIs(t, err, ErrTooManyRevisions)

	other := edit(3, 7)
	other.UserID = bob.ID
	_, err = db.RecordEdit(ctx, other, Limits{})
	require.ErrorIs(t, err, ErrNotFound)

	late := edit(3, 7)
	late.MaxRevisions, late.Until = 0, time.Now().Add(-time.Second)
	_, err = db.RecordEdit(ctx, late, Limits{})
	require.ErrorIs(t, err, ErrEditExpired)

	_, err = db.MarkSeen(ctx, "k", time.Now())
	require.NoError(t, err)
	open := edit(3, 7)
	open.MaxRevisions = 0
	_, err = db.RecordEdit(ctx, open, Limits{})
	require.ErrorIs(t, err, ErrEditSeen)

	seen, edited, err := db.EditMarks(ctx, "k")
	require.NoError(t, err)
	require.False(t, seen.IsZero())
	require.False(t, edited.IsZero())
	_, _, err = db.EditMarks(ctx, "none")
	require.ErrorIs(t, err, ErrNotFound)

	// A revoked session refuses the edit
	addPaste(t, db, NewPaste{Key: "s", UserID: alice.ID, Size: 1})
	gone := Edit{Key: "s", UserID: alice.ID, Rev: 1, StoreKey: "s.r1", Until: until, SessionHash: []byte("nope")}
	_, err = db.RecordEdit(ctx, gone, Limits{})
	require.ErrorIs(t, err, ErrRevoked)

	// Deleting the paste queues its revisions
	_, _, err = db.DeletePastes(ctx, alice.ID, []string{"k"})
	require.NoError(t, err)
	require.Equal(t, []string{"k", "k.r1", "k.r2"}, queued(t, db))
}

func TestInsertVersion(t *testing.T) {
	ctx := context.Background()
	db := openTest(t)
	alice := newUser(t, db, 1, "alice")
	bob := newUser(t, db, 2, "bob")
	addPaste(t, db, NewPaste{Key: "v1", UserID: alice.ID, Title: "log", Size: 1})
	addPaste(t, db, NewPaste{Key: "v2", UserID: alice.ID, Size: 1, PrevKey: "v1"})

	p, err := db.PasteByKey(ctx, alice.ID, "v2")
	require.NoError(t, err)
	require.Equal(t, "v1", p.RootKey)
	require.Equal(t, "v1", p.PrevKey)
	require.Equal(t, 2, p.Version)
	first, err := db.PasteByKey(ctx, alice.ID, "v1")
	require.NoError(t, err)
	require.Equal(t, "v2", first.NextKey)

	// Someone else's paste cannot be continued
	err = db.InsertPaste(ctx, NewPaste{Key: "x", UserID: bob.ID, Size: 1, PrevKey: "v2"}, Limits{})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = db.PasteByKey(ctx, bob.ID, "x")
	require.ErrorIs(t, err, ErrNotFound)

	// The next version deletes the previous one in the same step
	require.NoError(t, db.InsertPaste(ctx, NewPaste{Key: "v3", UserID: alice.ID, Size: 1, PrevKey: "v2", DeletePrevious: true}, Limits{}))
	_, err = db.PasteByKey(ctx, alice.ID, "v2")
	require.ErrorIs(t, err, ErrNotFound)
	p, err = db.PasteByKey(ctx, alice.ID, "v3")
	require.NoError(t, err)
	require.Equal(t, 3, p.Version)
	require.Equal(t, "v1", p.RootKey)
	require.Empty(t, p.PrevKey, "the deleted previous version is unlinked")
	require.Equal(t, []string{"v2"}, queued(t, db))
	versions, err := db.Versions(ctx, alice.ID, "v1")
	require.NoError(t, err)
	require.Len(t, versions, 2)
}
