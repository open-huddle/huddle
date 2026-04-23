-- Modify "messages" table
ALTER TABLE "messages" ADD COLUMN "edited_at" timestamptz NULL, ADD COLUMN "deleted_at" timestamptz NULL;
-- Modify "notifications" table
ALTER TABLE "notifications" ADD COLUMN "source" character varying NOT NULL DEFAULT 'message_created';
