-- One-time privileged provisioning, never executed by the running bridge.
-- psql -X -v ON_ERROR_STOP=1 -v bridge_role=modelguard_bridge -v db_name=sub2api
-- Read the password from the process environment, never from command arguments.
\set ECHO none
\getenv bridge_password MODEL_GUARD_DB_PASSWORD
\if :{?bridge_password}
\else
  \echo 'MODEL_GUARD_DB_PASSWORD must be set for provisioning'
  \quit 1
\endif

BEGIN;
SELECT format('CREATE ROLE %I', :'bridge_role')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'bridge_role')
\gexec
ALTER ROLE :"bridge_role" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB
  NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 16
  PASSWORD :'bridge_password';
ALTER ROLE :"bridge_role" SET default_transaction_read_only = on;
ALTER ROLE :"bridge_role" SET statement_timeout = '8s';
ALTER ROLE :"bridge_role" SET search_path = public;

CREATE OR REPLACE VIEW public.modelguard_bridge_accounts AS
SELECT a.id, a.name, a.platform, a.type, a.status, a.schedulable,
       a.updated_at, a.expires_at, a.parent_account_id,
       a.credentials->>'api_key' AS api_key,
       a.credentials->>'base_url' AS base_url,
       a.credentials->'model_mapping' AS model_mapping,
       CASE WHEN p.id IS NULL THEN NULL ELSE json_build_object(
         'protocol', p.protocol, 'host', p.host, 'port', p.port,
         'username', p.username, 'password', p.password) END AS proxy
FROM public.accounts a
LEFT JOIN public.proxies p ON p.id = a.proxy_id
WHERE a.deleted_at IS NULL AND a.platform = 'openai' AND a.type = 'apikey'
  AND a.parent_account_id IS NULL;

REVOKE ALL ON public.accounts, public.proxies FROM :"bridge_role";
REVOKE ALL ON public.modelguard_bridge_accounts FROM PUBLIC;
REVOKE ALL ON public.modelguard_bridge_accounts FROM :"bridge_role";
GRANT CONNECT ON DATABASE :"db_name" TO :"bridge_role";
GRANT USAGE ON SCHEMA public TO :"bridge_role";
GRANT SELECT ON public.modelguard_bridge_accounts TO :"bridge_role";
COMMIT;
