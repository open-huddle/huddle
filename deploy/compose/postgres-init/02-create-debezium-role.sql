-- Debezium replication user. Used only when the `debezium` compose profile
-- is active; harmless to create unconditionally because the role just sits
-- idle when nothing connects as it.
--
-- REPLICATION is the privilege Debezium's pgoutput plugin needs to attach a
-- logical replication slot. We deliberately do NOT grant SUPERUSER — Debezium
-- works with the narrower set, and SUPERUSER on a role that's exposed over
-- the wire is a footgun the project doesn't want anywhere near production
-- patterns.
--
-- Runs after 01-create-keycloak-db.sql (alphabetical order).

CREATE ROLE debezium WITH LOGIN REPLICATION PASSWORD 'debezium';

-- Debezium needs SELECT on the tables it captures. outbox_events is
-- INSERT-only from the app's perspective, but the connector inspects
-- pg_catalog at startup and ent's Schema.Create runs after this script,
-- so the default-privileges grant is what actually applies the SELECT
-- to outbox_events when the table is later created.
GRANT CONNECT ON DATABASE huddle TO debezium;
GRANT USAGE ON SCHEMA public TO debezium;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO debezium;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO debezium;
