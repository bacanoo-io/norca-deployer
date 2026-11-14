# ACL policy of a service that reads the event stream of region eu-central
# from the system bus of the servers (NATS embedded in Norca): a durable
# JetStream consumer of the stream NORCA_EVENTS_eu-central. Give its token to
# the service; it connects to the bus port (4649) of any server. The replies
# of its requests come only under its own inbox prefix: the service sets it
# to _INBOX.<accessor ID of the token> (nats.go CustomInboxPrefix,
# nats --inbox-prefix), or its JetStream requests time out.
bus {
  subscribe = ["norca.eu-central.>"]
  publish = [
    "$JS.API.INFO",
    "$JS.API.STREAM.INFO.NORCA_EVENTS_eu-central",
    "$JS.API.CONSUMER.*.NORCA_EVENTS_eu-central",
    "$JS.API.CONSUMER.*.NORCA_EVENTS_eu-central.>",
    "$JS.API.CONSUMER.MSG.NEXT.NORCA_EVENTS_eu-central.>",
    "$JS.ACK.NORCA_EVENTS_eu-central.>",
  ]
}
