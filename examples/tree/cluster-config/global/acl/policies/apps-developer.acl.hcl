# The developers of the namespace apps: read and submit jobs, read and write
# its Variables.
policy "apps-developer" {
  name        = "apps-developer"
  description = "Developers of the namespace apps"

  rules {
    namespace "apps" {
      capabilities = ["read-job", "submit-job", "dispatch-job", "read-logs"]

      variables {
        path "*" {
          capabilities = ["read", "write", "list"]
        }
      }
    }
  }
}
