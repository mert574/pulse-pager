-- +goose Up
-- RFC-019: the notifier mints the invite token at send time and writes its hash to
-- the row, so an invitation now exists briefly with no token between the api's INSERT
-- and the notifier's update. Allow token_hash to be NULL for that window. The
-- uniq_invite_token unique index stays: Postgres treats NULLs as distinct, so many
-- token-less pending rows are fine, and once the notifier sets the hash it is unique.
ALTER TABLE invitations ALTER COLUMN token_hash DROP NOT NULL;

-- +goose Down
-- Reverse: requires every row to have a token_hash (fails if any token-less invite is
-- still pending, which is the point of the forward change). Dev-only rollback.
ALTER TABLE invitations ALTER COLUMN token_hash SET NOT NULL;
