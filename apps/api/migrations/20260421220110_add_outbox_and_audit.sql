-- Create "outbox_events" table
CREATE TABLE "outbox_events" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "aggregate_type" character varying NOT NULL, "aggregate_id" uuid NOT NULL, "event_type" character varying NOT NULL, "subject" character varying NOT NULL, "payload" bytea NOT NULL, "actor_id" uuid NULL, "organization_id" uuid NULL, "resource_type" character varying NOT NULL, "resource_id" uuid NOT NULL, "published_at" timestamptz NULL, PRIMARY KEY ("id"));
-- Create index "outboxevent_created_at_id" to table: "outbox_events"
CREATE INDEX "outboxevent_created_at_id" ON "outbox_events" ("created_at", "id");
-- Create index "outboxevent_published_at_created_at" to table: "outbox_events"
CREATE INDEX "outboxevent_published_at_created_at" ON "outbox_events" ("published_at", "created_at");
-- Create "audit_events" table
CREATE TABLE "audit_events" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "event_type" character varying NOT NULL, "actor_id" uuid NULL, "organization_id" uuid NULL, "resource_type" character varying NOT NULL, "resource_id" uuid NOT NULL, "payload" bytea NOT NULL, "outbox_event_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "audit_events_outbox_events_audit_event" FOREIGN KEY ("outbox_event_id") REFERENCES "outbox_events" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "audit_events_outbox_event_id_key" to table: "audit_events"
CREATE UNIQUE INDEX "audit_events_outbox_event_id_key" ON "audit_events" ("outbox_event_id");
-- Create index "auditevent_actor_id_created_at" to table: "audit_events"
CREATE INDEX "auditevent_actor_id_created_at" ON "audit_events" ("actor_id", "created_at");
-- Create index "auditevent_organization_id_created_at" to table: "audit_events"
CREATE INDEX "auditevent_organization_id_created_at" ON "audit_events" ("organization_id", "created_at");
