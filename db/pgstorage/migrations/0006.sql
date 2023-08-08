-- +migrate Down
ALTER TABLE sync.monitored_txs ADD COLUMN is_l1 BOOLEAN DEFAULT FALSE;
