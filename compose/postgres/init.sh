#!/usr/bin/env bash
# Local Docker Compose only. One database per service, with two roles each: an
# owner that runs migrations and an application role that can only read and
# write data (D57). The passwords repeat the role names because this database
# is reachable only inside the Compose network; the clusters take credentials
# from Vault.
set -euo pipefail

for db in psp payment; do
  psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname postgres <<SQL
CREATE ROLE ${db}_owner LOGIN PASSWORD '${db}_owner';
CREATE ROLE ${db}_app LOGIN PASSWORD '${db}_app';
CREATE DATABASE ${db} OWNER ${db}_owner;
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
