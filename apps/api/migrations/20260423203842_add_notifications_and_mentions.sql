-- Modify "outbox_events" table
ALTER TABLE "outbox_events" ADD COLUMN "notified_at" timestamptz NULL;
-- Create index "outboxevent_notified_at_created_at" to table: "outbox_events"
CREATE INDEX "outboxevent_notified_at_created_at" ON "outbox_events" ("notified_at", "created_at");
-- Create "message_mentions" table
CREATE TABLE "message_mentions" ("id" uuid NOT NULL, "message_id" uuid NOT NULL, "user_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "message_mentions_messages_mentions" FOREIGN KEY ("message_id") REFERENCES "messages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "message_mentions_users_mentioned_in" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "messagemention_message_id_user_id" to table: "message_mentions"
CREATE UNIQUE INDEX "messagemention_message_id_user_id" ON "message_mentions" ("message_id", "user_id");
-- Create index "messagemention_user_id_message_id" to table: "message_mentions"
CREATE INDEX "messagemention_user_id_message_id" ON "message_mentions" ("user_id", "message_id");
-- Create "notifications" table
CREATE TABLE "notifications" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "kind" character varying NOT NULL, "read_at" timestamptz NULL, "channel_id" uuid NULL, "message_id" uuid NULL, "organization_id" uuid NOT NULL, "recipient_user_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "notifications_channels_notifications" FOREIGN KEY ("channel_id") REFERENCES "channels" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "notifications_messages_notifications" FOREIGN KEY ("message_id") REFERENCES "messages" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "notifications_organizations_notifications" FOREIGN KEY ("organization_id") REFERENCES "organizations" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "notifications_users_notifications" FOREIGN KEY ("recipient_user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "notification_organization_id_created_at" to table: "notifications"
CREATE INDEX "notification_organization_id_created_at" ON "notifications" ("organization_id", "created_at");
-- Create index "notification_recipient_user_id_message_id_kind" to table: "notifications"
CREATE UNIQUE INDEX "notification_recipient_user_id_message_id_kind" ON "notifications" ("recipient_user_id", "message_id", "kind");
-- Create index "notification_recipient_user_id_read_at_created_at" to table: "notifications"
CREATE INDEX "notification_recipient_user_id_read_at_created_at" ON "notifications" ("recipient_user_id", "read_at", "created_at");
