-- Modify "outbox_events" table
ALTER TABLE "outbox_events" ADD COLUMN "indexed_at" timestamptz NULL;
-- Create index "outboxevent_indexed_at_created_at" to table: "outbox_events"
CREATE INDEX "outboxevent_indexed_at_created_at" ON "outbox_events" ("indexed_at", "created_at");
