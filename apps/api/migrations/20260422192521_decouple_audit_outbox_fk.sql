-- Modify "audit_events" table
ALTER TABLE "audit_events" DROP CONSTRAINT "audit_events_outbox_events_audit_event", ALTER COLUMN "outbox_event_id" DROP NOT NULL, ADD CONSTRAINT "audit_events_outbox_events_audit_event" FOREIGN KEY ("outbox_event_id") REFERENCES "outbox_events" ("id") ON UPDATE NO ACTION ON DELETE SET NULL;
