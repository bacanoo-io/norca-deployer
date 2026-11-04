# namespaces/apps/secrets/api/db.hcl   -> built-in secret store, path api/db
items = {
  user     = "api"
  password = "!generate:api-db-password"      # created once, then kept
}

overwrite = false
