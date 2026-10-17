-- Instance settings: the runtime policy layer described in
-- docs/architecture/configuration.md. A value is typed and validated by a
-- registry entry in the application, so this table stores only the operator's
-- override; an absent key means the registry default applies.
--
-- Rollback limit: additive and read by no released binary before this
-- migration, so the previous release still runs against this schema. Dropping
-- the table is safe only while nothing has been configured.

CREATE TABLE instance_settings (
    key        TEXT PRIMARY KEY,
    value      JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by UUID
);
