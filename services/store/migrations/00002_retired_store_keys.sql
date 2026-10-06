-- +goose Up
-- A retired memory store keeps its rows (retention) but frees its key, so a
-- store re-declared under the same key starts empty instead of silently
-- re-granting retained private data to a new seat identity (§4.1, A18).
ALTER TABLE memory_stores DROP CONSTRAINT memory_stores_organization_id_key_key;
CREATE UNIQUE INDEX memory_stores_active_key ON memory_stores (organization_id, key) WHERE retired_at IS NULL;

-- +goose Down
DROP INDEX memory_stores_active_key;
ALTER TABLE memory_stores ADD CONSTRAINT memory_stores_organization_id_key_key UNIQUE (organization_id, key);
