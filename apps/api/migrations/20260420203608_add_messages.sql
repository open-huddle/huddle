-- Create "messages" table
CREATE TABLE "messages" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "body" character varying NOT NULL, "channel_id" uuid NOT NULL, "author_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "messages_channels_messages" FOREIGN KEY ("channel_id") REFERENCES "channels" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "messages_users_messages" FOREIGN KEY ("author_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "message_channel_id_created_at" to table: "messages"
CREATE INDEX "message_channel_id_created_at" ON "messages" ("channel_id", "created_at");
