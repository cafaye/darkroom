-- ci/init.sql — runs once when the postgres service container starts (fresh data dir).
--
-- The service container creates POSTGRES_USER (`darkroom`) as a SUPERUSER, and
-- superusers bypass FORCE ROW LEVEL SECURITY — which is the property the
-- tenancy tier exists to prove. Demote it here, while it is still a superuser
-- and can alter itself, so the owner role is subject to the same policies the
-- login role is.
DO $$ BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'darkroom' AND rolsuper) THEN
    ALTER ROLE darkroom NOSUPERUSER NOBYPASSRLS;
  END IF;
END $$;

-- The login role the RLS policies name. Created here rather than in a step so
-- the role exists before any migration runs.
DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'darkroom_app') THEN
    CREATE ROLE darkroom_app LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION
      PASSWORD 'darkroom';
  END IF;
END $$;

GRANT darkroom_app TO darkroom;
