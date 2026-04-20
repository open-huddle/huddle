variable "db_url" {
  type    = string
  default = "postgres://huddle:huddle@localhost:5432/huddle?sslmode=disable"
}

env "local" {
  url = var.db_url
  migration {
    dir = "file://migrations"
  }
}
