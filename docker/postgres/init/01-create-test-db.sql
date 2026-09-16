-- Executed once by the postgres image when the volume is first created.
-- A separate database is used by the integration tests so the demo data in
-- `wallet` is never mixed with test data.
CREATE DATABASE wallet_test OWNER wallet;
