package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/armbian/ansi-hastebin/internal/accounts"
	"github.com/armbian/ansi-hastebin/internal/storage"
	"github.com/stretchr/testify/require"
)

// TestDeletedPrivatePastesNeverServed deletes 200 private pastes in every
// way a row goes (owner delete, bulk delete, expiry, a new version that
// deletes the previous one, account delete) while 4 anonymous readers loop
// over their /raw links and a slow drainer deletes the content. Not one
// read may answer 200: before the gate knew about the delete queue, a key
// whose row was gone read as an anonymous public paste until its content
// was deleted.
func TestDeletedPrivatePastesNeverServed(t *testing.T) {
	ctx := context.Background()
	e := newEditTest(t, nil)
	bob, err := e.db.UserByGitHubID(ctx, 2)
	require.NoError(t, err)

	const n = 200
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("secret%04d", i)
		owner := e.aliceID
		if i >= 160 {
			owner = bob.ID
		}
		require.NoError(t, e.store.Set(keys[i], "secret content "+keys[i], true))
		require.NoError(t, e.db.InsertPaste(ctx, accounts.NewPaste{Key: keys[i], UserID: owner,
			Visibility: accounts.VisibilityPrivate, Size: 1}, accounts.Limits{}))
	}

	deleter := e.store.(storage.Deleter)
	drainer := accounts.NewDrainer(e.db, func(key string) error {
		// A slow store, as S3 under load: the window it opened was this long
		time.Sleep(2 * time.Millisecond)
		return deleter.Delete(key)
	})
	drainCtx, stopDrain := context.WithCancel(ctx)
	drained := make(chan struct{})
	go func() {
		drainer.Run(drainCtx)
		close(drained)
	}()
	e.dash.Drainer = drainer

	var served, missing, other atomic.Int64
	var servedKeys sync.Map
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for w := range 4 {
		readers.Add(1)
		go func(w int) {
			defer readers.Done()
			for i := w; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				req := httptest.NewRequest(http.MethodGet, "/raw/"+keys[i%n], nil)
				req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1", w+1)
				rec := httptest.NewRecorder()
				e.mux.ServeHTTP(rec, req)
				switch rec.Code {
				case http.StatusOK:
					served.Add(1)
					servedKeys.Store(keys[i%n], true)
					t.Errorf("anonymous read of deleted private paste %s: %q", keys[i%n], rec.Body.String())
				case http.StatusNotFound:
					missing.Add(1)
				default:
					other.Add(1)
				}
			}
		}(w)
	}

	// Owner deletes, one at a time and in bulk, through the API, within
	// its 30 changes a minute
	for _, key := range keys[:20] {
		rec := e.do(http.MethodDelete, "/api/v1/pastes/"+key, "{}", e.alice, "Content-Type", "application/json")
		require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	}
	for i := 20; i < 80; i += 10 {
		body := `{"keys": ["` + strings.Join(keys[i:i+10], `", "`) + `"]}`
		rec := e.do(http.MethodPost, "/api/v1/pastes/delete", body, e.alice, "Content-Type", "application/json")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	// Expiry, by the sweeper
	_, err = e.db.W.Exec("UPDATE pastes SET expires_at = 1 WHERE key >= ? AND key <= ?", keys[80], keys[119])
	require.NoError(t, err)
	swept, err := accounts.NewSweeper(e.db, drainer).Sweep(ctx)
	require.NoError(t, err)
	require.Equal(t, 40, swept)
	// A new version that deletes the previous one
	for _, key := range keys[120:160] {
		require.NoError(t, e.db.InsertPaste(ctx, accounts.NewPaste{Key: key + "v2", UserID: e.aliceID, PrevKey: key,
			DeletePrevious: true, Size: 1}, accounts.Limits{}))
	}
	drainer.Kick()
	// Account delete
	_, err = e.db.DeleteUser(ctx, bob)
	require.NoError(t, err)
	drainer.Kick()

	// Read on until the drainer has deleted every content
	deadline := time.Now().Add(30 * time.Second)
	for {
		left, err := e.db.QueueLen(ctx)
		require.NoError(t, err)
		if left == 0 {
			break
		}
		require.True(t, time.Now().Before(deadline), "the drainer did not finish")
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(stop)
	readers.Wait()
	stopDrain()
	<-drained

	leaked := 0
	servedKeys.Range(func(any, any) bool { leaked++; return true })
	t.Logf("anonymous reads during deletion: %d served (%d of %d keys), %d missing, %d other",
		served.Load(), leaked, n, missing.Load(), other.Load())
	require.Zero(t, served.Load())
	require.Zero(t, other.Load())
	require.Positive(t, missing.Load())
	for _, key := range keys {
		_, err := e.store.Size(key)
		require.ErrorIs(t, err, storage.ErrNotFound, key)
	}
}
