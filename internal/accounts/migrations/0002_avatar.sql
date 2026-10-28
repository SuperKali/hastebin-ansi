-- The GitHub avatar of each user, refreshed at every login. NULL when
-- GitHub sent none or one outside avatars.githubusercontent.com.

ALTER TABLE users ADD COLUMN avatar_url TEXT;
