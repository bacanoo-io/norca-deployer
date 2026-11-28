# The secret api/db of the namespace apps. No value is written here in plain
# text: the deployer resolves the forms and sends the values with the request
# only, as sensitive variables of the stack.
items = {
  user     = "api"
  password = "!env:APP_DB_PASSWORD" # an environment variable that values.env allows
  token    = "!generate:api-db-token" # made once by the servers and kept
}
