#!/usr/bin/env bash
# Local Docker Compose only. One database per service, with two roles each: an
# owner that runs migrations and an application role that can only read and
# write data (D57). Idempotent: it runs on every start, provisions what a newer
# revision adds to an existing volume, and sets every role's password from its
# secret file, which also applies a rotation.
#
# It connects as the superuser over the server's Unix socket, which the image
# trusts, and removes the superuser's password, so the superuser can log in
# through that socket only, as CloudNativePG does on the clusters. No password
# enters a command line or the environment, and each session keeps them out of
# the server's log.
set -euo pipefail

secrets=/run/secrets
for db in psp payment orders email; do
  for role in owner app; do
    # psql would read an empty or blank file as an empty password and clear
    # the role's instead.
    value=$(<"$secrets/$db-$role") || exit 1
    [[ -n "${value//[[:space:]]/}" ]] || { echo "provision: $secrets/$db-$role is empty" >&2; exit 1; }
  done
done

# Every way PostgreSQL can log a statement's text, for this session only,
# down to the parse-tree dumps.
quiet="SET log_statement = 'none'; SET log_min_duration_statement = -1; SET log_min_duration_sample = -1;
SET log_transaction_sample_rate = 0; SET log_min_error_statement = 'panic'; SET log_min_messages = 'panic';"

psql -X -v ON_ERROR_STOP=1 --dbname postgres -c 'ALTER ROLE postgres PASSWORD NULL'

for db in psp payment orders email; do
  psql -X -v ON_ERROR_STOP=1 --dbname postgres <<SQL
$quiet
\set owner_password \`cat $secrets/${db}-owner\`
\set app_password \`cat $secrets/${db}-app\`
SELECT format('CREATE ROLE %I LOGIN', r)
  FROM unnest(ARRAY['${db}_owner', '${db}_app']) AS r
  WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = r)\gexec
ALTER ROLE ${db}_owner PASSWORD :'owner_password';
ALTER ROLE ${db}_app PASSWORD :'app_password';
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
