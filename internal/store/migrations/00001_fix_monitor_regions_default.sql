-- +goose Up
-- The monitors.regions column default was the legacy 'home' region, which no worker
-- consumes after the region rename (internal/region). Point it at the current default
-- region. Rows are set explicitly by the app on insert, so this only corrects the
-- column default for any future raw insert that omits regions.
ALTER TABLE monitors ALTER COLUMN regions SET DEFAULT '{eu-central}';

-- +goose Down
ALTER TABLE monitors ALTER COLUMN regions SET DEFAULT '{home}';
