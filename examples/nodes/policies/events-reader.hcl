# ACL policy of a service that reads the changes of region eu-central
# (change data capture): the events of the namespaces on the system bus of
# the servers (NATS embedded in Norca). Give its token to the service; it
# asks the servers for its consumer (norca cdc consumer apply, or
# PUT /v1/cdc/consumer/events) and reads it on the bus port (4649) of any
# server. The replies of its requests come only under its own inbox prefix:
# the service sets it to _INBOX.<accessor ID of the token> (nats.go
# CustomInboxPrefix, nats --inbox-prefix), or its requests time out.
cdc {
  receive = ["norca.eu-central.>"]
}
