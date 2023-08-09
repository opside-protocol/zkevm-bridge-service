-- +migrate Up
ALTER TABLE sync.monitored_txs ADD COLUMN is_l1 BOOLEAN DEFAULT FALSE;
alter table sync.monitored_txs drop constraint monitored_txs_pkey;
alter table sync.monitored_txs add constraint monitored_txs_pkey primary key (id, is_l1);