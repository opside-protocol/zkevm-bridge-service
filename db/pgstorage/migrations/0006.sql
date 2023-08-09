-- +migrate Down
ALTER TABLE sync.monitored_txs ADD COLUMN is_l1 BOOLEAN DEFAULT FALSE;
alter table sync.monitored_txs drop constraint monitored_txs_pkey;
alter table sync.monitored_txs add primary key (id, is_l1);