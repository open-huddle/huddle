-- Bootstraps the Keycloak database on first Postgres boot. Runs exactly once
-- per data volume (Postgres's /docker-entrypoint-initdb.d/ semantics), so
-- `make dev-down -v` is required to re-seed.
--
-- The `huddle` database itself is created by POSTGRES_DB in the service env —
-- we only add what POSTGRES_DB cannot express: a second database and its
-- dedicated owner role, so Huddle and Keycloak never share credentials.

CREATE ROLE keycloak WITH LOGIN PASSWORD 'keycloak';
CREATE DATABASE keycloak OWNER keycloak;
GRANT ALL PRIVILEGES ON DATABASE keycloak TO keycloak;
