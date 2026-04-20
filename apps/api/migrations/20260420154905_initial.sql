-- Create "organizations" table
CREATE TABLE "organizations" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, PRIMARY KEY ("id"));
-- Create index "organizations_slug_key" to table: "organizations"
CREATE UNIQUE INDEX "organizations_slug_key" ON "organizations" ("slug");
-- Create "users" table
CREATE TABLE "users" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "subject" character varying NOT NULL, "email" character varying NOT NULL, "display_name" character varying NULL, PRIMARY KEY ("id"));
-- Create index "user_email" to table: "users"
CREATE INDEX "user_email" ON "users" ("email");
-- Create index "users_subject_key" to table: "users"
CREATE UNIQUE INDEX "users_subject_key" ON "users" ("subject");
-- Create "memberships" table
CREATE TABLE "memberships" ("id" uuid NOT NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "role" character varying NOT NULL DEFAULT 'member', "organization_memberships" uuid NOT NULL, "user_memberships" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "memberships_organizations_memberships" FOREIGN KEY ("organization_memberships") REFERENCES "organizations" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "memberships_users_memberships" FOREIGN KEY ("user_memberships") REFERENCES "users" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "membership_user_memberships_organization_memberships" to table: "memberships"
CREATE UNIQUE INDEX "membership_user_memberships_organization_memberships" ON "memberships" ("user_memberships", "organization_memberships");
