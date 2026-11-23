#!/usr/bin/env bash
# Local Docker Compose only. One database per service, with two roles each: an
# owner that runs migrations and an application role that can only read and
# write data (D57). Idempotent, so it runs on every `docker compose up` and
# also provisions the databases a newer revision adds to an existing volume.
# The passwords repeat the role names because this database is reachable only
# inside the Compose network; the clusters take credentials from Vault.
set -euo pipefail

for db in psp payment orders; do
  psql -X -v ON_ERROR_STOP=1 --dbname postgres <<SQL
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', r, r)
  FROM unnest(ARRAY['${db}_owner', '${db}_app']) AS r
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r)\gexec
SELECT format('CREATE DATABASE %I OWNER %I', '${db}', '${db}_owner')
  WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = '${db}')\gexec
REVOKE ALL ON DATABASE ${db} FROM PUBLIC;
GRANT CONNECT ON DATABASE ${db} TO ${db}_app;
\connect ${db}
ALTER SCHEMA public OWNER TO ${db}_owner;
GRANT USAGE ON SCHEMA public TO ${db}_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ${db}_owner IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO ${db}_app;
ALTER DEFAULT PRIVILEGES FOR ROLE ${db}_owner IN SCHEMA public
  GRANT USAGE, SELECT ON SEQUENCES TO ${db}_app;
SQL
done
