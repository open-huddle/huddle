-- Modify "notifications" table
ALTER TABLE "notifications" ADD COLUMN "emailed_at" timestamptz NULL;
-- Create "notification_preferences" table
CREATE TABLE "notification_preferences" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "kind" character varying NOT NULL, "email_enabled" boolean NOT NULL DEFAULT true, "user_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "notification_preferences_users_notification_preferences" FOREIGN KEY ("user_id") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "notificationpreference_user_id_kind" to table: "notification_preferences"
CREATE UNIQUE INDEX "notificationpreference_user_id_kind" ON "notification_preferences" ("user_id", "kind");
