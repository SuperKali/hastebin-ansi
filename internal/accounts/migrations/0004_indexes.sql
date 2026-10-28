-- One index for each order of the dashboard list (Sorts in pastes.go), so
-- a page is read in index order and stops at its last row instead of
-- sorting all of a user's pastes. Each ends in created_at and key, the tie
-- breakers of every order; key is listed because a rowid table's index does
-- not hold it.

DROP INDEX pastes_user_created;
CREATE INDEX pastes_user_created ON pastes (user_id, created_at, key);

CREATE INDEX pastes_user_size ON pastes (user_id, size, created_at, key);
CREATE INDEX pastes_user_views ON pastes (user_id, views, created_at, key);
CREATE INDEX pastes_user_title ON pastes (user_id, title COLLATE NOCASE, created_at DESC, key DESC);

DROP INDEX pastes_user_opened;
CREATE INDEX pastes_user_opened ON pastes (user_id, last_opened_at IS NULL, last_opened_at DESC, created_at DESC, key DESC);

-- The order the drainer takes the delete queue in
CREATE INDEX delete_queue_order ON delete_queue (attempts, enqueued_at, store_key);
